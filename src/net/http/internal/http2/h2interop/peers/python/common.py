# Copyright 2026 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

"""Shared helpers for the h2interop Python (hyper-h2) client and server."""

import asyncio
import sys

ALPHABET = b"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ\n"


def pattern(n):
    """Returns Pattern(n), the n-byte body pattern from peers/README.md."""
    reps = n // len(ALPHABET) + 1
    return (ALPHABET * reps)[:n]


def header_pattern(n):
    """Returns headerPattern(n) from peers/README.md."""
    a = "abcdefghijklmnopqrstuvwxyz0123456789"
    return (a * (n // len(a) + 1))[:n]


def log(*args):
    print(*args, file=sys.stderr, flush=True)


def dec(b):
    """Decodes a header name or value received from h2 as bytes."""
    if isinstance(b, str):
        return b
    return b.decode("utf-8", errors="replace")


def ecode(code):
    """Formats an HTTP/2 error code."""
    return getattr(code, "name", str(code))


class StreamGone(Exception):
    """Raised when sending on a stream that has been reset or whose
    connection has died."""


class Conn:
    """Conn wraps an h2.connection.H2Connection and its transport.

    All h2 calls happen on the asyncio event loop thread, so no locking
    is needed as long as callers don't await between related h2 calls.
    """

    def __init__(self, h2conn, reader, writer):
        self.h2 = h2conn
        self.reader = reader
        self.writer = writer
        self.dead = None  # reason string once the connection is unusable
        self._changed = asyncio.Event()

    def flush(self):
        data = self.h2.data_to_send()
        if data and not self.writer.is_closing():
            self.writer.write(data)

    async def drain(self):
        self.flush()
        if not self.writer.is_closing():
            try:
                await self.writer.drain()
            except (ConnectionError, OSError) as e:
                self.kill("write: %s" % e)

    def pulse(self):
        """Wakes all coroutines blocked in wait_change."""
        self._changed.set()
        self._changed = asyncio.Event()

    async def wait_change(self):
        """Blocks until the next pulse (window update, settings change,
        stream state change, or connection death)."""
        await self._changed.wait()

    def kill(self, reason):
        if self.dead is None:
            self.dead = reason
        self.pulse()
        if not self.writer.is_closing():
            self.writer.close()

    async def send_body(self, sid, data, end_stream, alive, chunk=None):
        """Sends data on stream sid, respecting the connection and stream
        flow control windows and the peer's SETTINGS_MAX_FRAME_SIZE.

        alive is a callable returning whether the stream is still usable.
        If chunk is set, at most chunk bytes are sent per DATA frame.
        """
        mv = memoryview(data)
        off = 0
        if not mv and end_stream:
            self._check(alive)
            self.h2.end_stream(sid)
            await self.drain()
            return
        while off < len(mv):
            while True:
                self._check(alive)
                w = self.h2.local_flow_control_window(sid)
                if w > 0:
                    break
                await self.wait_change()
            n = min(w, self.h2.max_outbound_frame_size, len(mv) - off)
            if chunk:
                n = min(n, chunk)
            last = end_stream and off + n == len(mv)
            self.h2.send_data(sid, bytes(mv[off:off + n]), end_stream=last)
            off += n
            await self.drain()

    def _check(self, alive):
        if self.dead is not None:
            raise StreamGone("connection closed: " + self.dead)
        if not alive():
            raise StreamGone("stream closed")
