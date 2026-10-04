# Copyright 2026 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

"""h2interop route-set server using the h2 (hyper-h2) library.

Run as: server '<ServerSpec JSON>'. See ../README.md.
"""

import asyncio
import hashlib
import json
import re
import ssl
import sys
import traceback

import h2.config
import h2.connection
import h2.events
import h2.exceptions
from h2.errors import ErrorCodes
from h2.settings import SettingCodes

from common import Conn, StreamGone, dec, ecode, header_pattern, log, pattern

HELLO = b"Hello, HTTP/2 interop!\n"
TRAILERS_BODY = b"trailers follow\n"


class Stream:
    """Stream is the server-side state of one request."""

    def __init__(self, conn, sid, headers):
        self.conn = conn
        self.sid = sid
        self.pseudo = {}
        self.header = []  # [(name, value)] excluding pseudo-headers
        for k, v in headers:
            k, v = dec(k), dec(v)
            if k.startswith(":"):
                self.pseudo[k] = v
            else:
                self.header.append((k, v))
        self.method = self.pseudo.get(":method", "")
        self.path = self.pseudo.get(":path", "")
        self.trailer = []
        self.body = asyncio.Queue()  # (data, flow_controlled_length), or None at end
        self.reset = False
        self.discard = False  # handler is done; ack and drop further data
        self.got_end = False  # END_STREAM received
        expect = (self.get("expect") or "").lower()
        self.expect_continue = expect == "100-continue"

    def get(self, name):
        for k, v in self.header:
            if k == name:
                return v
        return None

    def alive(self):
        return not self.reset

    async def read(self):
        """Returns the next chunk of request body, or None at EOF.
        Flow control credit is returned as data is consumed."""
        if self.expect_continue:
            # The handler wants the body; RFC 9110 section 10.1.1.
            self.expect_continue = False
            if not self.reset and not self.got_end:
                self.conn.h2.send_headers(self.sid, [(b":status", b"100")])
                await self.conn.drain()
        item = await self.body.get()
        if self.reset:
            raise StreamGone("stream reset")
        if item is None:
            self.body.put_nowait(None)
            return None
        data, flen = item
        self.conn.h2.acknowledge_received_data(flen, self.sid)
        self.conn.flush()
        return data

    def stop_reading(self):
        """Acknowledges and discards any unread request body."""
        self.discard = True
        while not self.body.empty():
            item = self.body.get_nowait()
            if item is not None:
                try:
                    self.conn.h2.acknowledge_received_data(item[1], self.sid)
                except Exception:
                    pass
        self.conn.flush()

    async def respond(self, status, headers=(), body=b"", trailers=None, chunk=None):
        hs = [(":status", str(status))] + list(headers)
        if self.method == "HEAD":
            body, trailers = b"", None
        end = not body and not trailers
        self.conn.h2.send_headers(self.sid, enc(hs), end_stream=end)
        await self.conn.drain()
        if body:
            await self.conn.send_body(self.sid, body, end_stream=not trailers,
                                      alive=self.alive, chunk=chunk)
        if trailers:
            if self.reset:
                raise StreamGone("stream reset")
            self.conn.h2.send_headers(self.sid, enc(trailers), end_stream=True)
            await self.conn.drain()


def enc(headers):
    return [(k.encode("utf-8") if isinstance(k, str) else k,
             v.encode("utf-8") if isinstance(v, str) else v) for k, v in headers]


def lower_map(fields):
    m = {}
    for k, v in fields:
        m.setdefault(k.lower(), []).append(v)
    return m


def json_body(obj):
    j = json.dumps(obj, separators=(",", ":")).encode()
    return [("content-type", "application/json"), ("content-length", str(len(j)))], j


ROUTE_RE = re.compile(r"^/(bytes|stream|status|bigheader|manyheaders|delay)/(\d+)$")


async def handle(s):
    path = s.path.split("?", 1)[0]
    m = ROUTE_RE.match(path)
    route, n = (m.group(1), int(m.group(2))) if m else (path, None)

    if route == "/hello":
        await s.respond(200, [("content-type", "text/plain"),
                              ("content-length", str(len(HELLO)))], HELLO)
    elif route == "bytes":
        await s.respond(200, [("content-type", "application/octet-stream"),
                              ("content-length", str(n))], pattern(n))
    elif route == "stream":
        # Each 1000-byte write is sent as its own DATA frame.
        await s.respond(200, [("content-type", "application/octet-stream")],
                        pattern(n), chunk=1000)
    elif route == "/echo":
        hs = [("content-type", "application/octet-stream")]
        cl = s.get("content-length")
        if cl is not None:
            hs.append(("content-length", cl))
        c = s.conn
        c.h2.send_headers(s.sid, enc([(":status", "200")] + hs))
        await c.drain()
        while True:
            data = await s.read()
            if data is None:
                break
            if data:
                await c.send_body(s.sid, data, end_stream=False, alive=s.alive)
        await c.send_body(s.sid, b"", end_stream=True, alive=s.alive)
    elif route == "/upload":
        h = hashlib.sha256()
        total = 0
        while True:
            data = await s.read()
            if data is None:
                break
            h.update(data)
            total += len(data)
        cl = s.get("content-length")
        ub = {"len": total, "sha256": h.hexdigest(),
              "content_length": int(cl) if cl is not None else -1}
        if s.trailer:
            ub["trailer"] = lower_map(s.trailer)
        hs, j = json_body(ub)
        await s.respond(200, hs, j)
    elif route == "/trailers":
        await s.respond(200, [("trailer", "x-trailer-a, x-trailer-b"),
                              ("content-type", "text/plain")], TRAILERS_BODY,
                        trailers=[("x-trailer-a", "1"), ("x-trailer-b", "two")])
    elif route == "status":
        await s.respond(n)
    elif route == "bigheader":
        await s.respond(200, [("x-big", header_pattern(n))], b"ok\n")
    elif route == "manyheaders":
        await s.respond(200, [("x-h-%d" % i, "value-%d" % i) for i in range(n)], b"ok\n")
    elif route == "/early-hints":
        s.conn.h2.send_headers(s.sid, enc([(":status", "103"),
                                           ("link", "</style.css>; rel=preload; as=style")]))
        await s.conn.drain()
        await s.respond(200, [], b"ok\n")
    elif route == "/info":
        authority = s.pseudo.get(":authority") or s.get("host") or ""
        hs, j = json_body({"method": s.method, "path": s.path, "authority": authority,
                           "proto": "HTTP/2.0", "header": lower_map(s.header)})
        await s.respond(200, hs, j)
    elif route == "delay":
        deadline = asyncio.get_running_loop().time() + n / 1000
        while s.alive():
            left = deadline - asyncio.get_running_loop().time()
            if left <= 0:
                break
            try:
                await asyncio.wait_for(s.conn.wait_change(), left)
            except asyncio.TimeoutError:
                pass
        if s.alive():
            await s.respond(200, [], b"ok\n")
    elif route == "/rst":
        c = s.conn
        c.h2.send_headers(s.sid, enc([(":status", "200"),
                                      ("content-type", "application/octet-stream")]))
        await c.send_body(s.sid, pattern(1000), end_stream=False, alive=s.alive)
        c.h2.reset_stream(s.sid, ErrorCodes.INTERNAL_ERROR)
        await c.drain()
    else:
        await s.respond(404, [("content-type", "text/plain")], b"404 page not found\n")


async def run_stream(s):
    try:
        await handle(s)
    except (StreamGone, h2.exceptions.StreamClosedError) as e:
        if s.conn.dead is None:
            log("stream %d %s %s: %s" % (s.sid, s.method, s.path, e))
    except Exception:
        log("stream %d %s %s: %s" % (s.sid, s.method, s.path, traceback.format_exc()))
        if s.conn.dead is None and not s.reset:
            try:
                s.conn.h2.reset_stream(s.sid, ErrorCodes.INTERNAL_ERROR)
                s.reset = True
            except Exception:
                pass
    finally:
        s.stop_reading()
        s.conn.pulse()


def settings_from_spec(spec):
    out = {}
    for name, value in (spec.get("settings") or {}).items():
        code = name[len("SETTINGS_"):] if name.startswith("SETTINGS_") else name
        try:
            out[SettingCodes[code]] = value
        except KeyError:
            log("ignoring unsupported setting", name)
    return out


async def serve_conn(spec, settings, reader, writer):
    peer = writer.get_extra_info("peername")
    cfg = h2.config.H2Configuration(client_side=False)
    c = Conn(h2.connection.H2Connection(config=cfg), reader, writer)
    streams = {}
    tasks = set()
    try:
        c.h2.initiate_connection()
        if settings:
            c.h2.update_settings(settings)
        await c.drain()
        while c.dead is None:
            data = await reader.read(65536)
            if not data:
                break
            try:
                events = c.h2.receive_data(data)
            except h2.exceptions.ProtocolError as e:
                log("conn %s: h2 protocol error: %r" % (peer, e))
                c.flush()  # send h2's GOAWAY
                break
            for ev in events:
                sid = getattr(ev, "stream_id", None)
                s = streams.get(sid)
                if isinstance(ev, h2.events.RequestReceived):
                    s = Stream(c, sid, ev.headers)
                    streams[sid] = s
                    t = asyncio.create_task(run_stream(s))
                    tasks.add(t)
                    t.add_done_callback(tasks.discard)
                elif isinstance(ev, h2.events.DataReceived):
                    if s is None or s.discard:
                        c.h2.acknowledge_received_data(ev.flow_controlled_length, sid)
                    else:
                        s.body.put_nowait((ev.data, ev.flow_controlled_length))
                elif isinstance(ev, h2.events.TrailersReceived):
                    if s:
                        s.trailer = [(dec(k), dec(v)) for k, v in ev.headers]
                elif isinstance(ev, h2.events.StreamEnded):
                    if s:
                        s.got_end = True
                        s.body.put_nowait(None)
                elif isinstance(ev, h2.events.StreamReset):
                    if s:
                        s.reset = True
                        s.body.put_nowait(None)
                        streams.pop(sid, None)
                elif isinstance(ev, h2.events.ConnectionTerminated):
                    if ev.error_code != ErrorCodes.NO_ERROR:
                        log("conn %s: GOAWAY from client: %s %r" % (peer, ecode(ev.error_code),
                                                                   ev.additional_data))
            c.flush()
            c.pulse()
    except (ConnectionError, OSError, ssl.SSLError) as e:
        log("conn %s: %r" % (peer, e))
    except Exception:
        log("conn %s: %s" % (peer, traceback.format_exc()))
    finally:
        try:
            await c.drain()
        except Exception:
            pass
        c.kill("connection closed")
        for t in list(tasks):
            t.cancel()


async def main():
    spec = json.loads(sys.argv[1])
    settings = settings_from_spec(spec)
    sslctx = None
    if spec.get("tls"):
        sslctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        sslctx.load_cert_chain(spec["cert"], spec["key"])
        sslctx.set_alpn_protocols(["h2"])
    host, port = spec["addr"].rsplit(":", 1)
    srv = await asyncio.start_server(
        lambda r, w: serve_conn(spec, settings, r, w), host, int(port), ssl=sslctx,
        limit=1 << 20)
    log("python-h2 server listening on", spec["addr"], "tls" if sslctx else "h2c",
        "settings", {k.name: v for k, v in settings.items()})
    async with srv:
        await srv.serve_forever()


if __name__ == "__main__":
    asyncio.run(main())
