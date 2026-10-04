// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/internal/http2"
	"slices"
	"strconv"
	"strings"
	"time"
)

// This file has frame-level conformance tests of the Go client. A
// scripted server (rawConn) sends the Go client valid and invalid
// frames, and each test checks that the client reacts as RFC 9113,
// RFC 7541, and RFC 9218 require. The tests are named
// conform/client/<name>.

type clientConfCase struct {
	name string
	// desc states the requirement being tested.
	desc string
	// settings are the server's initial SETTINGS.
	settings []http2.Setting
	// reqs are the requests the Go client makes, sequentially
	// (concurrently if concurrent is set). The default is GET /.
	reqs       []*Req
	concurrent bool
	// profile is the Go client profile, if not the default.
	profile *goProfile
	// rawStart runs script immediately after reading the client
	// preface, before the server sends SETTINGS.
	rawStart bool
	// covers are the IDs of the requirements (see requirements.txt)
	// the case covers.
	covers string
	script func(x *clientConfRun)
}

type clientConfRun struct {
	tc    *testCtx
	sc    *rawConn
	ln    net.Listener
	conns chan net.Conn // connections that sent the client preface
	req   *rawFrame     // the first request's HEADERS
	// clientSettings are the client's initial settings.
	clientSettings map[uint16]uint32
	results        chan []*Result
	got            []*Result
}

func init() {
	for _, c := range clientConfCases {
		extraTests = append(extraTests, &Test{
			Name: "conform/client/" + c.name,
			Desc: c.desc,
			Run: func(ctx context.Context, tc *testCtx) {
				runClientConf(ctx, tc, c)
			},
		})
	}
}

func runClientConf(ctx context.Context, tc *testCtx, c *clientConfCase) {
	tc.peerFindingsIgnored = true
	tc.Logf("requirement: %s", c.desc)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tc.Errorf("%v", err)
		return
	}
	defer ln.Close()
	tap, err := tc.Tap(ln.Addr().String(), false, "Go client", "test server")
	if err != nil {
		tc.Errorf("%v", err)
		return
	}
	reqs := c.reqs
	if reqs == nil {
		reqs = []*Req{{Method: "GET", Path: "/"}}
	}
	spec := &ClientSpec{URL: "http://" + tap.Addr(), H2C: true, Requests: reqs, Concurrent: c.concurrent}
	profile := c.profile
	if profile == nil {
		profile = goProfiles[0]
	}
	x := &clientConfRun{tc: tc, ln: ln, results: make(chan []*Result, 1), conns: make(chan net.Conn, 100)}
	go x.acceptLoop()
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	go func() {
		out, _ := (&goClient{profile: profile}).Do(cctx, tc, spec)
		x.results <- out.Results
	}()

	x.sc = x.accept(c.rawStart, c.settings)
	if x.sc == nil {
		return
	}
	if !c.rawStart {
		x.req = x.waitRequest(x.sc, 1)
		if x.req == nil {
			return
		}
	}
	c.script(x)
	if x.got == nil {
		select {
		case x.got = <-x.results:
		case <-time.After(5 * time.Second):
			tc.Logf("Go client request did not finish")
		}
	}
	for i, r := range x.got {
		tc.Logf("Go client result %d: status=%d err=%q body=%q", i, r.Status, r.Error, truncate(r.Body, 200))
	}
}

// acceptLoop accepts connections from the Go client, and sends those
// on which the client sends the connection preface to x.conns. (The
// Go client may open more connections than it uses.)
func (x *clientConfRun) acceptLoop() {
	for {
		c, err := x.ln.Accept()
		if err != nil {
			return
		}
		x.tc.Cleanup(func() { c.Close() })
		go func() {
			preface := make([]byte, len(clientPreface))
			c.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := io.ReadFull(c, preface); err != nil || string(preface) != clientPreface {
				x.tc.Logf("connection from Go client without preface (%q, %v)", preface, err)
				c.Close()
				return
			}
			c.SetReadDeadline(time.Time{})
			x.conns <- c
		}()
	}
}

// accept waits for a connection from the Go client and performs the
// server side of the connection preface. If raw is set, it only
// reads the client preface.
func (x *clientConfRun) accept(raw bool, settings []http2.Setting) *rawConn {
	tc := x.tc
	var c net.Conn
	select {
	case c = <-x.conns:
	case <-time.After(5 * time.Second):
		tc.Failf("Go client did not connect")
		return nil
	}
	sc := newRawConn(tc, "server", c)
	sc.startReading()
	if raw {
		return sc
	}
	sc.writeSettings(settings...)
	f, _ := sc.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Type == ftSettings && f.Flags&flagAck == 0 })
	if f == nil {
		tc.Failf("client did not send SETTINGS")
		return nil
	}
	if x.clientSettings == nil {
		x.clientSettings = map[uint16]uint32{}
		for p := f.Payload; len(p) >= 6; p = p[6:] {
			x.clientSettings[binary.BigEndian.Uint16(p)] = binary.BigEndian.Uint32(p[2:])
		}
	}
	sc.writeSettingsAck()
	return sc
}

// waitRequest waits for a complete request field block on stream.
func (x *clientConfRun) waitRequest(sc *rawConn, stream uint32) *rawFrame {
	f, _ := sc.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Fields != nil && f.Stream == stream })
	if f == nil {
		x.tc.Failf("client did not send request on stream %d", stream)
	}
	return f
}

// result waits for the Go client's results.
func (x *clientConfRun) result() []*Result {
	if x.got != nil {
		return x.got
	}
	select {
	case x.got = <-x.results:
	case <-time.After(10 * time.Second):
		x.tc.Failf("Go client request did not finish")
	}
	return x.got
}

// expectRequestError checks that the Go client reported an error for
// its first request.
func (x *clientConfRun) expectRequestError(why string) {
	rs := x.result()
	if len(rs) == 0 {
		return
	}
	if rs[0].Error == "" {
		x.tc.Failf("Go client request succeeded (status %d, body %q); want error: %s", rs[0].Status, truncate(rs[0].Body, 100), why)
	} else {
		x.tc.Logf("Go client returned error as expected: %s", rs[0].Error)
	}
}

// expectRequestOK checks that the Go client's requests succeeded
// with a 200 response and the given body.
func (x *clientConfRun) expectRequestOK(body string) {
	for i, r := range x.result() {
		switch {
		case r.Error != "":
			x.tc.Failf("Go client request %d failed: %s", i, r.Error)
		case r.Status != 200:
			x.tc.Failf("Go client request %d: status %d; want 200", i, r.Status)
		case body != "" && r.Body != body:
			x.tc.Failf("Go client request %d: body %q; want %q", i, r.Body, body)
		}
	}
}

// respond sends a simple 200 response on stream.
func (x *clientConfRun) respond(sc *rawConn, stream uint32, body string) {
	sc.writeFields(stream, false, ":status", "200", "content-length", strconv.Itoa(len(body)))
	sc.writeData(stream, true, []byte(body))
}

// malformed returns a script that sends a malformed response field
// block (given as name, value pairs) and checks that the client
// treats it as malformed (RFC 9113 §8.1.1): a stream error of type
// PROTOCOL_ERROR, and the response isn't accepted.
func malformed(kv ...string) func(x *clientConfRun) {
	return func(x *clientConfRun) {
		x.sc.writeFields(1, false, kv...)
		x.sc.writeData(1, true, []byte("ok"))
		x.sc.expectStreamError(1, http2.ErrCodeProtocol)
		x.expectRequestError("malformed response")
	}
}

// connError returns a script that has the server write a raw frame
// and expects a connection error with one of codes.
func connError(typ, flags byte, stream uint32, payload []byte, codes ...http2.ErrCode) func(x *clientConfRun) {
	return func(x *clientConfRun) {
		x.sc.writeRaw(typ, flags, stream, payload)
		x.sc.expectConnError(codes...)
	}
}

func uploadReq(n int) []*Req {
	return []*Req{{Method: "POST", Path: "/upload", BodyLen: n}}
}

// drainRequestBody reads DATA on stream until END_STREAM, granting
// flow control credit as it goes, and returns the number of bytes.
func (x *clientConfRun) drainRequestBody(sc *rawConn, stream uint32) int {
	n := 0
	for {
		f, closed := sc.waitFor(5*time.Second, func(f *rawFrame) bool {
			return f.Stream == stream && (f.Type == ftData || f.Type == ftRSTStream || f.Fields != nil)
		})
		if f == nil {
			x.tc.Failf("request body incomplete after %d bytes (closed=%v)", n, closed)
			return n
		}
		if f.Type == ftRSTStream {
			x.tc.Failf("client reset stream: %v", f)
			return n
		}
		if f.Type == ftData {
			n += len(f.Payload)
			if len(f.Payload) > 0 {
				sc.writeWindowUpdate(0, uint32(len(f.Payload)))
				sc.writeWindowUpdate(stream, uint32(len(f.Payload)))
			}
		}
		if (f.Type == ftData && f.Flags&flagEndStream != 0) || (f.Fields != nil && f.EndStream) {
			return n
		}
	}
}

var clientConfCases = []*clientConfCase{
	// Connection preface and SETTINGS.
	{
		name:     "first-frame-not-settings",
		covers:   "9113-3.4-4",
		desc:     "RFC 9113 §3.4: the server preface MUST be a SETTINGS frame; an invalid preface is a connection error of type PROTOCOL_ERROR",
		rawStart: true,
		script: func(x *clientConfRun) {
			x.sc.writePing(false, [8]byte{1})
			x.sc.expectConnError(http2.ErrCodeProtocol)
			x.expectRequestError("invalid server preface")
		},
	},
	{
		name:   "settings-ack-with-payload",
		covers: "9113-6.5-4",
		desc:   "RFC 9113 §6.5: a SETTINGS ACK with a non-zero length MUST be treated as a connection error of type FRAME_SIZE_ERROR",
		script: connError(ftSettings, flagAck, 0, settingsPayload(0x3, 100), http2.ErrCodeFrameSize),
	},
	{
		name:   "settings-bad-length",
		covers: "9113-4.2-3 9113-6.5-7 9113-6.5-8",
		desc:   "RFC 9113 §6.5: a SETTINGS frame whose length is not a multiple of 6 MUST be treated as a connection error of type FRAME_SIZE_ERROR",
		script: connError(ftSettings, 0, 0, []byte{0, 3, 0, 0, 0}, http2.ErrCodeFrameSize),
	},
	{
		name:   "settings-on-stream",
		covers: "9113-6.5-6",
		desc:   "RFC 9113 §6.5: a SETTINGS frame with a non-zero stream ID MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: connError(ftSettings, 0, 1, nil, http2.ErrCodeProtocol),
	},
	{
		name:   "settings-enable-push-1",
		covers: "9113-6.5.2-6",
		desc:   "RFC 9113 §6.5.2: a client MUST treat SETTINGS_ENABLE_PUSH=1 from a server as a connection error of type PROTOCOL_ERROR",
		script: connError(ftSettings, 0, 0, settingsPayload(setEnablePush, 1), http2.ErrCodeProtocol),
	},
	{
		name:   "settings-enable-push-2",
		covers: "9113-6.5.2-3",
		desc:   "RFC 9113 §6.5.2: SETTINGS_ENABLE_PUSH other than 0 or 1 MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: connError(ftSettings, 0, 0, settingsPayload(setEnablePush, 2), http2.ErrCodeProtocol),
	},
	{
		name:   "settings-initial-window-too-large",
		covers: "9113-6.5.2-9",
		desc:   "RFC 9113 §6.5.2: SETTINGS_INITIAL_WINDOW_SIZE above 2^31-1 MUST be treated as a connection error of type FLOW_CONTROL_ERROR",
		script: connError(ftSettings, 0, 0, settingsPayload(setInitialWindowSize, 1<<31), http2.ErrCodeFlowControl),
	},
	{
		name:   "settings-max-frame-size-too-small",
		covers: "9113-6.5.2-11",
		desc:   "RFC 9113 §6.5.2: SETTINGS_MAX_FRAME_SIZE below 2^14 MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: connError(ftSettings, 0, 0, settingsPayload(setMaxFrameSize, 16383), http2.ErrCodeProtocol),
	},
	{
		name:   "settings-max-frame-size-too-large",
		covers: "9113-6.5.2-11",
		desc:   "RFC 9113 §6.5.2: SETTINGS_MAX_FRAME_SIZE above 2^24-1 MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: connError(ftSettings, 0, 0, settingsPayload(setMaxFrameSize, 1<<24), http2.ErrCodeProtocol),
	},
	{
		name:   "settings-unknown",
		covers: "9113-5.5-1 9113-6.5.2-13 9113-6.5.3-3",
		desc:   "RFC 9113 §6.5.2: an endpoint that receives a SETTINGS frame with any unknown or unsupported identifier MUST ignore that setting",
		script: func(x *clientConfRun) {
			x.sc.writeRaw(ftSettings, 0, 0, settingsPayload(0xfe, 1, 0x7777, 99))
			x.respond(x.sc, 1, "ok")
			x.sc.expectNoError(500 * time.Millisecond)
			x.expectRequestOK("ok")
		},
	},
	{
		name:   "settings-duplicate",
		covers: "9113-6.5.3-2",
		desc:   "RFC 9113 §6.5.3: the values in a SETTINGS frame MUST be processed in the order they appear (duplicates are permitted)",
		script: func(x *clientConfRun) {
			x.sc.writeRaw(ftSettings, 0, 0, settingsPayload(setInitialWindowSize, 100, setInitialWindowSize, 65535))
			x.respond(x.sc, 1, "ok")
			x.sc.expectNoError(500 * time.Millisecond)
			x.expectRequestOK("ok")
		},
	},
	{
		name:   "settings-acknowledged",
		covers: "9113-3.4-3 9113-6.5.3-4",
		desc:   "RFC 9113 §6.5.3: upon receiving a SETTINGS frame, the recipient MUST immediately emit a SETTINGS frame with the ACK flag set",
		script: func(x *clientConfRun) {
			// The initial SETTINGS was acknowledged during the handshake; send another.
			if f, _ := x.sc.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Type == ftSettings && f.Flags&flagAck != 0 }); f == nil {
				x.tc.Failf("no ACK of initial SETTINGS")
			}
			x.sc.writeSettings(http2.Setting{ID: http2.SettingMaxConcurrentStreams, Val: 50})
			if f, _ := x.sc.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Type == ftSettings && f.Flags&flagAck != 0 }); f == nil {
				x.tc.Failf("no ACK of second SETTINGS")
			}
			x.respond(x.sc, 1, "ok")
			x.expectRequestOK("ok")
		},
	},

	// PING and GOAWAY.
	{
		name:   "ping-ack",
		covers: "9113-6.7-2 9113-6.7-4",
		desc:   "RFC 9113 §6.7: receivers of a PING frame without ACK MUST send a PING frame with ACK and an identical payload",
		script: func(x *clientConfRun) {
			data := [8]byte{'h', '2', 'i', 'n', 't', 'e', 'r', 'p'}
			x.sc.writePing(false, data)
			x.sc.expectPingAck(data)
			x.respond(x.sc, 1, "ok")
			x.expectRequestOK("ok")
		},
	},
	{
		name:   "ping-bad-length",
		covers: "9113-4.2-2 9113-5.4-5 9113-6.7-7",
		desc:   "RFC 9113 §6.7: a PING frame with a length other than 8 MUST be treated as a connection error of type FRAME_SIZE_ERROR",
		script: connError(ftPing, 0, 0, make([]byte, 7), http2.ErrCodeFrameSize),
	},
	{
		name:   "ping-on-stream",
		covers: "9113-6.7-6",
		desc:   "RFC 9113 §6.7: a PING frame with a non-zero stream ID MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: connError(ftPing, 0, 1, make([]byte, 8), http2.ErrCodeProtocol),
	},
	{
		name:   "goaway-on-stream",
		covers: "9113-6.8-4",
		desc:   "RFC 9113 §6.8: a GOAWAY frame with a non-zero stream ID MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: connError(ftGoAway, 0, 1, make([]byte, 8), http2.ErrCodeProtocol),
	},
	{
		name:   "goaway-then-retry",
		covers: "9113-6.8-1 9113-8.7-1 9113-6.8-d3",
		desc:   "RFC 9113 §6.8: requests on streams above GOAWAY's last-stream-ID were not processed and can be retried on a new connection",
		script: func(x *clientConfRun) {
			x.sc.writeGoAway(0, http2.ErrCodeNo)
			x.sc.conn.Close()
			sc2 := x.accept(false, nil)
			if sc2 == nil {
				return
			}
			if f := x.waitRequest(sc2, 1); f == nil {
				return
			}
			x.respond(sc2, 1, "retried")
			x.expectRequestOK("retried")
		},
	},

	// WINDOW_UPDATE and flow control.
	{
		name:   "window-update-zero-connection",
		covers: "9113-6.9-3",
		desc:   "RFC 9113 §6.9: a WINDOW_UPDATE with an increment of 0 on the connection MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: connError(ftWindowUpdate, 0, 0, []byte{0, 0, 0, 0}, http2.ErrCodeProtocol),
	},
	{
		name:   "window-update-zero-stream",
		covers: "9113-6.9-3",
		desc:   "RFC 9113 §6.9: a WINDOW_UPDATE with an increment of 0 on a stream MUST be treated as a stream error of type PROTOCOL_ERROR",
		reqs:   uploadReq(200000),
		script: func(x *clientConfRun) {
			x.sc.writeWindowUpdate(1, 0)
			x.sc.expectStreamError(1, http2.ErrCodeProtocol)
		},
	},
	{
		name:   "window-update-overflow-connection",
		covers: "9113-6.9.1-4",
		desc:   "RFC 9113 §6.9.1: a WINDOW_UPDATE that makes the connection window exceed 2^31-1 MUST be treated as a connection error of type FLOW_CONTROL_ERROR",
		script: connError(ftWindowUpdate, 0, 0, binary.BigEndian.AppendUint32(nil, 1<<31-1), http2.ErrCodeFlowControl),
	},
	{
		name:   "window-update-overflow-stream",
		covers: "9113-6.9.1-4",
		desc:   "RFC 9113 §6.9.1: a WINDOW_UPDATE that makes a stream window exceed 2^31-1 MUST cause the stream to be terminated with FLOW_CONTROL_ERROR",
		reqs:   uploadReq(200000),
		script: func(x *clientConfRun) {
			// The client has used its initial window, so two
			// maximal increments are needed to overflow it.
			x.sc.writeWindowUpdate(1, 1<<31-1)
			x.sc.writeWindowUpdate(1, 1<<31-1)
			x.sc.expectStreamError(1, http2.ErrCodeFlowControl)
		},
	},
	{
		name:   "window-update-bad-length",
		covers: "9113-4.2-2 9113-6.9-6",
		desc:   "RFC 9113 §6.9: a WINDOW_UPDATE frame with a length other than 4 MUST be treated as a connection error of type FRAME_SIZE_ERROR",
		script: connError(ftWindowUpdate, 0, 0, []byte{0, 0, 1}, http2.ErrCodeFrameSize),
	},
	{
		name:   "window-update-idle-stream",
		covers: "9113-5.1-1 9113-5.1-14",
		desc:   "RFC 9113 §5.1: receiving a frame other than HEADERS or PRIORITY on an idle stream MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: connError(ftWindowUpdate, 0, 3, binary.BigEndian.AppendUint32(nil, 100), http2.ErrCodeProtocol),
	},
	{
		name:   "data-exceeds-stream-window",
		covers: "9113-6.9.1-1",
		desc:   "RFC 9113 §6.9.1: a receiver MUST treat DATA exceeding its advertised window as a FLOW_CONTROL_ERROR",
		// A stream window smaller than one frame, so the first DATA
		// frame exceeds it before the client can grant more.
		profile: &goProfile{Name: "tiny-window", HTTP2: http.HTTP2Config{
			MaxReadFrameSize:          16384,
			MaxReceiveBufferPerStream: 1024,
		}},
		script: func(x *clientConfRun) {
			x.sc.writeFields(1, false, ":status", "200")
			x.sc.writeData(1, false, make([]byte, 16384))
			x.sc.expectStreamError(1, http2.ErrCodeFlowControl)
		},
	},
	{
		name:   "initial-window-decrease",
		covers: "9113-6.5.3-1 9113-6.9.2-1 9113-6.9.2-2",
		desc:   "RFC 9113 §6.9.2: senders MUST track negative flow control windows caused by a decrease of SETTINGS_INITIAL_WINDOW_SIZE",
		reqs:   uploadReq(300000),
		script: func(x *clientConfRun) {
			x.sc.writeSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 1000})
			n := x.drainRequestBody(x.sc, 1)
			x.respond(x.sc, 1, mustJSON(UploadBody{Len: int64(n)}))
			x.result()
		},
	},
	{
		name:     "initial-window-zero",
		covers:   "9113-5.2.1-2 9113-6.5-2 9113-6.9-1 9113-5.2.1-d1",
		desc:     "RFC 9113 §6.9: a sender MUST NOT send flow-controlled frames exceeding the receiver's window, including a zero initial window",
		settings: []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 0}},
		reqs:     uploadReq(100000),
		script: func(x *clientConfRun) {
			time.Sleep(200 * time.Millisecond)
			x.sc.writeWindowUpdate(1, 100000)
			x.sc.writeWindowUpdate(0, 100000)
			n := x.drainRequestBody(x.sc, 1)
			x.respond(x.sc, 1, strconv.Itoa(n))
			x.expectRequestOK("100000")
		},
	},

	// DATA, HEADERS, and stream states.
	{
		name:   "data-on-stream-0",
		covers: "9113-6.1-5",
		desc:   "RFC 9113 §6.1: a DATA frame on stream 0 MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: connError(ftData, 0, 0, []byte("x"), http2.ErrCodeProtocol),
	},
	{
		name:   "data-on-idle-stream",
		covers: "9113-5.1-1 9113-5.1-14",
		desc:   "RFC 9113 §5.1: receiving DATA on an idle stream MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: connError(ftData, 0, 3, []byte("x"), http2.ErrCodeProtocol),
	},
	{
		name:   "data-padding-too-long",
		covers: "9113-6.1-7",
		desc:   "RFC 9113 §6.1: a DATA frame whose padding length is >= the payload length MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: func(x *clientConfRun) {
			x.sc.writeFields(1, false, ":status", "200")
			x.sc.writeRaw(ftData, flagPadded, 1, []byte{10, 'a', 'b'})
			x.sc.expectConnError(http2.ErrCodeProtocol)
		},
	},
	{
		name:   "headers-padding-too-long",
		covers: "9113-6.2-7",
		desc:   "RFC 9113 §6.2: a HEADERS frame whose padding length is >= the payload length MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: func(x *clientConfRun) {
			blk := x.sc.block(":status", "200")
			x.sc.writeRaw(ftHeaders, flagPadded|flagEndHeaders, 1, append([]byte{byte(len(blk) + 5)}, blk...))
			x.sc.expectConnError(http2.ErrCodeProtocol)
		},
	},
	{
		name:   "headers-on-unpromised-even-stream",
		covers: "9113-5.1-2 9113-5.1.1-3",
		desc:   "RFC 9113 §5.1.1: an endpoint that receives an unexpected stream identifier MUST respond with a connection error of type PROTOCOL_ERROR (servers open streams only with PUSH_PROMISE)",
		script: func(x *clientConfRun) {
			x.sc.writeFields(2, true, ":status", "200")
			x.sc.expectConnError(http2.ErrCodeProtocol)
		},
	},
	{
		name:   "push-promise-push-disabled",
		covers: "9113-6.5-2 9113-6.5.2-2 9113-6.6-9",
		desc:   "RFC 9113 §6.6: an endpoint that disabled push and received acknowledgment MUST treat PUSH_PROMISE as a connection error of type PROTOCOL_ERROR",
		script: func(x *clientConfRun) {
			blk := x.sc.block(":method", "GET", ":scheme", "http", ":authority", "example.com", ":path", "/pushed")
			x.sc.writeRaw(ftPushPromise, flagEndHeaders, 1, append(binary.BigEndian.AppendUint32(nil, 2), blk...))
			x.sc.expectConnError(http2.ErrCodeProtocol)
		},
	},
	{
		name:   "rst-no-error-after-response",
		covers: "9113-8.1-7",
		desc:   "RFC 9113 §8.1: clients MUST NOT discard a complete response because of a subsequent RST_STREAM with NO_ERROR",
		script: func(x *clientConfRun) {
			x.sc.writeFields(1, false, ":status", "200", "content-length", "2")
			x.sc.writeData(1, true, []byte("ok"))
			x.sc.writeRST(1, http2.ErrCodeNo)
			x.expectRequestOK("ok")
		},
	},
	{
		name:   "early-response-rst-no-error",
		covers: "9113-8.1-6 9113-8.1-7 9113-8.1.1-6",
		desc:   "RFC 9113 §8.1: a server MAY respond before the request body is complete and reset with NO_ERROR; clients MUST NOT discard the response",
		reqs:   uploadReq(10000000),
		script: func(x *clientConfRun) {
			x.sc.writeFields(1, false, ":status", "200", "content-length", "5")
			x.sc.writeData(1, true, []byte("early"))
			x.sc.writeRST(1, http2.ErrCodeNo)
			x.expectRequestOK("early")
		},
	},
	{
		name:   "refused-stream-retry",
		covers: "9113-8.7-1 9113-8.7-d1",
		desc:   "RFC 9113 §8.7: a request refused with REFUSED_STREAM was not processed and can be safely retried",
		script: func(x *clientConfRun) {
			x.sc.writeRST(1, http2.ErrCodeRefusedStream)
			f, _ := x.sc.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Fields != nil && f.Stream > 1 })
			if f == nil {
				x.tc.Failf("client did not retry the request on the same connection")
				return
			}
			x.respond(x.sc, f.Stream, "retried")
			x.expectRequestOK("retried")
		},
	},
	{
		name:   "rst-stream-bad-length",
		covers: "9113-4.2-2 9113-6.4-7",
		desc:   "RFC 9113 §6.4: a RST_STREAM frame with a length other than 4 MUST be treated as a connection error of type FRAME_SIZE_ERROR",
		reqs:   uploadReq(1000000),
		script: connError(ftRSTStream, 0, 1, []byte{0, 0, 8}, http2.ErrCodeFrameSize),
	},
	{
		name:   "rst-stream-on-stream-0",
		covers: "9113-6.4-4",
		desc:   "RFC 9113 §6.4: a RST_STREAM frame on stream 0 MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: connError(ftRSTStream, 0, 0, []byte{0, 0, 0, 8}, http2.ErrCodeProtocol),
	},
	{
		name:   "rst-stream-on-idle-stream",
		covers: "9113-5.1-1 9113-6.4-6",
		desc:   "RFC 9113 §6.4: a RST_STREAM frame on an idle stream MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: connError(ftRSTStream, 0, 3, []byte{0, 0, 0, 8}, http2.ErrCodeProtocol),
	},
	{
		name:   "priority-bad-length",
		covers: "9113-6.3-2",
		desc:   "RFC 9113 §6.3: a PRIORITY frame with a length other than 5 MUST be treated as a stream error of type FRAME_SIZE_ERROR",
		script: func(x *clientConfRun) {
			x.sc.writeRaw(ftPriority, 0, 1, []byte{0, 0, 0, 0})
			x.sc.expectStreamError(1, http2.ErrCodeFrameSize)
		},
	},
	{
		name:   "priority-on-stream-0",
		covers: "9113-6.3-1",
		desc:   "RFC 9113 §6.3: a PRIORITY frame on stream 0 MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: connError(ftPriority, 0, 0, []byte{0, 0, 0, 1, 16}, http2.ErrCodeProtocol),
	},
	{
		name:   "priority-update-from-server",
		covers: "9218-7.1-11",
		desc:   "RFC 9218 §7.1: a client that receives a PRIORITY_UPDATE frame MUST respond with a connection error of type PROTOCOL_ERROR",
		script: connError(ftPriorityUpdate, 0, 0, append(binary.BigEndian.AppendUint32(nil, 1), "u=1"...), http2.ErrCodeProtocol),
	},
	{
		name:   "unknown-frame-type",
		covers: "9113-4.1-2 9113-5.5-1 9113-5.5-2",
		desc:   "RFC 9113 §5.5: implementations MUST ignore and discard frames of unknown types",
		script: func(x *clientConfRun) {
			x.sc.writeRaw(0xfa, 0xff, 0, []byte("ignore me"))
			x.sc.writeRaw(0xfb, 0, 1, []byte("ignore me"))
			x.respond(x.sc, 1, "ok")
			x.sc.expectNoError(300 * time.Millisecond)
			x.expectRequestOK("ok")
		},
	},
	{
		name:   "unknown-flags",
		covers: "9113-4.1-3 9113-5.5-1",
		desc:   "RFC 9113 §4.1: unknown flags MUST be ignored",
		script: func(x *clientConfRun) {
			x.sc.writeRaw(ftHeaders, flagEndHeaders|0x40|0x10, 1, x.sc.block(":status", "200", "content-length", "2"))
			x.sc.writeRaw(ftData, flagEndStream|0x20|0x40, 1, []byte("ok"))
			x.expectRequestOK("ok")
		},
	},
	{
		name:   "reserved-bit-set",
		covers: "9113-4.1-4",
		desc:   "RFC 9113 §4.1: the reserved bit of the stream identifier MUST remain unset when sending and MUST be ignored when receiving",
		script: func(x *clientConfRun) {
			blk := x.sc.block(":status", "200", "content-length", "2")
			x.sc.writeRaw(ftHeaders, flagEndHeaders, 1|1<<31, blk)
			x.sc.writeRaw(ftData, flagEndStream, 1|1<<31, []byte("ok"))
			x.expectRequestOK("ok")
		},
	},

	// Field blocks and HPACK.
	{
		name:   "continuation-without-headers",
		covers: "9113-6.10-6",
		desc:   "RFC 9113 §6.10: a CONTINUATION frame not preceded by HEADERS, PUSH_PROMISE, or CONTINUATION without END_HEADERS MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: func(x *clientConfRun) {
			x.sc.writeContinuation(1, true, x.sc.block(":status", "200"))
			x.sc.expectConnError(http2.ErrCodeProtocol)
		},
	},
	{
		name:   "frame-interleaved-in-field-block",
		covers: "9113-6.2-4 9113-6.10-2",
		desc:   "RFC 9113 §6.10: any other frame between HEADERS and CONTINUATION MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: func(x *clientConfRun) {
			blk := x.sc.block(":status", "200", "x-a", "1")
			x.sc.writeHeaders(1, true, false, blk[:1])
			x.sc.writePing(false, [8]byte{})
			x.sc.writeContinuation(1, true, blk[1:])
			x.sc.expectConnError(http2.ErrCodeProtocol)
		},
	},
	{
		name:   "continuation-other-stream",
		covers: "9113-6.2-4",
		desc:   "RFC 9113 §6.10: a CONTINUATION frame on a different stream MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: func(x *clientConfRun) {
			blk := x.sc.block(":status", "200", "x-a", "1")
			x.sc.writeHeaders(1, true, false, blk[:1])
			x.sc.writeContinuation(3, true, blk[1:])
			x.sc.expectConnError(http2.ErrCodeProtocol)
		},
	},
	{
		name:   "unknown-frame-in-field-block",
		covers: "9113-5.5-3",
		desc:   "RFC 9113 §5.5: extension frames in the middle of a field block MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: func(x *clientConfRun) {
			blk := x.sc.block(":status", "200", "x-a", "1")
			x.sc.writeHeaders(1, true, false, blk[:1])
			x.sc.writeRaw(0xfa, 0, 0, []byte("x"))
			x.sc.writeContinuation(1, true, blk[1:])
			x.sc.expectConnError(http2.ErrCodeProtocol)
		},
	},
	{
		name:   "continuation-split-response",
		covers: "9113-6.10-d1",
		desc:   "RFC 9113 §6.10: a field block may be split across HEADERS and any number of CONTINUATION frames",
		script: func(x *clientConfRun) {
			blk := x.sc.block(":status", "200", "content-length", "2", "x-long", strings.Repeat("v", 100))
			x.sc.writeHeaders(1, false, false, blk[:1])
			for i := 1; i < len(blk); i += 7 {
				x.sc.writeContinuation(1, i+7 >= len(blk), blk[i:min(i+7, len(blk))])
			}
			x.sc.writeData(1, true, []byte("ok"))
			x.expectRequestOK("ok")
		},
	},
	{
		name:   "frame-exceeds-max-frame-size",
		covers: "9113-4.2-2 9113-6.5-2",
		desc:   "RFC 9113 §4.2: an endpoint MUST send FRAME_SIZE_ERROR for a frame exceeding its SETTINGS_MAX_FRAME_SIZE",
		script: func(x *clientConfRun) {
			max := int(x.clientSettings[setMaxFrameSize])
			if max == 0 {
				max = 16384
			}
			x.sc.writeFields(1, false, ":status", "200")
			x.sc.writeData(1, true, make([]byte, max+1))
			x.sc.expectStreamError(1, http2.ErrCodeFrameSize)
		},
	},
	{
		name:   "headers-exceed-max-frame-size",
		covers: "9113-4.2-3",
		desc:   "RFC 9113 §4.2: a frame size error in a frame carrying a field block MUST be treated as a connection error",
		script: func(x *clientConfRun) {
			max := int(x.clientSettings[setMaxFrameSize])
			if max == 0 {
				max = 16384
			}
			x.sc.writeFields(1, true, ":status", "200", "x-big", strings.Repeat("a", max+1))
			x.sc.expectConnError(http2.ErrCodeFrameSize)
		},
	},
	{
		name:   "hpack-invalid-index",
		covers: "9113-4.3-2 9113-4.3-3 9113-5.4-5 9113-5.4.1-1 9113-5.4.1-2 7541-2.3.3-1 7541-3.2-1",
		desc:   "RFC 7541 §2.3.3: an index beyond the tables MUST be treated as a decoding error (RFC 9113 §4.3: COMPRESSION_ERROR)",
		script: func(x *clientConfRun) {
			x.sc.writeHeaders(1, true, true, []byte{0x88, 0xff, 0x7f})
			x.sc.expectConnError(http2.ErrCodeCompression)
		},
	},
	{
		name:   "hpack-table-size-update-too-large",
		covers: "9113-4.3-3 7541-6.3-2",
		desc:   "RFC 7541 §6.3: a dynamic table size update exceeding SETTINGS_HEADER_TABLE_SIZE MUST be treated as a decoding error",
		script: func(x *clientConfRun) {
			// Size update to 8192, then :status 200 (static index 8).
			x.sc.writeHeaders(1, true, true, []byte{0x3f, 0xe1, 0x3f, 0x88})
			x.sc.expectConnError(http2.ErrCodeCompression)
		},
	},
	{
		name:   "hpack-size-update-after-field",
		covers: "9113-4.3-3",
		desc:   "RFC 7541 §4.2: a dynamic table size update MUST occur at the beginning of a field block",
		script: func(x *clientConfRun) {
			x.sc.writeHeaders(1, true, true, []byte{0x88, 0x20})
			x.sc.expectConnError(http2.ErrCodeCompression)
		},
	},
	{
		name:   "hpack-huffman-eos",
		covers: "9113-4.3-3 7541-3.2-1 7541-5.2-3",
		desc:   "RFC 7541 §5.2: a Huffman-encoded string literal containing the EOS symbol MUST be treated as a decoding error",
		script: func(x *clientConfRun) {
			// Literal without indexing, name "x-a", Huffman value of all 1 bits (EOS).
			blk := []byte{0x88, 0x00, 0x03, 'x', '-', 'a', 0x84, 0xff, 0xff, 0xff, 0xff}
			x.sc.writeHeaders(1, true, true, blk)
			x.sc.expectConnError(http2.ErrCodeCompression)
		},
	},
	{
		name:     "header-table-size-zero",
		covers:   "9113-4.3.1-1 9113-6.5-2 7541-4.2-2",
		desc:     "RFC 7541 §4.2: after acknowledging a smaller SETTINGS_HEADER_TABLE_SIZE, the encoder MUST signal the change at the start of the next field block",
		settings: []http2.Setting{{ID: http2.SettingHeaderTableSize, Val: 0}},
		reqs:     []*Req{{Method: "GET", Path: "/a"}, {Method: "GET", Path: "/b"}},
		script: func(x *clientConfRun) {
			x.respond(x.sc, 1, "a")
			f := x.waitRequest(x.sc, 3)
			if f == nil {
				return
			}
			x.respond(x.sc, 3, "b")
			x.result()
			// The validator checks the encoding; see decodeFields.
		},
	},

	// Malformed responses (RFC 9113 §8.1.1, §8.2, §8.3).
	{
		name:   "response-uppercase-field-name",
		covers: "9113-8.1.1-3 9113-8.2.1-3",
		desc:   "RFC 9113 §8.2.1: a field name MUST NOT contain uppercase characters; such a response is malformed",
		script: malformed(":status", "200", "X-Upper", "1"),
	},
	{
		name:   "response-field-name-space",
		covers: "9113-8.2.1-1 9113-8.2.1-2 9113-8.2.1-3",
		desc:   "RFC 9113 §8.2.1: a field name MUST NOT contain SP; such a response is malformed",
		script: malformed(":status", "200", "x space", "1"),
	},
	{
		name:   "response-field-name-colon",
		covers: "9113-8.2.1-4",
		desc:   "RFC 9113 §8.2.1: field names MUST NOT include a colon except for pseudo-headers; such a response is malformed",
		script: malformed(":status", "200", "x:colon", "1"),
	},
	{
		name:   "response-field-name-empty",
		covers: "9113-8.2.1-1",
		desc:   "RFC 9113 §8.2.1, RFC 9110 §5.1: a field name must be a token; an empty name is malformed",
		script: malformed(":status", "200", "", "1"),
	},
	{
		name:   "response-field-value-crlf",
		covers: "9113-8.2.1-2 9113-8.2.1-5 9113-8.2.1-7",
		desc:   "RFC 9113 §8.2.1: a field value MUST NOT contain CR or LF; such a response is malformed",
		script: malformed(":status", "200", "x-a", "a\r\nx-injected: 1"),
	},
	{
		name:   "response-field-value-nul",
		covers: "9113-8.2.1-5",
		desc:   "RFC 9113 §8.2.1: a field value MUST NOT contain NUL; such a response is malformed",
		script: malformed(":status", "200", "x-a", "a\x00b"),
	},
	{
		name:   "response-field-value-leading-space",
		covers: "9113-8.2.1-6 9113-8.2.1-7",
		desc:   "RFC 9113 §8.2.1: a field value MUST NOT start with whitespace; such a response is malformed",
		script: malformed(":status", "200", "x-a", " a"),
	},
	{
		name:   "response-field-value-trailing-tab",
		covers: "9113-8.2.1-6",
		desc:   "RFC 9113 §8.2.1: a field value MUST NOT end with whitespace; such a response is malformed",
		script: malformed(":status", "200", "x-a", "a\t"),
	},
	{
		name:   "response-connection-header",
		covers: "9113-8.2.2-2",
		desc:   "RFC 9113 §8.2.2: a message containing connection-specific fields MUST be treated as malformed",
		script: malformed(":status", "200", "connection", "close"),
	},
	{
		name:   "response-transfer-encoding",
		covers: "9113-8.2.2-2",
		desc:   "RFC 9113 §8.2.2: a message containing Transfer-Encoding MUST be treated as malformed",
		script: malformed(":status", "200", "transfer-encoding", "chunked"),
	},
	{
		name:   "response-keep-alive",
		covers: "9113-8.2.2-2",
		desc:   "RFC 9113 §8.2.2: a message containing Keep-Alive MUST be treated as malformed",
		script: malformed(":status", "200", "keep-alive", "timeout=5"),
	},
	{
		name:   "response-upgrade",
		covers: "9113-8.2.2-2",
		desc:   "RFC 9113 §8.2.2: a message containing Upgrade MUST be treated as malformed",
		script: malformed(":status", "200", "upgrade", "websocket"),
	},
	{
		name:   "response-missing-status",
		covers: "9113-8.1.1-3 9113-8.1.1-5 9113-8.3.2-1",
		desc:   "RFC 9113 §8.3.2: a response MUST include :status; otherwise it is malformed",
		script: malformed("content-type", "text/plain"),
	},
	{
		name:   "response-duplicate-status",
		covers: "9113-8.3-8",
		desc:   "RFC 9113 §8.3: pseudo-headers MUST NOT appear more than once; such a response is malformed",
		script: malformed(":status", "200", ":status", "204"),
	},
	{
		name:   "response-pseudo-after-regular",
		covers: "9113-8.3-6",
		desc:   "RFC 9113 §8.3: all pseudo-headers MUST appear before regular fields; otherwise the response is malformed",
		script: malformed("x-a", "1", ":status", "200"),
	},
	{
		name:   "response-request-pseudo-header",
		covers: "9113-8.3-4",
		desc:   "RFC 9113 §8.3: request pseudo-headers MUST NOT appear in responses; such a response is malformed",
		script: malformed(":status", "200", ":path", "/"),
	},
	{
		name:   "response-unknown-pseudo-header",
		covers: "9113-8.3-4",
		desc:   "RFC 9113 §8.3: endpoints MUST treat a response containing undefined pseudo-headers as malformed",
		script: malformed(":status", "200", ":foo", "bar"),
	},
	{
		name:   "response-invalid-status",
		desc:   "RFC 9110 §15: the status code is a three-digit integer; RFC 9113 §8.1.1 requires treating invalid control data as malformed",
		script: malformed(":status", "2000"),
	},
	{
		name:   "response-status-101",
		desc:   "RFC 9113 §8.6: HTTP/2 does not support 101 (Switching Protocols)",
		script: malformed(":status", "101", "upgrade", "websocket"),
	},
	{
		name:   "response-content-length-too-long",
		covers: "9113-8.1.1-5 9113-8.1.1-d1",
		desc:   "RFC 9113 §8.1.1: a response whose content-length doesn't equal the DATA length is malformed; clients MUST NOT accept it",
		script: func(x *clientConfRun) {
			x.sc.writeFields(1, false, ":status", "200", "content-length", "10")
			x.sc.writeData(1, true, []byte("short"))
			x.expectRequestError("content-length mismatch")
		},
	},
	{
		name:   "response-content-length-too-short",
		covers: "9113-8.1.1-5 9113-8.1.1-d1",
		desc:   "RFC 9113 §8.1.1: a response whose content-length doesn't equal the DATA length is malformed; clients MUST NOT accept it",
		script: func(x *clientConfRun) {
			x.sc.writeFields(1, false, ":status", "200", "content-length", "2")
			x.sc.writeData(1, true, []byte("toolong"))
			x.expectRequestError("content-length mismatch")
		},
	},
	{
		name:   "response-content-length-invalid",
		desc:   "RFC 9110 §8.6: content-length must be a non-negative integer; RFC 9113 §8.1.1 makes such a response malformed",
		script: malformed(":status", "200", "content-length", "abc"),
	},
	{
		name:   "response-trailers-with-pseudo-header",
		covers: "9113-8.1-4",
		desc:   "RFC 9113 §8.1: trailers MUST NOT include pseudo-headers; the response is malformed",
		script: func(x *clientConfRun) {
			x.sc.writeFields(1, false, ":status", "200")
			x.sc.writeData(1, false, []byte("body"))
			x.sc.writeFields(1, true, ":status", "200", "x-trailer", "1")
			x.sc.expectStreamError(1, http2.ErrCodeProtocol)
			x.expectRequestError("pseudo-header in trailers")
		},
	},
	{
		name:   "response-trailers-without-end-stream",
		covers: "9113-8.1-5",
		desc:   "RFC 9113 §8.1: a HEADERS frame without END_STREAM after the final response header section makes the response malformed",
		script: func(x *clientConfRun) {
			x.sc.writeFields(1, false, ":status", "200")
			x.sc.writeData(1, false, []byte("body"))
			x.sc.writeFields(1, false, "x-trailer", "1")
			x.sc.expectStreamError(1, http2.ErrCodeProtocol)
			x.expectRequestError("trailers without END_STREAM")
		},
	},
	{
		name: "response-informational-end-stream",
		desc: "RFC 9113 §8.1: a HEADERS frame with END_STREAM carrying an informational status code is malformed",
		script: func(x *clientConfRun) {
			x.sc.writeFields(1, true, ":status", "103", "link", "</a>")
			x.sc.expectStreamError(1, http2.ErrCodeProtocol)
			x.expectRequestError("1xx with END_STREAM")
		},
	},
	{
		name:   "response-multiple-informational",
		covers: "9113-8.1-1",
		desc:   "RFC 9113 §8.1: a server MAY send any number of interim responses before the final response",
		script: func(x *clientConfRun) {
			x.sc.writeFields(1, false, ":status", "103", "link", "</a>")
			x.sc.writeFields(1, false, ":status", "103", "link", "</b>")
			x.respond(x.sc, 1, "ok")
			x.expectRequestOK("ok")
			if rs := x.result(); len(rs) > 0 && len(rs[0].Informational) != 2 {
				x.tc.Failf("got %d interim responses; want 2", len(rs[0].Informational))
			}
		},
	},

	{
		name:   "window-update-rate",
		covers: "9113-6.9.1-d1",
		desc:   "RFC 9113 §10.5: peers may treat excessive control frames as abusive; a receiver shouldn't send many WINDOW_UPDATE frames per DATA frame",
		script: func(x *clientConfRun) {
			const total = 8 << 20
			frame := int(x.clientSettings[setMaxFrameSize])
			if frame == 0 {
				frame = 16384
			}
			x.sc.writeFields(1, false, ":status", "200", "content-length", strconv.Itoa(total))
			// Send DATA as the client's windows allow.
			connWin, streamWin := 65535, int(x.clientSettings[setInitialWindowSize])
			if _, ok := x.clientSettings[setInitialWindowSize]; !ok {
				streamWin = 65535
			}
			sent, frames, updates := 0, 0, 0
			buf := make([]byte, frame)
			for sent < total {
				for _, f := range x.sc.seen(isType(ftWindowUpdate)) {
					incr := int(binary.BigEndian.Uint32(f.Payload) & (1<<31 - 1))
					if f.Stream == 0 {
						connWin += incr
					} else {
						streamWin += incr
					}
					updates++
				}
				x.sc.mu.Lock()
				x.sc.pending = slices.DeleteFunc(x.sc.pending, isType(ftWindowUpdate))
				x.sc.mu.Unlock()
				n := min(frame, total-sent, connWin, streamWin)
				if n <= 0 {
					time.Sleep(time.Millisecond)
					continue
				}
				x.sc.writeData(1, sent+n == total, buf[:n])
				sent += n
				connWin -= n
				streamWin -= n
				frames++
			}
			x.result()
			time.Sleep(100 * time.Millisecond)
			updates += len(x.sc.seen(isType(ftWindowUpdate)))
			x.tc.Logf("client sent %d WINDOW_UPDATE frames for %d DATA frames of up to %d bytes (%d bytes)", updates, frames, frame, total)
			if updates > 10*frames {
				x.tc.Warnf("client sent %d WINDOW_UPDATE frames for %d DATA frames (%.1f per frame); Envoy's default flood protection allows about 10 per DATA frame",
					updates, frames, float64(updates)/float64(frames))
			}
		},
	},

	// Cancellation.
	{
		name:     "context-deadline-during-body-write",
		desc:     "a request's context deadline ends the request even while its body is blocked on flow control after the response headers arrived (RFC 9113 §8.1 permits responses before the request is complete)",
		settings: []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 0}},
		reqs:     []*Req{{Method: "POST", Path: "/upload", BodyLen: 100000, Timeout: 2 * time.Second}},
		script: func(x *clientConfRun) {
			// Respond without granting any flow control window, so
			// the client can't send its body.
			x.sc.writeFields(1, false, ":status", "200")
			start := time.Now()
			select {
			case x.got = <-x.results:
				x.tc.Logf("request finished after %v", time.Since(start).Round(time.Millisecond))
				if len(x.got) > 0 && x.got[0].Error == "" {
					x.tc.Failf("request succeeded; want context deadline error")
				}
			case <-time.After(8 * time.Second):
				x.tc.Failf("request still running %v after its 2s deadline expired", time.Since(start).Round(time.Second))
			}
		},
	},

	// Concurrency.
	{
		name:       "max-concurrent-streams-after-ack",
		covers:     "9113-5.1.2-1 9113-6.5-2 9113-6.5.3-1",
		desc:       "RFC 9113 §5.1.2: an endpoint MUST NOT exceed the limit set by its peer; once a lower SETTINGS_MAX_CONCURRENT_STREAMS is acknowledged, new streams (including retries of refused ones) must respect it",
		concurrent: true,
		reqs: func() []*Req {
			var rs []*Req
			for range 10 {
				rs = append(rs, &Req{Method: "GET", Path: "/"})
			}
			return rs
		}(),
		rawStart: true,
		script: func(x *clientConfRun) {
			sc := x.sc
			// Let the client send its burst of requests before it
			// learns the limit, which is permitted.
			time.Sleep(200 * time.Millisecond)
			sc.writeSettings(http2.Setting{ID: http2.SettingMaxConcurrentStreams, Val: 1})
			if f, _ := sc.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Type == ftSettings && f.Flags&flagAck == 0 }); f != nil {
				sc.writeSettingsAck()
			}
			if f, _ := sc.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Type == ftSettings && f.Flags&flagAck != 0 }); f == nil {
				x.tc.Failf("client did not acknowledge SETTINGS")
				return
			}
			// Refuse all but the first stream, then serve requests one at
			// a time. The validator checks that the client never has
			// more than one stream open after its ACK.
			served := 0
			var active []uint32
			for served < 10 {
				f, closed := sc.waitFor(5*time.Second, func(f *rawFrame) bool { return f.Fields != nil && f.Type == ftHeaders })
				if f == nil {
					x.tc.Failf("only %d requests served (closed=%v)", served, closed)
					return
				}
				active = append(active, f.Stream)
				if len(active) > 1 {
					// Refuse all but the oldest.
					for _, id := range active[1:] {
						sc.writeRST(id, http2.ErrCodeRefusedStream)
					}
					active = active[:1]
				}
				// Serve the oldest.
				x.respond(sc, active[0], "ok")
				active = active[:0]
				served++
			}
			x.expectRequestOK("ok")
		},
	},
}
