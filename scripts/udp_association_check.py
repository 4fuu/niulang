#!/usr/bin/env python3
"""Probe DNS repeatedly through one persistent SOCKS5 UDP association."""

import argparse
import datetime
import ipaddress
import os
import secrets
import socket
import struct
import sys
import time


def parse_endpoint(value):
    if value.startswith("["):
        host, separator, port = value[1:].partition("]:")
    else:
        host, separator, port = value.rpartition(":")
    if not separator or not host:
        raise argparse.ArgumentTypeError("endpoint must be HOST:PORT")
    try:
        number = int(port)
    except ValueError as error:
        raise argparse.ArgumentTypeError("endpoint port must be numeric") from error
    if not 1 <= number <= 65535:
        raise argparse.ArgumentTypeError("endpoint port must be between 1 and 65535")
    return host, number


def receive_exact(connection, length):
    result = bytearray()
    while len(result) < length:
        part = connection.recv(length - len(result))
        if not part:
            raise EOFError("SOCKS control connection closed")
        result.extend(part)
    return bytes(result)


def encode_address(host, port):
    try:
        address = ipaddress.ip_address(host)
    except ValueError:
        encoded = host.encode("idna")
        if not encoded or len(encoded) > 255:
            raise ValueError("destination hostname must contain 1 to 255 encoded bytes")
        return b"\x03" + bytes([len(encoded)]) + encoded + struct.pack("!H", port)
    if address.version == 4:
        return b"\x01" + address.packed + struct.pack("!H", port)
    return b"\x04" + address.packed + struct.pack("!H", port)


def decode_address(data, offset=0):
    if len(data) <= offset:
        raise ValueError("truncated SOCKS address")
    kind = data[offset]
    offset += 1
    if kind == 1:
        end = offset + 4
        if len(data) < end + 2:
            raise ValueError("truncated SOCKS address")
        host = socket.inet_ntop(socket.AF_INET, data[offset:end])
    elif kind == 4:
        end = offset + 16
        if len(data) < end + 2:
            raise ValueError("truncated SOCKS address")
        host = socket.inet_ntop(socket.AF_INET6, data[offset:end])
    elif kind == 3:
        if len(data) <= offset:
            raise ValueError("truncated SOCKS hostname length")
        length = data[offset]
        if length == 0:
            raise ValueError("empty SOCKS hostname")
        offset += 1
        end = offset + length
        if len(data) < end + 2:
            raise ValueError("truncated SOCKS address")
        try:
            host = data[offset:end].decode("idna")
        except UnicodeError as error:
            raise ValueError("invalid SOCKS hostname") from error
    else:
        raise ValueError(f"unsupported SOCKS address type {kind}")
    return host, struct.unpack("!H", data[end : end + 2])[0], end + 2


def encode_dns_name(name):
    labels = name.rstrip(".").split(".")
    if not labels or any(not label for label in labels):
        raise ValueError("DNS name has an empty label")
    encoded = bytearray()
    for label in labels:
        value = label.encode("idna")
        if len(value) > 63:
            raise ValueError("DNS label exceeds 63 encoded bytes")
        encoded.append(len(value))
        encoded.extend(value)
    encoded.append(0)
    if len(encoded) > 255:
        raise ValueError("encoded DNS name exceeds 255 bytes")
    return bytes(encoded)


def decode_dns_name(data, offset):
    """Decode one possibly-compressed DNS name and return raw lowercase labels."""
    labels = []
    resume = None
    visited = set()
    encoded_length = 1
    while True:
        if offset >= len(data):
            raise ValueError("truncated DNS name")
        length = data[offset]
        if length & 0xC0 == 0xC0:
            if offset + 1 >= len(data):
                raise ValueError("truncated DNS compression pointer")
            pointer = ((length & 0x3F) << 8) | data[offset + 1]
            if pointer >= len(data) or pointer in visited:
                raise ValueError("invalid DNS compression pointer")
            visited.add(pointer)
            if resume is None:
                resume = offset + 2
            offset = pointer
            continue
        if length & 0xC0:
            raise ValueError("invalid DNS label encoding")
        offset += 1
        if length == 0:
            return b".".join(labels).lower(), resume if resume is not None else offset
        if length > 63 or offset + length > len(data):
            raise ValueError("truncated DNS label")
        encoded_length += length + 1
        if encoded_length > 255:
            raise ValueError("DNS name exceeds 255 bytes")
        labels.append(data[offset : offset + length])
        offset += length


def canonical_dns_name(name):
    encoded = encode_dns_name(name)
    decoded, consumed = decode_dns_name(encoded, 0)
    if consumed != len(encoded):
        raise ValueError("invalid encoded DNS name")
    return decoded


def validate_dns_response(data, transaction, expected_name=None, expected_type=1, expected_class=1):
    if len(data) < 12:
        raise ValueError("truncated DNS reply")
    reply_transaction, flags, questions, answers, _, _ = struct.unpack("!HHHHHH", data[:12])
    if reply_transaction != transaction:
        raise ValueError("DNS transaction does not match")
    if not flags & 0x8000:
        raise ValueError("DNS reply flag is not set")
    if flags & 0x7800:
        raise ValueError("DNS reply has a non-query opcode")
    if flags & 0x0200:
        raise ValueError("DNS reply is truncated")
    if flags & 0x000F:
        raise ValueError(f"DNS reply has error code {flags & 0x000F}")
    if questions != 1 or answers < 1:
        raise ValueError("DNS reply does not contain one question and an answer")
    question_name, offset = decode_dns_name(data, 12)
    if offset + 4 > len(data):
        raise ValueError("truncated DNS question")
    question_type, question_class = struct.unpack("!HH", data[offset : offset + 4])
    offset += 4
    if expected_name is not None and question_name != canonical_dns_name(expected_name):
        raise ValueError("DNS reply question name does not match")
    if question_type != expected_type or question_class != expected_class:
        raise ValueError("DNS reply question type or class does not match")
    for _ in range(answers):
        _, offset = decode_dns_name(data, offset)
        if offset + 10 > len(data):
            raise ValueError("truncated DNS answer")
        _, _, _, data_length = struct.unpack("!HHIH", data[offset : offset + 10])
        offset += 10
        if offset + data_length > len(data):
            raise ValueError("truncated DNS answer data")
        offset += data_length


def open_association(endpoint, timeout):
    control = socket.create_connection(endpoint, timeout)
    control.settimeout(timeout)
    control.sendall(b"\x05\x01\x00")
    if receive_exact(control, 2) != b"\x05\x00":
        control.close()
        raise RuntimeError("SOCKS server refused no-authentication mode")
    control.sendall(b"\x05\x03\x00\x01\x00\x00\x00\x00\x00\x00")
    header = receive_exact(control, 4)
    if header[:2] != b"\x05\x00" or header[2] != 0:
        control.close()
        raise RuntimeError(f"SOCKS UDP ASSOCIATE failed with reply {header[1]}")
    kind = header[3]
    if kind == 1:
        rest = receive_exact(control, 6)
    elif kind == 4:
        rest = receive_exact(control, 18)
    elif kind == 3:
        length = receive_exact(control, 1)
        rest = length + receive_exact(control, length[0] + 2)
    else:
        control.close()
        raise RuntimeError(f"SOCKS server returned address type {kind}")
    host, port, _ = decode_address(bytes([kind]) + rest)
    if host in ("0.0.0.0", "::"):
        host = endpoint[0]
    udp = None
    try:
        addresses = socket.getaddrinfo(host, port, type=socket.SOCK_DGRAM)
        if not addresses:
            raise RuntimeError("SOCKS server returned an unresolvable UDP relay")
        family, socktype, protocol, _, address = addresses[0]
        udp = socket.socket(family, socktype, protocol)
        udp.connect(address)
    except Exception:
        if udp is not None:
            udp.close()
        control.close()
        raise
    return control, udp, (host, port)


def utc_now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="milliseconds")


def main(arguments=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--socks", type=parse_endpoint, default=("127.0.0.1", 1080))
    parser.add_argument("--destination", type=parse_endpoint, default=("1.1.1.1", 53))
    parser.add_argument("--name", default="cloudflare.com", help="DNS A name to query")
    parser.add_argument("--count", type=int, default=10)
    parser.add_argument("--interval", type=float, default=1.0, help="seconds between completed attempts")
    parser.add_argument("--timeout", type=float, default=3.0, help="seconds per DNS reply")
    parser.add_argument("--min-success", type=int, help="minimum successful replies (default: all)")
    parser.add_argument("--require-final-successes", type=int, default=1)
    parser.add_argument("--output", default="-", help="new TSV file, or - for stdout")
    options = parser.parse_args(arguments)
    if options.count <= 0 or options.interval < 0 or options.timeout <= 0:
        parser.error("count and timeout must be positive and interval must be non-negative")
    minimum = options.count if options.min_success is None else options.min_success
    if not 0 <= minimum <= options.count:
        parser.error("min-success must be between zero and count")
    if not 0 <= options.require_final_successes <= options.count:
        parser.error("require-final-successes must be between zero and count")
    if options.output != "-" and os.path.exists(options.output):
        parser.error(f"output already exists: {options.output}")

    destination = encode_address(*options.destination)
    question = encode_dns_name(options.name) + struct.pack("!HH", 1, 1)
    control, udp, relay = open_association(options.socks, options.timeout)
    try:
        output = sys.stdout if options.output == "-" else open(
            options.output, "x", encoding="utf-8", buffering=1
        )
    except Exception:
        udp.close()
        control.close()
        raise
    successes = 0
    outcomes = []
    try:
        print(f"# socks={options.socks[0]}:{options.socks[1]} relay={relay[0]}:{relay[1]} "
              f"destination={options.destination[0]}:{options.destination[1]} name={options.name}", file=output)
        print("query\tstarted_utc\tstatus\tseconds\tbytes\terror", file=output)
        for query in range(1, options.count + 1):
            transaction = secrets.randbelow(65536)
            dns = struct.pack("!HHHHHH", transaction, 0x0100, 1, 0, 0, 0) + question
            packet = b"\x00\x00\x00" + destination + dns
            started_at = utc_now()
            started = time.monotonic()
            status, size, message = "lost", 0, ""
            try:
                udp.send(packet)
                deadline = started + options.timeout
                while True:
                    udp.settimeout(max(0.001, deadline - time.monotonic()))
                    reply = udp.recv(65535)
                    if len(reply) < 4 or reply[:3] != b"\x00\x00\x00":
                        raise ValueError("invalid SOCKS UDP header")
                    _, _, offset = decode_address(reply, 3)
                    dns_reply = reply[offset:]
                    if len(dns_reply) < 12:
                        raise ValueError("truncated DNS reply")
                    reply_transaction = struct.unpack("!H", dns_reply[:2])[0]
                    if reply_transaction != transaction:
                        if time.monotonic() >= deadline:
                            raise TimeoutError("timed out after stale DNS replies")
                        continue
                    validate_dns_response(dns_reply, transaction, options.name)
                    status, size = "ok", len(dns_reply)
                    successes += 1
                    break
            except (OSError, ValueError) as error:
                message = str(error).replace("\t", " ").replace("\n", " ")
            elapsed = time.monotonic() - started
            outcomes.append(status)
            print(f"{query}\t{started_at}\t{status}\t{elapsed:.3f}\t{size}\t{message}", file=output, flush=True)
            if query != options.count:
                time.sleep(options.interval)
        final = options.require_final_successes
        final_successes = final if final == 0 else sum(result == "ok" for result in outcomes[-final:])
        final_ok = final_successes == final
        print(f"# successes={successes}/{options.count} final_successes={final_successes}/{final}", file=output)
        return 0 if successes >= minimum and final_ok else 1
    finally:
        udp.close()
        control.close()
        if output is not sys.stdout:
            output.close()


if __name__ == "__main__":
    raise SystemExit(main())
