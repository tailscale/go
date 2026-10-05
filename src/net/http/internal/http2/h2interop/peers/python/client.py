# Copyright 2026 The Go Authors. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

"""h2interop JSON-protocol client using the h2 (hyper-h2) library.

Reads a ClientSpec on stdin and writes a ClientOutput on stdout.
See ../README.md.
"""

import asyncio
import hashlib
import json
import ssl
import sys
import traceback
import urllib.parse

import h2.config
import h2.connection
import h2.events
import h2.exceptions
from h2.errors import ErrorCodes

from common import Conn, StreamGone, dec, ecode, log, pattern

MAX_REPORTED_BODY = 64 << 10
SETTINGS_WAIT = 10  # seconds to wait for the server's first SETTINGS
CONTINUE_WAIT = 10  # seconds to wait for a 100 (Continue) response


class Request:
    def __init__(self, spec):
        self.spec = spec
        self.sid = None
        self.status = 0
        self.header = []
        self.trailer = []
        self.informational = []
        self.sha = hashlib.sha256()
        self.n = 0
        self.prefix = bytearray()  # first MAX_REPORTED_BODY+1 body bytes
        self.error = ""
        self.canceled = False
        self.reset = False  # stream reset by either side
        self.done = asyncio.Event()
        self.continue_ev = asyncio.Event()  # 100 or final response seen

    def finish(self, error=""):
        if self.done.is_set():
            return
        if error and not self.error:
            self.error = error
        self.done.set()
        self.continue_ev.set()

    def result(self):
        r = {
            "status": self.status,
            "proto": "h2",
            "header": self.header,
            "body_len": self.n,
            "body_sha256": self.sha.hexdigest(),
        }
        if self.trailer:
            r["trailer"] = self.trailer
        if self.informational:
            r["informational"] = self.informational
        if len(self.prefix) <= MAX_REPORTED_BODY and self.n == len(self.prefix):
            try:
                body = bytes(self.prefix).decode("utf-8")
                if body:
                    r["body"] = body
            except UnicodeDecodeError:
                pass
        if self.error:
            r["error"] = self.error
        if self.canceled:
            r["canceled"] = True
        return r


def fields(headers):
    """Converts h2 headers to [name, value] lists, without pseudo-headers."""
    out = []
    for k, v in headers:
        k, v = dec(k), dec(v)
        if not k.startswith(":"):
            out.append([k, v])
    return out


def status_of(headers):
    for k, v in headers:
        if dec(k) == ":status":
            return int(dec(v))
    return 0


class Client:
    def __init__(self, spec):
        self.spec = spec
        u = urllib.parse.urlsplit(spec["url"])
        self.scheme = u.scheme
        self.host = u.hostname
        self.port = u.port or (443 if u.scheme == "https" else 80)
        self.authority = spec.get("authority") or u.netloc
        self.base_path = u.path.rstrip("/")
        self.conn = None
        self.streams = {}  # stream ID -> Request
        self.settings_ev = asyncio.Event()
        self.goaway = None

    async def connect(self):
        sslctx = None
        if self.scheme == "https":
            sslctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
            sslctx.check_hostname = False
            sslctx.verify_mode = ssl.CERT_NONE
            sslctx.set_alpn_protocols(["h2"])
        reader, writer = await asyncio.open_connection(
            self.host, self.port, ssl=sslctx,
            server_hostname=self.host if sslctx else None)
        if sslctx:
            proto = writer.get_extra_info("ssl_object").selected_alpn_protocol()
            if proto != "h2":
                writer.close()
                raise RuntimeError("server negotiated ALPN %r, not h2" % proto)
        cfg = h2.config.H2Configuration(client_side=True)
        self.conn = Conn(h2.connection.H2Connection(config=cfg), reader, writer)
        self.conn.h2.initiate_connection()
        await self.conn.drain()
        self.read_task = asyncio.create_task(self.read_loop())

    def kill(self, reason):
        log("connection failed:", reason)
        self.conn.kill(reason)
        self.settings_ev.set()
        for req in self.streams.values():
            req.finish("connection closed: " + reason)

    async def read_loop(self):
        c = self.conn
        try:
            while c.dead is None:
                data = await c.reader.read(65536)
                if not data:
                    self.kill("EOF from server")
                    return
                try:
                    events = c.h2.receive_data(data)
                except h2.exceptions.ProtocolError as e:
                    c.flush()  # send h2's GOAWAY
                    self.kill("h2 protocol error: %r" % e)
                    return
                for ev in events:
                    self.handle(ev)
                c.flush()
                c.pulse()
        except Exception as e:
            self.kill("read: %r" % e)

    def handle(self, ev):
        c = self.conn
        if isinstance(ev, h2.events.RemoteSettingsChanged):
            self.settings_ev.set()
            return
        if isinstance(ev, h2.events.ConnectionTerminated):
            self.goaway = ev
            msg = "GOAWAY from server: error=%s last_stream_id=%d debug=%r" % (
                ecode(ev.error_code), ev.last_stream_id, ev.additional_data)
            log(msg)
            for sid, req in self.streams.items():
                if sid > ev.last_stream_id or ev.error_code != ErrorCodes.NO_ERROR:
                    req.finish(msg)
            return
        sid = getattr(ev, "stream_id", None)
        req = self.streams.get(sid)
        if req is None:
            if isinstance(ev, h2.events.DataReceived):
                c.h2.acknowledge_received_data(ev.flow_controlled_length, ev.stream_id)
            return
        if isinstance(ev, h2.events.InformationalResponseReceived):
            st = status_of(ev.headers)
            req.informational.append({"status": st, "header": fields(ev.headers)})
            if st == 100:
                req.continue_ev.set()
        elif isinstance(ev, h2.events.ResponseReceived):
            req.status = status_of(ev.headers)
            req.header = fields(ev.headers)
            req.continue_ev.set()
        elif isinstance(ev, h2.events.DataReceived):
            c.h2.acknowledge_received_data(ev.flow_controlled_length, sid)
            if req.done.is_set():
                return
            req.sha.update(ev.data)
            req.n += len(ev.data)
            if len(req.prefix) <= MAX_REPORTED_BODY:
                req.prefix += ev.data[:MAX_REPORTED_BODY + 1 - len(req.prefix)]
            ca = req.spec.get("cancel_after") or 0
            if ca > 0 and req.n >= ca:
                c.h2.reset_stream(sid, ErrorCodes.CANCEL)
                req.reset = True
                req.canceled = True
                req.finish()
        elif isinstance(ev, h2.events.TrailersReceived):
            req.trailer = fields(ev.headers)
        elif isinstance(ev, h2.events.StreamEnded):
            req.finish()
        elif isinstance(ev, h2.events.StreamReset):
            req.reset = True
            how = "server" if ev.remote_reset else "client (h2)"
            req.finish("stream reset by %s: %s" % (how, ecode(ev.error_code)))

    async def wait_for_slot(self):
        """Waits until a new stream may be opened under the server's
        SETTINGS_MAX_CONCURRENT_STREAMS."""
        c = self.conn
        while True:
            if c.dead is not None:
                raise StreamGone("connection closed: " + c.dead)
            if self.goaway is not None:
                raise StreamGone("connection received GOAWAY")
            if c.h2.open_outbound_streams < c.h2.remote_settings.max_concurrent_streams:
                return
            await c.wait_change()

    async def do(self, req):
        try:
            await self._do(req)
        except Exception as e:
            if not isinstance(e, (StreamGone, h2.exceptions.StreamClosedError)):
                log("request %s %s: %s" % (req.spec.get("method"), req.spec.get("path"),
                                           traceback.format_exc()))
            if not req.done.is_set() and req.sid is not None and not req.reset:
                try:
                    self.conn.h2.reset_stream(req.sid, ErrorCodes.CANCEL)
                    req.reset = True
                    self.conn.flush()
                except Exception:
                    pass
            # A failure to send the rest of the body after the server
            # completed its response is not a request failure.
            req.finish("%s: %s" % (type(e).__name__, e))
        finally:
            self.conn.pulse()

    async def _do(self, req):
        c = self.conn
        rs = req.spec
        try:
            await asyncio.wait_for(self.settings_ev.wait(), SETTINGS_WAIT)
        except asyncio.TimeoutError:
            log("no SETTINGS from server after %ds; proceeding" % SETTINGS_WAIT)
        await self.wait_for_slot()

        body_len = rs.get("body_len") or 0
        trailer = rs.get("trailer") or []
        headers = [
            (":method", rs["method"]),
            (":scheme", self.scheme),
            (":authority", self.authority),
            (":path", self.base_path + rs["path"]),
        ]
        if body_len > 0 and not rs.get("no_content_length"):
            headers.append(("content-length", str(body_len)))
        if rs.get("expect_continue"):
            headers.append(("expect", "100-continue"))
        if trailer:
            # RFC 9110 section 6.6.2: a sender that intends to generate
            # trailer fields SHOULD declare them in a Trailer header field.
            names = []
            for k, _ in trailer:
                if k not in names:
                    names.append(k)
            headers.append(("trailer", ", ".join(names)))
        for k, v in rs.get("header") or []:
            headers.append((k, v))
        headers = [(k.encode("utf-8"), v.encode("utf-8")) for k, v in headers]
        has_body = body_len > 0 or bool(trailer)

        sid = c.h2.get_next_available_stream_id()
        req.sid = sid
        self.streams[sid] = req
        c.h2.send_headers(sid, headers, end_stream=not has_body)
        await c.drain()

        if has_body:
            if rs.get("expect_continue"):
                try:
                    await asyncio.wait_for(req.continue_ev.wait(), CONTINUE_WAIT)
                except asyncio.TimeoutError:
                    raise RuntimeError("no 100 (Continue) or final response after %ds"
                                       % CONTINUE_WAIT)
            try:
                await c.send_body(sid, pattern(body_len), end_stream=not trailer,
                                  alive=lambda: not req.reset)
                if trailer:
                    if req.reset:
                        raise StreamGone("stream closed")
                    c.h2.send_headers(sid, [(k.encode(), v.encode()) for k, v in trailer],
                                      end_stream=True)
                    await c.drain()
            except (StreamGone, h2.exceptions.StreamClosedError):
                if not req.done.is_set():
                    raise
                # The response already finished; the server doesn't
                # want the rest of the body.
        await req.done.wait()


async def run(spec):
    reqs = [Request(r) for r in spec.get("requests") or []]
    out = {"results": None}
    cl = Client(spec)
    try:
        await cl.connect()
    except Exception as e:
        msg = "connect: %s: %s" % (type(e).__name__, e)
        for r in reqs:
            r.finish(msg)
        out["error"] = msg
        out["results"] = [r.result() for r in reqs]
        return out
    if spec.get("concurrent"):
        await asyncio.gather(*(cl.do(r) for r in reqs))
    else:
        for r in reqs:
            await cl.do(r)
    try:
        cl.conn.h2.close_connection()
        await cl.conn.drain()
    except Exception:
        pass
    cl.conn.kill("done")
    cl.read_task.cancel()
    out["results"] = [r.result() for r in reqs]
    return out


def main():
    try:
        spec = json.load(sys.stdin)
    except Exception as e:
        print(json.dumps({"results": [], "error": "bad spec: %s" % e}))
        return
    try:
        out = asyncio.run(run(spec))
    except BaseException as e:
        log(traceback.format_exc())
        out = {"results": [], "error": "fatal: %s: %s" % (type(e).__name__, e)}
    sys.stdout.write(json.dumps(out))
    sys.stdout.write("\n")
    sys.stdout.flush()


if __name__ == "__main__":
    main()
