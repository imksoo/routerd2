#!/usr/bin/env python3
"""Bridge one QEMU stream netdev and one Linux TAP device.

QEMU stream sends each Ethernet frame as a network-byte-order uint32 length
followed by the frame.  This process is used only by the isolated PVE fixture;
it does not create routes, addresses, or host bridges.
"""
import argparse
import fcntl
import os
import select
import signal
import socket
import struct
import sys


MAX_FRAME = 65535


def write_all(sock, data):
    view = memoryview(data)
    while view:
        written = sock.send(view)
        if written == 0:
            raise ConnectionError("QEMU stream closed")
        view = view[written:]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tap", required=True)
    parser.add_argument("--socket", required=True)
    args = parser.parse_args()
    stream = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    stream.connect(args.socket)
    tap = os.open("/dev/net/tun", os.O_RDWR)
    fcntl.ioctl(tap, 0x400454ca, struct.pack("16sH", args.tap.encode(), 0x1002))
    stopping = False

    def stop(_signal, _frame):
        nonlocal stopping
        stopping = True

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    pending = bytearray()
    expected = None
    try:
        while not stopping:
            readable, _, _ = select.select((stream, tap), (), (), 1)
            if tap in readable:
                frame = os.read(tap, MAX_FRAME)
                write_all(stream, struct.pack("!I", len(frame)) + frame)
            if stream in readable:
                data = stream.recv(65536)
                if not data:
                    return
                pending.extend(data)
                while True:
                    if expected is None:
                        if len(pending) < 4:
                            break
                        expected = struct.unpack("!I", pending[:4])[0]
                        del pending[:4]
                        if expected == 0 or expected > MAX_FRAME:
                            raise ValueError("invalid QEMU stream frame length")
                    if len(pending) < expected:
                        break
                    os.write(tap, pending[:expected])
                    del pending[:expected]
                    expected = None
    finally:
        os.close(tap)
        stream.close()


if __name__ == "__main__":
    try:
        main()
    except (ConnectionError, OSError, ValueError) as error:
        print(error, file=sys.stderr)
        raise SystemExit(1)
