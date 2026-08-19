#!/usr/bin/env python3
"""Build the bundled country route set the iOS tunnel can bypass.

The source is APNIC's `delegated-apnic-latest`, the registry's own record of
which blocks it allocated to which economy. Its header is explicit that the
file says where a resource was *allocated*, not where it is in use, so the
result is an approximation and the app labels it as one.

Registry data is the right source anyway: it has a name, a date, and a
regeneration path, which a scraped aggregate list does not.

Usage:
    scripts/generate_cn_geoip.py --input delegated-apnic-latest \\
        --output mobile/ios/PacketTunnel/Resources/cn-direct.bin
"""

from __future__ import annotations

import argparse
import heapq
import ipaddress
import pathlib
import struct
import sys


MAGIC = b"QQGO"
FORMAT_VERSION = 1
HEADER = struct.Struct(">4sBBHII")


def parse_delegation(text: str, country: str) -> list[ipaddress._BaseNetwork]:
    """Extract one country's networks from a delegated-* registry file.

    IPv4 rows carry a host *count* rather than a prefix length. The count need
    not be a power of two and the start address need not be aligned to it, so a
    row is in general several CIDR blocks: 1.0.1.0 with 768 hosts is a /24 plus
    a /23. ipaddress.summarize_address_range does that decomposition, and it is
    the reason this cannot be a prefix-length lookup.
    """
    networks: list[ipaddress._BaseNetwork] = []
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        fields = line.split("|")
        if len(fields) < 7:
            continue
        _, economy, family, start, value, _, status = fields[:7]
        if economy != country or status not in ("allocated", "assigned"):
            continue
        if family == "ipv4":
            count = int(value)
            if count <= 0:
                continue
            first = ipaddress.IPv4Address(start)
            last = ipaddress.IPv4Address(int(first) + count - 1)
            networks.extend(ipaddress.summarize_address_range(first, last))
        elif family == "ipv6":
            networks.append(ipaddress.IPv6Network(f"{start}/{value}"))
    return networks


def aggregate(
    networks: list[ipaddress._BaseNetwork],
    limit: int,
) -> tuple[list[ipaddress._BaseNetwork], int]:
    """Merge the cheapest neighbouring blocks until at most `limit` remain.

    The registry set is finer than a route table wants to be, so blocks have to
    be joined, and joining two blocks pulls in whatever sits between them. That
    is not free: an over-included address is one this country set claims is
    direct when the registry says it is not, and traffic to it leaves the
    tunnel. So the merge is greedy on exactly that cost — always the pair that
    swallows the fewest addresses — and the total is returned for the caller to
    publish rather than absorb.

    Returns the aggregated networks and the number of addresses over-included.
    """
    if len(networks) <= limit:
        return sorted(networks), 0

    entries = sorted(networks)
    alive = [True] * len(entries)
    # Doubly linked list over the sorted entries, so a merge can find the new
    # neighbours of the block it produced without rescanning.
    previous = list(range(-1, len(entries) - 1))
    following = list(range(1, len(entries) + 1))
    following[-1] = -1
    covered = [entry.num_addresses for entry in entries]
    remaining = len(entries)
    wasted = 0

    def supernet_of(left: int, right: int) -> ipaddress._BaseNetwork:
        candidate = entries[left]
        while not candidate.supernet_of(entries[right]):
            candidate = candidate.supernet()
        return candidate

    def cost(left: int, right: int) -> int:
        joined = supernet_of(left, right)
        return joined.num_addresses - covered[left] - covered[right]

    heap = []
    for index in range(len(entries) - 1):
        heapq.heappush(heap, (cost(index, index + 1), index, index + 1))

    while remaining > limit and heap:
        price, left, right = heapq.heappop(heap)
        if not alive[left] or not alive[right] or following[left] != right:
            continue  # Superseded by an earlier merge; the entry is stale.
        joined = supernet_of(left, right)
        # The supernet may reach past its two parents. Absorbing those costs
        # nothing extra and is what makes the greedy pass converge quickly.
        absorbed = covered[left] + covered[right]
        alive[right] = False
        remaining -= 1
        last = right
        walker = following[right]
        while walker != -1 and joined.supernet_of(entries[walker]):
            absorbed += covered[walker]
            alive[walker] = False
            remaining -= 1
            last = walker
            walker = following[walker]
        earlier = previous[left]
        while earlier != -1 and joined.supernet_of(entries[earlier]):
            absorbed += covered[earlier]
            alive[earlier] = False
            remaining -= 1
            earlier = previous[earlier]

        wasted += joined.num_addresses - absorbed
        # `left` stays the live slot; only its links move.
        entries[left] = joined
        covered[left] = joined.num_addresses
        following[left] = following[last]
        previous[left] = earlier
        if following[left] != -1:
            previous[following[left]] = left
            heapq.heappush(heap, (cost(left, following[left]), left, following[left]))
        if previous[left] != -1:
            following[previous[left]] = left
            heapq.heappush(heap, (cost(previous[left], left), previous[left], left))

    return [entries[index] for index in range(len(entries)) if alive[index]], wasted


def encode(networks: list[ipaddress._BaseNetwork]) -> bytes:
    """Pack collapsed networks into the fixed-width form the app parses.

    Collapsing here rather than on the device is most of the point: the parse
    runs inside a NetworkExtension against a fixed memory budget, and the file
    is read once per connect.
    """
    # collapse_addresses refuses a mixed-family list, so the families are
    # separated before collapsing rather than after.
    v4 = sorted(ipaddress.collapse_addresses(n for n in networks if n.version == 4))
    v6 = sorted(ipaddress.collapse_addresses(n for n in networks if n.version == 6))
    body = bytearray()
    for network in v4:
        body += int(network.network_address).to_bytes(4, "big")
        body.append(network.prefixlen)
    for network in v6:
        body += int(network.network_address).to_bytes(16, "big")
        body.append(network.prefixlen)
    return HEADER.pack(MAGIC, FORMAT_VERSION, 0, 0, len(v4), len(v6)) + bytes(body)


def decode(blob: bytes) -> list[ipaddress._BaseNetwork]:
    """Read back what encode wrote. The Swift parser must agree with this."""
    magic, version, _, _, v4_count, v6_count = HEADER.unpack_from(blob)
    if magic != MAGIC:
        raise ValueError("not a Queqiao country route set")
    if version != FORMAT_VERSION:
        raise ValueError(f"unsupported format version {version}")
    expected = HEADER.size + 5 * v4_count + 17 * v6_count
    if len(blob) != expected:
        raise ValueError(f"expected {expected} bytes, found {len(blob)}")
    networks: list[ipaddress._BaseNetwork] = []
    offset = HEADER.size
    for _ in range(v4_count):
        address = int.from_bytes(blob[offset:offset + 4], "big")
        networks.append(ipaddress.IPv4Network((address, blob[offset + 4])))
        offset += 5
    for _ in range(v6_count):
        address = int.from_bytes(blob[offset:offset + 16], "big")
        networks.append(ipaddress.IPv6Network((address, blob[offset + 16])))
        offset += 17
    return networks


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--input",
        required=True,
        type=pathlib.Path,
        help="a delegated-* registry file, e.g. delegated-apnic-latest",
    )
    parser.add_argument("--output", required=True, type=pathlib.Path)
    parser.add_argument("--country", default="CN", help="ISO 3166 code (default CN)")
    parser.add_argument(
        "--max-blocks",
        type=int,
        default=7936,
        help=(
            "aggregate until at most this many blocks remain. The default is "
            "the app's route cap less a full hand-written bypass list, so the "
            "shipped set plus the user's own routes always fit. Today the "
            "registry set is well under it and no aggregation happens; "
            "over-inclusion is reported whenever it does."
        ),
    )
    arguments = parser.parse_args(argv)

    networks = parse_delegation(
        arguments.input.read_text(encoding="utf-8", errors="replace"),
        arguments.country,
    )
    if not networks:
        print(f"no {arguments.country} delegations found in {arguments.input}", file=sys.stderr)
        return 1

    families = {4: [], 6: []}
    for network in networks:
        families[network.version].append(network)
    exact = {
        version: sorted(ipaddress.collapse_addresses(members))
        for version, members in families.items()
    }
    # IPv6 delegations are already few and coarse; spending the block budget on
    # IPv4, where the registry set is an order of magnitude finer, is what keeps
    # the over-inclusion small.
    budget = max(arguments.max_blocks - len(exact[6]), 1)
    aggregated4, wasted = aggregate(exact[4], budget)
    covered4 = sum(n.num_addresses for n in exact[4])

    blob = encode(aggregated4 + exact[6])
    restored = decode(blob)
    arguments.output.parent.mkdir(parents=True, exist_ok=True)
    arguments.output.write_bytes(blob)
    v4 = sum(1 for n in restored if n.version == 4)
    print(
        f"{arguments.output}: {v4} IPv4 and {len(restored) - v4} IPv6 blocks "
        f"from {len(networks)} delegations, {len(blob)} bytes"
    )
    print(
        f"IPv4 aggregation: {len(exact[4])} -> {v4} blocks, over-including "
        f"{wasted} addresses ({100 * wasted / covered4:.2f}% of the {covered4} "
        f"the registry delegates)"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
