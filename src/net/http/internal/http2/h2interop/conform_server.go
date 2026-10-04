// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http/internal/http2"
	"strconv"
	"strings"
	"time"
)

// This file has frame-level conformance tests of the Go server,
// complementing h2spec (which covers RFC 7540) with requirements of
// RFC 9113, RFC 9218, and RFC 8441, and with robustness checks. A
// scripted client (rawConn) sends the Go server valid and invalid
// frames, and each test checks the server's reaction. The tests are
// named conform/server/<name>.

type serverConfCase struct {
	name string
	// desc states the requirement being tested.
	desc string
	// settings are the client's initial SETTINGS.
	settings []http2.Setting
	timeout  time.Duration
	// profile is the Go server profile, if not the default.
	profile *goProfile
	// early, if non-nil, runs after the client preface is sent but
	// before the server's SETTINGS are read.
	early func(x *serverConfRun)
	// covers are the IDs of the requirements (see requirements.txt)
	// the case covers.
	covers string
	script func(x *serverConfRun)
}

type serverConfRun struct {
	tc             *testCtx
	c              *rawConn
	authority      string
	serverSettings map[uint16]uint32
}

func init() {
	for _, c := range serverConfCases {
		extraTests = append(extraTests, &Test{
			Name:    "conform/server/" + c.name,
			Desc:    c.desc,
			Timeout: c.timeout,
			Run: func(ctx context.Context, tc *testCtx) {
				runServerConf(ctx, tc, c)
			},
		})
	}
}

func runServerConf(ctx context.Context, tc *testCtx, c *serverConfCase) {
	tc.peerFindingsIgnored = true
	tc.Logf("requirement: %s", c.desc)
	profile := c.profile
	if profile == nil {
		profile = goProfiles[0]
	}
	addr, err := (&goServer{profile: profile}).Start(ctx, tc, "h2c")
	if err != nil {
		tc.Errorf("%v", err)
		return
	}
	tap, err := tc.Tap(addr, false, "test client", "Go server")
	if err != nil {
		tc.Errorf("%v", err)
		return
	}
	conn, err := net.Dial("tcp", tap.Addr())
	if err != nil {
		tc.Errorf("%v", err)
		return
	}
	x := &serverConfRun{tc: tc, authority: tap.Addr()}
	x.c = newRawConn(tc, "client", conn)
	if _, err := conn.Write([]byte(clientPreface)); err != nil {
		tc.Errorf("%v", err)
		return
	}
	x.c.writeSettings(c.settings...)
	x.c.startReading()
	if c.early != nil {
		c.early(x)
	}
	f, _ := x.c.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Type == ftSettings && f.Flags&flagAck == 0 })
	if f == nil {
		tc.Failf("server did not send SETTINGS")
		return
	}
	x.serverSettings = map[uint16]uint32{}
	for p := f.Payload; len(p) >= 6; p = p[6:] {
		x.serverSettings[binary.BigEndian.Uint16(p)] = binary.BigEndian.Uint32(p[2:])
	}
	x.c.writeSettingsAck()
	c.script(x)
}

// reqFields returns the pseudo-header fields for a request, followed
// by extra name, value pairs.
func (x *serverConfRun) reqFields(method, path string, extra ...string) []string {
	return append([]string{":method", method, ":scheme", "http", ":authority", x.authority, ":path", path}, extra...)
}

// response waits for the response field block on stream, returning
// the frame, or a RST_STREAM or GOAWAY if one comes first.
func (x *serverConfRun) response(stream uint32, timeout time.Duration) (f *rawFrame, closed bool) {
	return x.c.waitFor(timeout, func(f *rawFrame) bool {
		if f.Type == ftGoAway {
			// A graceful GOAWAY may precede the response.
			return f.ErrCode() != http2.ErrCodeNo
		}
		if f.Stream != stream {
			return false
		}
		if f.Type == ftRSTStream {
			return true
		}
		if f.Fields != nil && f.Status() != "" && !strings.HasPrefix(f.Status(), "1") {
			return true
		}
		return false
	})
}

// expectMalformed checks that the server treats the request on
// stream as malformed (RFC 9113 §8.1.1): a stream error of type
// PROTOCOL_ERROR, optionally preceded by a 4xx response.
func (x *serverConfRun) expectMalformed(stream uint32) {
	f, closed := x.response(stream, reactionTimeout)
	switch {
	case f == nil && closed:
		x.tc.Warnf("connection closed; want stream error PROTOCOL_ERROR")
	case f == nil:
		x.tc.Failf("no response or stream error within %v", reactionTimeout)
	case f.Type == ftRSTStream && f.ErrCode() == http2.ErrCodeProtocol:
		x.tc.Logf("got expected stream error: %v", f)
	case f.Type == ftGoAway && f.ErrCode() == http2.ErrCodeProtocol:
		x.tc.Logf("got connection error: %v", f)
	case f.Type == ftHeaders && strings.HasPrefix(f.Status(), "4"):
		// RFC 9113 §8.1.1: "For malformed requests, a server MAY
		// send an HTTP response prior to closing or resetting the
		// stream."
		x.tc.Logf("got %s response to malformed request (permitted by RFC 9113 §8.1.1)", f.Status())
	default:
		x.tc.Failf("got %v; the server accepted a malformed request", f)
	}
}

// expectStatus checks that the server responds on stream with status.
func (x *serverConfRun) expectStatus(stream uint32, status int) *rawFrame {
	f, closed := x.response(stream, reactionTimeout)
	switch {
	case f == nil:
		x.tc.Failf("no response on stream %d (closed=%v)", stream, closed)
	case f.Fields == nil:
		x.tc.Failf("got %v; want :status %d", f, status)
	case f.Status() != strconv.Itoa(status):
		x.tc.Failf("got :status %s; want %d", f.Status(), status)
	default:
		return f
	}
	return nil
}

// readBody reads the response body on stream until END_STREAM,
// granting flow control credit if grant is set.
func (x *serverConfRun) readBody(stream uint32, grant bool, timeout time.Duration) ([]byte, bool) {
	var body []byte
	for {
		f, _ := x.c.waitFor(timeout, func(f *rawFrame) bool {
			return f.Stream == stream && (f.Type == ftData || f.Type == ftRSTStream || f.Fields != nil) || f.Type == ftGoAway
		})
		if f == nil {
			return body, false
		}
		switch {
		case f.Type == ftData:
			body = append(body, f.Payload...)
			if grant && len(f.Payload) > 0 {
				x.c.writeWindowUpdate(0, uint32(len(f.Payload)))
				x.c.writeWindowUpdate(stream, uint32(len(f.Payload)))
			}
			if f.Flags&flagEndStream != 0 {
				return body, true
			}
		case f.Fields != nil && f.EndStream:
			return body, true
		case f.Type == ftRSTStream || f.Type == ftGoAway:
			x.tc.Failf("while reading body: %v", f)
			return body, false
		}
	}
}

func malformedRequest(extra ...string) func(x *serverConfRun) {
	return func(x *serverConfRun) {
		x.c.writeFields(1, true, x.reqFields("GET", "/hello", extra...)...)
		x.expectMalformed(1)
	}
}

func malformedRequestFields(fields func(x *serverConfRun) []string) func(x *serverConfRun) {
	return func(x *serverConfRun) {
		x.c.writeFields(1, true, fields(x)...)
		x.expectMalformed(1)
	}
}

var serverConfCases = []*serverConfCase{
	// Field validity (RFC 9113 §8.2.1). h2spec covers uppercase names
	// and connection-specific fields, from RFC 7540.
	{
		name:   "request-field-value-leading-space",
		covers: "9113-8.2.1-6 9113-8.2.1-7",
		desc:   "RFC 9113 §8.2.1: a field value MUST NOT start with whitespace; such a request is malformed",
		script: malformedRequest("x-a", " a"),
	},
	{
		name:   "request-field-value-trailing-tab",
		covers: "9113-8.2.1-6",
		desc:   "RFC 9113 §8.2.1: a field value MUST NOT end with whitespace; such a request is malformed",
		script: malformedRequest("x-a", "a\t"),
	},
	{
		name:   "request-field-value-crlf",
		covers: "9113-8.1.1-3 9113-8.2.1-2 9113-8.2.1-5 9113-8.2.1-7 9113-8.2.1-9",
		desc:   "RFC 9113 §8.2.1: a field value MUST NOT contain CR or LF; such a request is malformed",
		script: malformedRequest("x-a", "a\r\nx-injected: 1"),
	},
	{
		name:   "request-field-value-nul",
		covers: "9113-8.2.1-5",
		desc:   "RFC 9113 §8.2.1: a field value MUST NOT contain NUL; such a request is malformed",
		script: malformedRequest("x-a", "a\x00b"),
	},
	{
		name:   "request-field-name-space",
		covers: "9113-8.2.1-1 9113-8.2.1-2 9113-8.2.1-3",
		desc:   "RFC 9113 §8.2.1: a field name MUST NOT contain SP; such a request is malformed",
		script: malformedRequest("x a", "1"),
	},
	{
		name:   "request-field-name-colon",
		covers: "9113-8.2.1-4",
		desc:   "RFC 9113 §8.2.1: field names MUST NOT include a colon except for pseudo-headers; such a request is malformed",
		script: malformedRequest("x:a", "1"),
	},
	{
		name:   "request-field-name-non-ascii",
		covers: "9113-8.2.1-1 9113-8.2.1-3",
		desc:   "RFC 9113 §8.2.1: a field name MUST NOT contain characters 0x7f-0xff; such a request is malformed",
		script: malformedRequest("x-caf\xc3\xa9", "1"),
	},
	{
		name: "request-field-value-obs-text",
		desc: "RFC 9113 §8.2.1: only NUL, CR, LF, and leading/trailing whitespace make a field value malformed; obs-text is permitted",
		script: func(x *serverConfRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/info", "x-a", "caf\xc3\xa9 \xff")...)
			x.expectStatus(1, 200)
		},
	},

	// Control data (RFC 9113 §8.3.1, §8.5, RFC 8441).
	{
		name:   "request-path-empty",
		covers: "9113-8.3.1-10",
		desc:   "RFC 9113 §8.3.1: :path MUST NOT be empty for http or https URIs",
		script: malformedRequestFields(func(x *serverConfRun) []string { return x.reqFields("GET", "") }),
	},
	{
		name:   "request-path-not-absolute",
		desc:   "RFC 9113 §8.3.1: :path for http or https URIs is an absolute path (or \"*\" for OPTIONS); otherwise the request is malformed",
		script: malformedRequestFields(func(x *serverConfRun) []string { return x.reqFields("GET", "hello") }),
	},
	{
		name:   "request-path-asterisk-get",
		desc:   "RFC 9113 §8.3.1: :path \"*\" is only valid for OPTIONS requests",
		script: malformedRequestFields(func(x *serverConfRun) []string { return x.reqFields("GET", "*") }),
	},
	{
		name:   "request-options-asterisk",
		covers: "9113-8.3.1-11",
		desc:   "RFC 9113 §8.3.1: an OPTIONS request with :path \"*\" is valid",
		script: func(x *serverConfRun) {
			x.c.writeFields(1, true, x.reqFields("OPTIONS", "*")...)
			f, _ := x.response(1, reactionTimeout)
			if f == nil || f.Fields == nil {
				x.tc.Failf("got %v; want a response", f)
			}
		},
	},
	{
		name:   "request-host-authority-mismatch",
		covers: "9113-8.3.1-1 9113-8.3.1-4",
		desc:   "RFC 9113 §8.3.1: a server SHOULD treat a request as malformed if its host field differs from :authority",
		script: func(x *serverConfRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/info", "host", "other.example")...)
			f, _ := x.response(1, reactionTimeout)
			if f != nil && f.Fields != nil && strings.HasPrefix(f.Status(), "2") {
				x.tc.Warnf("server accepted request with host %q and :authority %q (RFC 9113 §8.3.1 SHOULD)", "other.example", x.authority)
				body, _ := x.readBody(1, false, reactionTimeout)
				x.tc.Logf("handler saw: %s", body)
			}
		},
	},
	{
		name: "request-host-without-authority",
		desc: "RFC 9113 §8.3.1: a request may carry host instead of :authority; the server must use it",
		script: func(x *serverConfRun) {
			x.c.writeFields(1, true, ":method", "GET", ":scheme", "http", ":path", "/info", "host", "h.example")
			if x.expectStatus(1, 200) == nil {
				return
			}
			body, _ := x.readBody(1, false, reactionTimeout)
			var info InfoBody
			json.Unmarshal(body, &info)
			if info.Authority != "h.example" {
				x.tc.Failf("handler saw authority %q; want %q", info.Authority, "h.example")
			}
		},
	},
	{
		name:   "request-authority-userinfo",
		covers: "9113-8.3.1-9",
		desc:   "RFC 9113 §8.3.1: :authority MUST NOT include userinfo",
		script: malformedRequestFields(func(x *serverConfRun) []string {
			return []string{":method", "GET", ":scheme", "http", ":authority", "user@" + x.authority, ":path", "/hello"}
		}),
	},
	{
		name:   "connect-with-path",
		covers: "9113-8.5-1",
		desc:   "RFC 9113 §8.5: :scheme and :path MUST be omitted from CONNECT requests",
		script: malformedRequestFields(func(x *serverConfRun) []string {
			return []string{":method", "CONNECT", ":authority", "example.com:443", ":path", "/"}
		}),
	},
	{
		name: "connect-without-authority",
		desc: "RFC 9113 §8.5: a CONNECT request MUST include :authority",
		script: malformedRequestFields(func(x *serverConfRun) []string {
			return []string{":method", "CONNECT"}
		}),
	},
	{
		name: "protocol-without-setting",
		desc: "RFC 8441 §3: a CONNECT request with :protocol to a server that hasn't sent SETTINGS_ENABLE_CONNECT_PROTOCOL is malformed",
		script: func(x *serverConfRun) {
			if x.serverSettings[setEnableConnectProtocol] == 1 {
				x.tc.Skipf("server enables extended CONNECT")
				return
			}
			x.c.writeFields(1, false, ":method", "CONNECT", ":protocol", "websocket", ":scheme", "http", ":authority", x.authority, ":path", "/info")
			x.expectMalformed(1)
		},
	},
	{
		name:   "te-trailers",
		covers: "9113-8.2.2-3",
		desc:   "RFC 9113 §8.2.2: TE may be present in a request with the value \"trailers\"",
		script: func(x *serverConfRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/hello", "te", "trailers")...)
			x.expectStatus(1, 200)
		},
	},

	// Trailers, interim responses.
	{
		name: "request-trailers-undeclared",
		desc: "RFC 9110 §6.6.2: a sender SHOULD (not MUST) declare trailers in a Trailer field; undeclared request trailers should reach the handler, as in HTTP/1.1",
		script: func(x *serverConfRun) {
			x.c.writeFields(1, false, x.reqFields("POST", "/upload")...)
			x.c.writeData(1, false, []byte("body"))
			x.c.writeFields(1, true, "x-trailer", "value")
			if x.expectStatus(1, 200) == nil {
				return
			}
			body, _ := x.readBody(1, false, reactionTimeout)
			var u UploadBody
			json.Unmarshal(body, &u)
			if got := u.Trailer["x-trailer"]; len(got) != 1 || got[0] != "value" {
				x.tc.Failf("handler saw request trailers %v; want x-trailer=value (response: %s)", u.Trailer, body)
			}
		},
	},
	{
		name: "request-trailers-declared",
		desc: "RFC 9110 §6.5: declared request trailers reach the handler",
		script: func(x *serverConfRun) {
			x.c.writeFields(1, false, x.reqFields("POST", "/upload", "trailer", "x-trailer")...)
			x.c.writeData(1, false, []byte("body"))
			x.c.writeFields(1, true, "x-trailer", "value")
			if x.expectStatus(1, 200) == nil {
				return
			}
			body, _ := x.readBody(1, false, reactionTimeout)
			var u UploadBody
			json.Unmarshal(body, &u)
			if got := u.Trailer["x-trailer"]; len(got) != 1 || got[0] != "value" {
				x.tc.Failf("handler saw request trailers %v; want x-trailer=value", u.Trailer)
			}
		},
	},
	{
		name: "expect-100-continue",
		desc: "RFC 9110 §10.1.1: a server receiving expect: 100-continue sends 100 (Continue) before reading the body",
		script: func(x *serverConfRun) {
			x.c.writeFields(1, false, x.reqFields("POST", "/upload", "expect", "100-continue", "content-length", "4")...)
			f, _ := x.c.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Stream == 1 && f.Fields != nil })
			if f == nil || f.Status() != "100" {
				x.tc.Failf("got %v; want 100 Continue", f)
				return
			}
			x.c.writeData(1, true, []byte("body"))
			x.expectStatus(1, 200)
		},
	},

	// Flow control by the server as a sender.
	{
		name:     "initial-window-zero",
		covers:   "9113-5.2.1-2 9113-6.5-2 9113-6.9-1 9113-6.9.2-1 9113-5.2.1-d1",
		desc:     "RFC 9113 §6.9.2: a sender MUST NOT send DATA beyond a zero initial window, and MUST adjust windows when SETTINGS_INITIAL_WINDOW_SIZE changes",
		settings: []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 0}},
		script: func(x *serverConfRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/bytes/1000")...)
			x.expectStatus(1, 200)
			if f, _ := x.c.waitFor(300*time.Millisecond, func(f *rawFrame) bool { return f.Type == ftData && len(f.Payload) > 0 }); f != nil {
				x.tc.Failf("server sent DATA with a zero window")
			}
			x.c.writeSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 65535})
			body, ok := x.readBody(1, false, reactionTimeout)
			if !ok || len(body) != 1000 {
				x.tc.Failf("got %d body bytes (complete=%v) after raising the window; want 1000", len(body), ok)
			}
		},
	},
	{
		name:   "initial-window-decrease",
		covers: "9113-6.5.3-1 9113-6.9.2-1 9113-6.9.2-2",
		desc:   "RFC 9113 §6.9.2: a sender MUST track negative flow control windows after SETTINGS_INITIAL_WINDOW_SIZE decreases",
		script: func(x *serverConfRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/bytes/1000000")...)
			x.expectStatus(1, 200)
			// Read part of the initial window, shrink it, then
			// read the rest while granting credit. The validator
			// checks that the server never exceeds the window.
			n := 0
			for n < 30000 {
				f, _ := x.c.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Type == ftData })
				if f == nil {
					x.tc.Failf("stalled after %d bytes", n)
					return
				}
				n += len(f.Payload)
			}
			x.c.writeSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 1000})
			x.c.writeWindowUpdate(0, 1<<20)
			x.c.writeWindowUpdate(1, 100000)
			body, ok := x.readBody(1, true, 5*time.Second)
			if !ok || n+len(body) != 1000000 {
				x.tc.Failf("got %d body bytes (complete=%v); want 1000000", n+len(body), ok)
			}
		},
	},
	{
		name:     "header-table-size-zero",
		covers:   "9113-4.3.1-1 9113-6.5-2 7541-4.2-2",
		desc:     "RFC 7541 §4.2: after acknowledging SETTINGS_HEADER_TABLE_SIZE=0, the encoder MUST signal the change at the start of the next field block",
		settings: []http2.Setting{{ID: http2.SettingHeaderTableSize, Val: 0}},
		script: func(x *serverConfRun) {
			x.c.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Type == ftSettings && f.Flags&flagAck != 0 })
			for i := uint32(1); i <= 5; i += 2 {
				x.c.writeFields(i, true, x.reqFields("GET", "/hello")...)
				x.expectStatus(i, 200)
			}
			// The validator checks the encoding; see decodeFields.
		},
	},

	// Behavior before the client has received the server's SETTINGS.
	{
		name:    "early-data-before-settings",
		covers:  "9113-6.9.3-1 9113-3.4-d2",
		desc:    "RFC 9113 §6.9.2: until it receives SETTINGS_INITIAL_WINDOW_SIZE, a client can use the default 65535-byte window; the server must accept that data even if it advertises a smaller window",
		profile: goProfiles[1], // constrained: 16KB stream receive window
		early: func(x *serverConfRun) {
			x.c.writeFields(1, false, x.reqFields("POST", "/upload", "content-length", "65535")...)
			for sent := 0; sent < 65535; sent += 16383 {
				x.c.writeData(1, sent+16383 >= 65535, make([]byte, min(16383, 65535-sent)))
			}
		},
		script: func(x *serverConfRun) {
			x.c.writeWindowUpdate(0, 1<<20)
			f, closed := x.response(1, reactionTimeout)
			switch {
			case f == nil:
				x.tc.Failf("no response (closed=%v)", closed)
			case f.Type == ftRSTStream && f.ErrCode() == http2.ErrCodeFlowControl:
				// RFC 9113 §6.9.3: "The receiver MAY instead send a
				// RST_STREAM with an error code of FLOW_CONTROL_ERROR
				// for the affected streams."
				x.tc.Warnf("server reset the stream with FLOW_CONTROL_ERROR, which RFC 9113 §6.9.3 permits, but the request fails: clients don't retry it")
			case f.Fields != nil && f.Status() == "200":
				body, _ := x.readBody(1, true, reactionTimeout)
				x.tc.Logf("response: %s", body)
			default:
				x.tc.Failf("got %v; want a 200 response or (permitted) RST_STREAM FLOW_CONTROL_ERROR", f)
			}
		},
	},
	{
		name:    "hpack-before-settings",
		covers:  "9113-3.4-d2 9113-4.3.1-d1",
		desc:    "RFC 7541 §4.2, RFC 9113 §6.5.3: until it acknowledges a smaller SETTINGS_HEADER_TABLE_SIZE, an encoder may use the default 4096-byte dynamic table; the server's decoder must accept that",
		profile: goProfiles[1], // constrained: MaxDecoderHeaderTableSize 256
		early: func(x *serverConfRun) {
			// Each field block inserts about 1KB of fields into the
			// dynamic table (the encoder uses incremental indexing),
			// and later blocks reference them.
			for i := uint32(1); i <= 5; i += 2 {
				x.c.writeFields(i, true, x.reqFields("GET", "/hello",
					"x-a", strings.Repeat("a", 300), "x-b", strings.Repeat("b", 300), "x-c", strings.Repeat("c", 300))...)
			}
		},
		script: func(x *serverConfRun) {
			for i := uint32(1); i <= 5; i += 2 {
				x.expectStatus(i, 200)
			}
		},
	},

	// RFC 9218 priorities.
	{
		name:   "priority-update-before-request",
		covers: "9218-7-2",
		desc:   "RFC 9218 §7.1: a client MAY send PRIORITY_UPDATE for a stream before opening it",
		script: func(x *serverConfRun) {
			x.c.writeRaw(ftPriorityUpdate, 0, 0, append(binary.BigEndian.AppendUint32(nil, 1), "u=1, i"...))
			x.c.writeFields(1, true, x.reqFields("GET", "/hello")...)
			x.expectStatus(1, 200)
		},
	},
	{
		name:   "priority-update-on-stream",
		covers: "9218-7.1-2",
		desc:   "RFC 9218 §7.1: PRIORITY_UPDATE with a non-zero stream ID MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: func(x *serverConfRun) {
			x.c.writeRaw(ftPriorityUpdate, 0, 1, append(binary.BigEndian.AppendUint32(nil, 1), "u=1"...))
			x.c.expectConnError(http2.ErrCodeProtocol)
		},
	},
	{
		name:   "priority-update-stream-zero",
		covers: "9218-7.1-9",
		desc:   "RFC 9218 §7.1: PRIORITY_UPDATE with a prioritized stream ID of 0 MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: func(x *serverConfRun) {
			x.c.writeRaw(ftPriorityUpdate, 0, 0, append(binary.BigEndian.AppendUint32(nil, 0), "u=1"...))
			x.c.expectConnError(http2.ErrCodeProtocol)
		},
	},
	{
		name: "priority-header",
		desc: "RFC 9218 §5: the priority request header is accepted",
		script: func(x *serverConfRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/hello", "priority", "u=0, i")...)
			x.expectStatus(1, 200)
		},
	},
	{
		name:   "settings-no-rfc7540-priorities-invalid",
		covers: "9218-2.1-2",
		desc:   "RFC 9218 §2.1: SETTINGS_NO_RFC7540_PRIORITIES other than 0 or 1 MUST be treated as a connection error of type PROTOCOL_ERROR",
		script: func(x *serverConfRun) {
			x.c.writeRaw(ftSettings, 0, 0, settingsPayload(setNoRFC7540Priorities, 2))
			x.c.expectConnError(http2.ErrCodeProtocol)
		},
	},

	// Robustness. These check that the server limits the resources a
	// misbehaving client can consume.
	{
		name:    "continuation-flood",
		covers:  "9113-10.5-2",
		desc:    "RFC 9113 §10.5.1: a server must bound the field block size it buffers; endless CONTINUATION frames must not be accepted indefinitely",
		timeout: 30 * time.Second,
		script: func(x *serverConfRun) {
			x.c.writeHeaders(1, true, false, x.c.block(x.reqFields("GET", "/hello")...))
			chunk := x.c.block("x-flood", strings.Repeat("a", 16000))
			const limit = 64 << 20
			sent := 0
			x.c.conn.SetWriteDeadline(time.Now().Add(20 * time.Second))
			for sent < limit {
				if len(x.c.seen(func(f *rawFrame) bool { return f.Type == ftGoAway || f.Fields != nil })) > 0 {
					break
				}
				x.c.wmu.Lock()
				err := x.c.fr.WriteContinuation(1, false, chunk)
				x.c.wmu.Unlock()
				if err != nil {
					x.tc.Logf("write failed after %d bytes: %v", sent, err)
					break
				}
				sent += len(chunk)
			}
			fs := x.c.seen(func(f *rawFrame) bool { return f.Type == ftGoAway || f.Fields != nil })
			switch {
			case len(fs) > 0:
				x.tc.Logf("server reacted after %d bytes of CONTINUATION: %v", sent, fs[0])
			case sent < limit:
				x.tc.Logf("server closed the connection after %d bytes of CONTINUATION", sent)
			default:
				x.tc.Failf("server accepted %d bytes of CONTINUATION frames without reacting", sent)
			}
		},
	},
	{
		name:    "empty-continuation-flood",
		covers:  "9113-10.5-2",
		desc:    "RFC 9113 §10.5: a server must bound the CPU a client can consume; endless empty CONTINUATION frames must not be accepted indefinitely",
		timeout: 30 * time.Second,
		script: func(x *serverConfRun) {
			x.c.writeHeaders(1, true, false, x.c.block(x.reqFields("GET", "/hello")...))
			const limit = 1000000
			x.c.conn.SetWriteDeadline(time.Now().Add(20 * time.Second))
			n := 0
			for n < limit {
				x.c.wmu.Lock()
				err := x.c.fr.WriteContinuation(1, false, nil)
				x.c.wmu.Unlock()
				if err != nil {
					x.tc.Logf("write failed after %d frames: %v", n, err)
					break
				}
				n++
				if n%10000 == 0 && len(x.c.seen(isType(ftGoAway))) > 0 {
					break
				}
			}
			if n >= limit && len(x.c.seen(isType(ftGoAway))) == 0 {
				x.tc.Warnf("server accepted %d empty CONTINUATION frames without reacting", n)
			} else {
				x.tc.Logf("server reacted after %d empty CONTINUATION frames", n)
			}
		},
	},
	{
		name:    "rapid-reset",
		covers:  "9113-10.5-2",
		desc:    "RFC 9113 §10.5: a server should protect itself from clients that rapidly open and reset streams (CVE-2023-44487)",
		timeout: 30 * time.Second,
		script: func(x *serverConfRun) {
			const n = 20000
			blk := x.c.block(x.reqFields("GET", "/delay/1000")...)
			x.c.conn.SetWriteDeadline(time.Now().Add(20 * time.Second))
			for i := range n {
				id := uint32(2*i + 1)
				x.c.wmu.Lock()
				err := x.c.fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: blk, EndStream: true, EndHeaders: true})
				if err == nil {
					err = x.c.fr.WriteRSTStream(id, http2.ErrCodeCancel)
				}
				x.c.wmu.Unlock()
				if err != nil {
					x.tc.Logf("write failed after %d streams: %v", i, err)
					break
				}
			}
			if g := x.c.seen(isType(ftGoAway)); len(g) > 0 {
				x.tc.Logf("server sent %v", g[0])
				return
			}
			// The connection should still work, or have been closed.
			id := uint32(2*n + 1)
			x.c.writeFields(id, true, x.reqFields("GET", "/hello")...)
			f, closed := x.response(id, 10*time.Second)
			x.tc.Logf("after %d rapid resets: response %v (closed=%v)", n, f, closed)
		},
	},
}
