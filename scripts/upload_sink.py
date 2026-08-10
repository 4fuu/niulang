#!/usr/bin/env python3
"""Bounded HTTP POST sink for controlled WANOPT upload measurements.

This is intentionally a test utility, not a production public service. It
accepts a fixed number of bounded request bodies, reports the byte count, and
then exits. Bind it to a temporary port and remove the listener immediately
after a benchmark.
"""

import argparse
import http.server
import threading


class SinkHandler(http.server.BaseHTTPRequestHandler):
    server_version = "wanopt-upload-sink/1"

    def do_POST(self):  # noqa: N802 - BaseHTTPRequestHandler API
        try:
            length = int(self.headers.get("Content-Length", "-1"))
        except ValueError:
            length = -1
        if length < 0 or length > self.server.max_bytes:
            self.send_error(413, "bounded Content-Length required")
            return
        self.connection.settimeout(self.server.read_timeout)
        remaining = length
        received = 0
        while remaining:
            block = self.rfile.read(min(1024 * 1024, remaining))
            if not block:
                self.send_error(400, "short request body")
                return
            received += len(block)
            remaining -= len(block)
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(str(received))))
        self.end_headers()
        self.wfile.write(str(received).encode("ascii"))
        self.server.accepted += 1
        if self.server.accepted >= self.server.max_requests:
            threading.Thread(target=self.server.shutdown, daemon=True).start()

    def do_GET(self):  # noqa: N802 - make accidental probes harmless
        self.send_error(405, "POST only")

    def log_message(self, fmt, *args):
        print("upload-sink: " + (fmt % args), flush=True)


class SinkServer(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address, max_bytes, max_requests, read_timeout):
        super().__init__(address, SinkHandler)
        self.max_bytes = max_bytes
        self.max_requests = max_requests
        self.read_timeout = read_timeout
        self.accepted = 0


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--listen", default="127.0.0.1:18080")
    parser.add_argument("--max-bytes", type=int, default=64 * 1024 * 1024)
    parser.add_argument("--max-requests", type=int, default=1)
    parser.add_argument("--read-timeout", type=float, default=120.0)
    args = parser.parse_args()
    host, port = args.listen.rsplit(":", 1)
    if args.max_bytes <= 0 or args.max_requests <= 0 or args.read_timeout <= 0:
        parser.error("bounds must be positive")
    server = SinkServer((host, int(port)), args.max_bytes, args.max_requests, args.read_timeout)
    print(f"upload-sink listening on {args.listen}", flush=True)
    server.serve_forever(poll_interval=0.2)


if __name__ == "__main__":
    main()
