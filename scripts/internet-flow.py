#!/usr/bin/env python3
"""Observe one public HTTPS connection or DNS UDP socket without reconnecting."""
import argparse
import http.client
import json
from pathlib import Path
import signal
import socket
import ssl
import struct
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("tcp", "udp"))
    parser.add_argument("--progress", type=Path, required=True)
    args = parser.parse_args()
    running = True

    def stop(*_):
        nonlocal running
        running = False

    signal.signal(signal.SIGTERM, stop)
    family = socket.SOCK_STREAM if args.mode == "tcp" else socket.SOCK_DGRAM
    with socket.socket(socket.AF_INET, family) as wire:
        wire.settimeout(60 if args.mode == "tcp" else 1)
        wire.connect(("1.1.1.1", 443 if args.mode == "tcp" else 53))
        endpoint = wire.getsockname()
        connection = None
        if args.mode == "tcp":
            connection = http.client.HTTPConnection("one.one.one.one")
            connection.sock = ssl.create_default_context().wrap_socket(wire, server_hostname="one.one.one.one")
            connection.auto_open = False
        received, missed, maximum_gap = 0, 0, 0.0
        last_reply = time.monotonic()
        sequence = 0
        try:
            while running:
                sequence = (sequence + 1) % 65536
                if connection:
                    connection.request("HEAD", "/", headers={"Host": "one.one.one.one"})
                    response = connection.getresponse()
                    response.read()
                    if response.status not in (200, 301, 302, 308) or response.will_close:
                        raise RuntimeError("public HTTP peer closed the persistent connection or rejected the request")
                else:
                    query = struct.pack("!6H", sequence, 0x100, 1, 0, 0, 0) + b"\x07example\x03net\x00\x00\x01\x00\x01"
                    try:
                        wire.send(query)
                        deadline = time.monotonic() + 1
                        matched = False
                        while time.monotonic() < deadline:
                            wire.settimeout(max(.001, deadline-time.monotonic()))
                            try:
                                answer = wire.recv(4096)
                            except socket.timeout:
                                break
                            if len(answer) >= 12 and struct.unpack_from("!H", answer)[0] == sequence:
                                matched = True
                                break
                    except OSError as error:
                        print("UDP loss", error, flush=True)
                        missed += 1
                        time.sleep(.5)
                        continue
                    if not matched:
                        missed += 1
                        continue
                    _, flags, _, answers, _, _ = struct.unpack_from("!6H", answer)
                    if not flags & 0x8000 or flags & 15 or not answers:
                        raise RuntimeError("public DNS peer returned no usable answer")
                now = time.monotonic()
                maximum_gap = max(maximum_gap, now-last_reply)
                last_reply = now
                received += 1
                progress = {"endpoint": endpoint, "received": received, "missed": missed, "maximum_reply_gap_seconds": maximum_gap}
                temporary = args.progress.with_suffix(".tmp")
                temporary.write_text(json.dumps(progress))
                temporary.replace(args.progress)
                time.sleep(.5)
        finally:
            if connection:
                connection.close()


if __name__ == "__main__":
    main()
