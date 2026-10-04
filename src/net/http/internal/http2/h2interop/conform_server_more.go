// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/internal/http2"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// This file has more frame-level conformance tests of the Go server,
// for requirements that need control over the server: a custom
// handler, access to the *http.Server (to shut it down or close it),
// or a server process with a different GODEBUG setting. The tests are
// named conform/server/<name>, like those in conform_server.go.

// smCase is a server conformance case run by runSMConf.
type smCase struct {
	// serverConfCase holds the name, description, covered
	// requirements, client settings, timeout, and server profile.
	// Its early and script fields are unused.
	serverConfCase

	// handler, if non-nil, replaces the route set as the handler of
	// the Go server.
	handler http.Handler
	// configure, if non-nil, adjusts the Go server before it starts.
	configure func(*http.Server)
	// godebug, if non-empty, runs the Go server with the route set
	// in a subprocess with this GODEBUG setting. Then handler and
	// configure are ignored, and smRun.srv is nil.
	godebug string

	early  func(x *smRun)
	script func(x *smRun)
}

// smRun is the state of a running smCase.
type smRun struct {
	*serverConfRun
	srv *http.Server // nil if the server runs in a subprocess

	// connSendWin is the client's connection-level send window,
	// as far as the client has seen WINDOW_UPDATE frames.
	connSendWin int64
	pingSeq     byte
}

func init() {
	for _, c := range smCases {
		serverConfCases = append(serverConfCases, &c.serverConfCase)
		extraTests = append(extraTests, &Test{
			Name:    "conform/server/" + c.name,
			Desc:    c.desc,
			Timeout: c.timeout,
			Run: func(ctx context.Context, tc *testCtx) {
				runSMConf(ctx, tc, c)
			},
		})
	}
}

// startSMServer starts a Go server for the test, like goServer.Start,
// but with an optional custom handler and configuration, returning
// the *http.Server.
func startSMServer(tc *testCtx, profile *goProfile, handler http.Handler, configure func(*http.Server)) (*http.Server, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	if handler == nil {
		handler = routeHandler()
	}
	cfg := profile.HTTP2
	cfg.CountError = func(token string) { tc.countError("Go server", token) }
	srv := &http.Server{
		Handler:  handler,
		HTTP2:    &cfg,
		ErrorLog: log.New(&logWriter{tc: tc, prefix: "Go server log: "}, "", 0),
	}
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	srv.Protocols.SetUnencryptedHTTP2(true)
	if configure != nil {
		configure(srv)
	}
	go srv.Serve(ln)
	tc.Cleanup(func() { srv.Close() })
	return srv, ln.Addr().String(), nil
}

// startSMSubprocess runs the Go server with the route set (h2c, see
// serveForDebugging) in a child process with the given GODEBUG
// setting, which is process-wide and read at init time.
func startSMSubprocess(ctx context.Context, tc *testCtx, godebug string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	ln.Close()
	if v := os.Getenv("GODEBUG"); v != "" {
		godebug = v + "," + godebug // later settings take precedence
	}
	cmd := exec.CommandContext(ctx, exe, "-serve", "h2c", "-addr", addr)
	cmd.Env = append(os.Environ(), "GODEBUG="+godebug)
	lw := &logWriter{tc: tc, prefix: "Go server subprocess: "}
	cmd.Stdout = lw
	cmd.Stderr = lw
	tc.Logf("starting %s -serve h2c -addr %s with GODEBUG=%s", exe, addr, godebug)
	if err := cmd.Start(); err != nil {
		return "", err
	}
	tc.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	for range 200 {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return addr, nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return "", fmt.Errorf("server subprocess didn't listen on %s", addr)
}

func runSMConf(ctx context.Context, tc *testCtx, c *smCase) {
	tc.peerFindingsIgnored = true
	tc.Logf("requirement: %s", c.desc)
	profile := c.profile
	if profile == nil {
		profile = goProfiles[0]
	}
	var (
		srv  *http.Server
		addr string
		err  error
	)
	if c.godebug != "" {
		addr, err = startSMSubprocess(ctx, tc, c.godebug)
	} else {
		srv, addr, err = startSMServer(tc, profile, c.handler, c.configure)
	}
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
	x := &smRun{
		serverConfRun: &serverConfRun{tc: tc, authority: tap.Addr()},
		srv:           srv,
		connSendWin:   65535,
	}
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

// streamWindow returns the server's initial stream receive window.
func (x *smRun) streamWindow() int {
	if v, ok := x.serverSettings[setInitialWindowSize]; ok {
		return int(v)
	}
	return 65535
}

// sendData writes a DATA frame, accounting for it in x.connSendWin.
func (x *smRun) sendData(stream uint32, endStream bool, data []byte) {
	x.connSendWin -= int64(len(data))
	x.c.writeData(stream, endStream, data)
}

// awaitConnCredit waits until the client's connection-level send
// window is at least need, consuming connection-level WINDOW_UPDATE
// frames from the server. It reports whether it got there.
func (x *smRun) awaitConnCredit(need int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for x.connSendWin < need {
		d := time.Until(deadline)
		if d <= 0 {
			return false
		}
		f, _ := x.c.waitFor(d, func(f *rawFrame) bool { return f.Type == ftWindowUpdate && f.Stream == 0 })
		if f == nil {
			return false
		}
		if len(f.Payload) == 4 {
			x.connSendWin += int64(binary.BigEndian.Uint32(f.Payload) & (1<<31 - 1))
		}
	}
	return true
}

// pingRoundTrip sends a PING and waits for its ACK. Once it returns
// true, the server has processed every frame sent before the PING.
// A connection error that arrives first fails the test.
func (x *smRun) pingRoundTrip() bool {
	x.pingSeq++
	data := [8]byte{'s', 'm', 'p', 'i', 'n', 'g', 0, x.pingSeq}
	x.c.writePing(false, data)
	f, closed := x.c.waitFor(reactionTimeout, func(f *rawFrame) bool {
		return f.Type == ftGoAway && f.ErrCode() != http2.ErrCodeNo ||
			f.Type == ftPing && f.Flags&flagAck != 0 && bytes.Equal(f.Payload, data[:])
	})
	switch {
	case f == nil:
		x.tc.Failf("no PING ACK within %v (closed=%v)", reactionTimeout, closed)
		return false
	case f.Type == ftGoAway:
		x.tc.Failf("unexpected connection error: %v", f)
		return false
	}
	return true
}

// awaitClose reads frames until the connection closes or timeout
// passes, answering PINGs, and returns the GOAWAY frames received
// (including any received earlier and not yet consumed).
func (x *smRun) awaitClose(timeout time.Duration) (goaways []*rawFrame, closed bool) {
	deadline := time.Now().Add(timeout)
	for {
		d := time.Until(deadline)
		if d <= 0 {
			return goaways, false
		}
		f, closed := x.c.waitFor(d, func(f *rawFrame) bool {
			return f.Type == ftGoAway || f.Type == ftPing && f.Flags&flagAck == 0
		})
		switch {
		case closed:
			return goaways, true
		case f == nil:
			return goaways, false
		case f.Type == ftPing:
			if len(f.Payload) == 8 {
				x.c.writePing(true, [8]byte(f.Payload))
			}
		default:
			goaways = append(goaways, f)
		}
	}
}

// readBodyGraceful is like readBody without granting credit, but
// tolerates graceful (NO_ERROR) GOAWAY frames.
func (x *smRun) readBodyGraceful(stream uint32, timeout time.Duration) ([]byte, bool) {
	var body []byte
	for {
		f, _ := x.c.waitFor(timeout, func(f *rawFrame) bool {
			return f.Stream == stream && (f.Type == ftData || f.Type == ftRSTStream || f.Fields != nil) ||
				f.Type == ftGoAway && f.ErrCode() != http2.ErrCodeNo
		})
		switch {
		case f == nil:
			return body, false
		case f.Type == ftData:
			body = append(body, f.Payload...)
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

// smGoAwayLastStream returns the last-stream-ID of a GOAWAY frame.
func smGoAwayLastStream(f *rawFrame) uint32 {
	if len(f.Payload) < 4 {
		return 0
	}
	return binary.BigEndian.Uint32(f.Payload) & (1<<31 - 1)
}

// checkGoAwayBeforeClose checks that the server sent GOAWAY before
// closing the connection, after the event described by what. It
// returns the GOAWAY frames.
func (x *smRun) checkGoAwayBeforeClose(what string, timeout time.Duration, prior ...*rawFrame) []*rawFrame {
	goaways, closed := x.awaitClose(timeout)
	goaways = append(prior, goaways...)
	for _, g := range goaways {
		x.tc.Logf("GOAWAY after %s: %v", what, g)
	}
	switch {
	case len(goaways) == 0 && closed:
		x.tc.Warnf("Go server closed the connection after %s without sending GOAWAY (RFC 9113 §5.4.1, §6.8, §9.1: SHOULD send GOAWAY before closing a connection)", what)
	case len(goaways) == 0:
		x.tc.Warnf("Go server sent no GOAWAY within %v after %s", timeout, what)
	case !closed:
		x.tc.Logf("connection still open %v after %s", timeout, what)
	default:
		x.tc.Logf("connection closed after GOAWAY")
	}
	return goaways
}

// upload sends a POST /upload request with an n-byte body on stream,
// waiting for connection flow control credit as needed, and checks
// the response. The body must fit in the server's initial stream
// window.
func (x *smRun) upload(stream uint32, n int) {
	x.c.writeFields(stream, n == 0, x.reqFields("POST", "/upload", "content-length", strconv.Itoa(n))...)
	body := Pattern(n)
	for len(body) > 0 {
		c := min(len(body), 16384, x.streamWindow())
		if !x.awaitConnCredit(int64(c), reactionTimeout) {
			x.tc.Failf("stream %d: no connection flow control credit to send %d more body bytes (window %d); the connection is stalled", stream, len(body), x.connSendWin)
			return
		}
		x.sendData(stream, c == len(body), body[:c])
		body = body[c:]
	}
	if x.expectStatus(stream, 200) == nil {
		return
	}
	b, _ := x.readBody(stream, false, reactionTimeout)
	var u UploadBody
	json.Unmarshal(b, &u)
	if u.Len != int64(n) {
		x.tc.Failf("stream %d: handler read %d body bytes; want %d (response: %s)", stream, u.Len, n, b)
	}
}

// HPACK encoding helpers, for field blocks that hpack.Encoder won't
// produce.

// smHpackInt appends an HPACK integer (RFC 7541 §5.1) with an n-bit
// prefix, ORing flags into the first octet.
func smHpackInt(b []byte, n uint, flags byte, v uint64) []byte {
	max := uint64(1)<<n - 1
	if v < max {
		return append(b, flags|byte(v))
	}
	b = append(b, flags|byte(max))
	v -= max
	for v >= 128 {
		b = append(b, byte(v%128)|0x80)
		v /= 128
	}
	return append(b, byte(v))
}

// smHpackLiteral appends a literal field with a literal name and no
// Huffman coding (RFC 7541 §6.2), with incremental indexing if index
// is set, and without indexing otherwise.
func smHpackLiteral(b []byte, index bool, name, value string) []byte {
	if index {
		b = append(b, 0x40)
	} else {
		b = append(b, 0x00)
	}
	b = smHpackInt(b, 7, 0, uint64(len(name)))
	b = append(b, name...)
	b = smHpackInt(b, 7, 0, uint64(len(value)))
	return append(b, value...)
}

// smHpackRequest appends literal (not indexed) representations of the
// pseudo-header fields of a GET request.
func (x *smRun) smHpackRequest(b []byte, path string) []byte {
	for _, kv := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":authority", x.authority}, {":path", path}} {
		b = smHpackLiteral(b, false, kv[0], kv[1])
	}
	return b
}

// smLeadingSizeUpdates returns the dynamic table size updates at the
// start of an HPACK field block.
func smLeadingSizeUpdates(block []byte) []uint64 {
	var ups []uint64
	for len(block) > 0 && block[0]&0xe0 == 0x20 {
		v, n, ok := hpackReadInt(block, 5)
		if !ok {
			break
		}
		ups = append(ups, v)
		block = block[n:]
	}
	return ups
}

// smConnSpecificHandler sets the response field named by the "name"
// query parameter to the "value" query parameter.
func smConnSpecificHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(r.URL.Query().Get("name"), r.URL.Query().Get("value"))
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "ok\n")
	})
}

func smConnSpecificCase(name, value string) *smCase {
	return &smCase{
		serverConfCase: serverConfCase{
			name:   "response-connection-specific-" + strings.ToLower(name),
			covers: "9113-8.2.2-1",
			desc:   fmt.Sprintf("RFC 9113 §8.2.2: an endpoint MUST NOT generate an HTTP/2 message containing connection-specific header fields; the server must not send %s: %s set by a handler", name, value),
		},
		handler: smConnSpecificHandler(),
		script: func(x *smRun) {
			q := "/?name=" + name + "&value=" + strings.ReplaceAll(value, " ", "%20")
			x.c.writeFields(1, true, x.reqFields("GET", q)...)
			f := x.expectStatus(1, 200)
			if f == nil {
				return
			}
			for _, hf := range f.Fields {
				if hf.Name == strings.ToLower(name) {
					x.tc.Failf("Go server sent connection-specific response field %q: %q set by the handler (RFC 9113 §8.2.2: MUST NOT)", hf.Name, hf.Value)
				}
			}
			x.readBody(1, false, reactionTimeout)
		},
	}
}

// smConnectHandler answers CONNECT requests with 200 and then reads
// the tunnel's bytes until the client ends the stream.
func smConnectHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			http.Error(w, "want CONNECT", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(200)
		http.NewResponseController(w).Flush()
		io.Copy(io.Discard, r.Body)
	})
}

func smConnectTunnelHeadersCase(name string, endStream bool) *smCase {
	return &smCase{
		serverConfCase: serverConfCase{
			name:   name,
			covers: "9113-8.5-2",
			desc:   fmt.Sprintf("RFC 9113 §8.5: frame types other than DATA, RST_STREAM, WINDOW_UPDATE, and PRIORITY MUST be treated as a stream error if received on a connected (CONNECT) stream; here a HEADERS frame (END_STREAM=%v)", endStream),
		},
		handler: smConnectHandler(),
		script: func(x *smRun) {
			x.c.writeFields(1, false, ":method", "CONNECT", ":authority", "tunnel.example:443")
			if x.expectStatus(1, 200) == nil {
				return
			}
			x.sendData(1, false, []byte("tunnel bytes"))
			x.c.writeFields(1, endStream, "x-a", "1")
			f, closed := x.c.waitFor(reactionTimeout, func(f *rawFrame) bool {
				if f.Type == ftGoAway {
					return true
				}
				return f.Stream == 1 && (f.Type == ftRSTStream ||
					f.Type == ftData && f.Flags&flagEndStream != 0 ||
					f.Fields != nil && f.EndStream)
			})
			switch {
			case f == nil:
				x.tc.Failf("no stream error within %v (closed=%v); want RST_STREAM on the connected stream", reactionTimeout, closed)
			case f.Type == ftRSTStream:
				x.tc.Logf("got expected stream error: %v", f)
			case f.Type == ftGoAway && f.ErrCode() != http2.ErrCodeNo:
				// RFC 9113 §5.4.1: an endpoint MAY treat a stream error
				// as a connection error.
				x.tc.Logf("got connection error: %v", f)
			default:
				x.tc.Failf("Go server accepted a HEADERS frame on a connected CONNECT stream as trailers and ended the stream normally (%v); want a stream error (RFC 9113 §8.5: MUST be treated as a stream error)", f)
			}
		},
	}
}

// priorityOrder runs a test of response scheduling: after exhausting
// the connection window, it requests two 50000-byte responses with
// the given priority fields on streams 3 and 5, opens the window
// for both, and checks that the response on stream want completes
// first, without interleaving once its DATA starts.
func (x *smRun) priorityOrder(prio3, prio5 string, want uint32, rule string) {
	const n = 50000
	x.c.writeFields(1, true, x.reqFields("GET", "/bytes/65535")...)
	if x.expectStatus(1, 200) == nil {
		return
	}
	// The client's connection window is now used up.
	if b, ok := x.readBody(1, false, reactionTimeout); !ok || len(b) != 65535 {
		x.tc.Errorf("got %d bytes of the 65535-byte response (complete=%v)", len(b), ok)
		return
	}
	x.c.writeFields(3, true, x.reqFields("GET", "/bytes/"+strconv.Itoa(n), "priority", prio3)...)
	x.c.writeFields(5, true, x.reqFields("GET", "/bytes/"+strconv.Itoa(n), "priority", prio5)...)
	if x.expectStatus(3, 200) == nil || x.expectStatus(5, 200) == nil {
		return
	}
	// Both handlers are now blocked writing their bodies, which the
	// server queues once the response HEADERS are written.
	if !x.pingRoundTrip() {
		return
	}
	x.c.writeWindowUpdate(0, 2*n)
	other := uint32(3)
	if want == 3 {
		other = 5
	}
	var order []string
	got := map[uint32]int{}
	done := map[uint32]bool{}
	var firstDone uint32
	interleaved := false
	for !done[3] || !done[5] {
		f, _ := x.c.waitFor(reactionTimeout, func(f *rawFrame) bool {
			return f.Type == ftData && (f.Stream == 3 || f.Stream == 5) || f.Type == ftGoAway || f.Type == ftRSTStream
		})
		if f == nil || f.Type != ftData {
			x.tc.Failf("while reading responses: got %v (have %v)", f, got)
			return
		}
		if len(f.Payload) > 0 {
			order = append(order, fmt.Sprintf("%d:%d", f.Stream, len(f.Payload)))
		}
		if f.Stream == other && got[want] > 0 && got[want] < n && len(f.Payload) > 0 {
			interleaved = true
		}
		got[f.Stream] += len(f.Payload)
		// A response is complete once all its bytes arrived; the
		// END_STREAM flag may come later in an empty DATA frame.
		if got[f.Stream] >= n && firstDone == 0 {
			firstDone = f.Stream
		}
		if f.Flags&flagEndStream != 0 {
			done[f.Stream] = true
		}
	}
	x.tc.Logf("DATA order (stream:length): %s", strings.Join(order, " "))
	if firstDone != want {
		x.tc.Warnf("the response on stream %d (priority %q) completed before the one on stream %d (priority %q); %s",
			other, map[uint32]string{3: prio3, 5: prio5}[other], want, map[uint32]string{3: prio3, 5: prio5}[want], rule)
	} else if interleaved {
		x.tc.Warnf("DATA for stream %d was interleaved with stream %d's response; %s", other, want, rule)
	}
}

var smCases = []*smCase{
	// Frame size (RFC 9113 §4.2).
	{
		serverConfCase: serverConfCase{
			name:   "headers-frame-too-large",
			covers: "9113-4.2-3",
			desc:   "RFC 9113 §4.2: a HEADERS frame larger than the server's advertised SETTINGS_MAX_FRAME_SIZE (1MiB by default) MUST be treated as a connection error of type FRAME_SIZE_ERROR",
		},
		script: smHeadersTooLarge,
	},
	{
		serverConfCase: serverConfCase{
			name:    "headers-frame-too-large-constrained",
			covers:  "9113-4.2-3",
			desc:    "RFC 9113 §4.2: a HEADERS frame larger than the server's advertised SETTINGS_MAX_FRAME_SIZE of 16384 MUST be treated as a connection error of type FRAME_SIZE_ERROR",
			profile: goProfiles[1],
		},
		script: smHeadersTooLarge,
	},

	// Compression state (RFC 9113 §4.3.1, RFC 7541).
	{
		serverConfCase: serverConfCase{
			name:    "hpack-table-reduction-without-update",
			covers:  "9113-4.3.1-2",
			desc:    "RFC 9113 §4.3.1: a field block that follows the acknowledgment of a reduced SETTINGS_HEADER_TABLE_SIZE and doesn't start with a dynamic table size update MUST be treated as a connection error of type COMPRESSION_ERROR",
			profile: goProfiles[1], // MaxDecoderHeaderTableSize 256
		},
		script: func(x *smRun) {
			hts, ok := x.serverSettings[setHeaderTableSize]
			if !ok || hts >= 4096 {
				x.tc.Errorf("server advertised SETTINGS_HEADER_TABLE_SIZE=%d (present=%v); want a reduction", hts, ok)
				return
			}
			// The client has acknowledged the server's SETTINGS.
			// Its encoder still uses a 4096-byte table and doesn't
			// signal a size update. The block inserts 8 fields of
			// 80 bytes each (RFC 7541 §4.1), more than the
			// server's table holds.
			var kv []string
			for i := range 8 {
				kv = append(kv, fmt.Sprintf("x-fill-%d", i), strings.Repeat(string(rune('a'+i)), 40))
			}
			x.c.writeFields(1, true, x.reqFields("GET", "/hello", kv...)...)
			f, closed := x.response(1, reactionTimeout)
			switch {
			case f == nil:
				x.tc.Failf("no response or connection error within %v (closed=%v)", reactionTimeout, closed)
				return
			case f.Type == ftGoAway && f.ErrCode() == http2.ErrCodeCompression:
				x.tc.Logf("got expected connection error: %v", f)
				return
			case f.Type == ftGoAway:
				x.tc.Failf("got %v; want GOAWAY COMPRESSION_ERROR", f)
				return
			}
			x.tc.Failf("Go server accepted a field block without a dynamic table size update after the client acknowledged SETTINGS_HEADER_TABLE_SIZE=%d (got %v); want connection error COMPRESSION_ERROR (RFC 9113 §4.3.1: MUST)", hts, f)
			// Show the consequence: the client's next block
			// references entries that the server's smaller table
			// evicted.
			x.c.writeFields(3, true, x.reqFields("GET", "/hello", kv...)...)
			f, closed = x.response(3, reactionTimeout)
			x.tc.Logf("a second block referencing the client's dynamic table entries got: %v (closed=%v)", f, closed)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "oversized-field-block-hpack-state",
			covers: "9113-10.5.1-2 9113-4.3-d1",
			desc:   "RFC 9113 §10.5.1: a field block that exceeds the server's limit MUST still be processed to keep the connection state consistent; dynamic table entries it inserts can be referenced by the next request",
		},
		configure: func(srv *http.Server) { srv.MaxHeaderBytes = 4096 },
		script: func(x *smRun) {
			limit, ok := x.serverSettings[setMaxHeaderListSize]
			if !ok || limit > 64<<10 {
				x.tc.Errorf("server advertised SETTINGS_MAX_HEADER_LIST_SIZE=%d (present=%v); want a small limit", limit, ok)
				return
			}
			// Filler fields of about 1KB each exceed the limit.
			// The fields to reference come last, after the
			// server has stopped collecting fields, and are the
			// most recent entries in the dynamic table.
			kv := x.reqFields("GET", "/info")
			for i := range int(limit)/1000 + 2 {
				kv = append(kv, fmt.Sprintf("x-fill-%d", i), strings.Repeat(string(rune('a'+i%26)), 1000))
			}
			ref := []string{"x-ref-a", "alpha-after-limit", "x-ref-b", "beta-after-limit"}
			kv = append(kv, ref...)
			x.c.writeFields(1, true, kv...)
			f, closed := x.response(1, reactionTimeout)
			switch {
			case f == nil:
				x.tc.Failf("no response to the oversized field block within %v (closed=%v)", reactionTimeout, closed)
				return
			case f.Type == ftGoAway:
				// RFC 9113 §10.5.1 permits closing the connection.
				x.tc.Logf("server closed the connection: %v", f)
				return
			case f.Type == ftRSTStream:
				x.tc.Logf("server reset the stream: %v", f)
			case f.Status() == "200":
				x.tc.Errorf("server accepted a %d-field request exceeding its SETTINGS_MAX_HEADER_LIST_SIZE of %d", len(kv)/2, limit)
				return
			default:
				x.tc.Logf("server responded %s to the oversized field block", f.Status())
			}
			// This block references the entries the oversized
			// block inserted.
			blk := x.c.block(x.reqFields("GET", "/info", ref...)...)
			if !hpackUsesDynamicTable(blk) {
				x.tc.Errorf("the second field block doesn't use the dynamic table")
				return
			}
			x.c.writeHeaders(3, true, true, blk)
			if x.expectStatus(3, 200) == nil {
				return
			}
			body, _ := x.readBody(3, false, reactionTimeout)
			var info InfoBody
			json.Unmarshal(body, &info)
			for i := 0; i < len(ref); i += 2 {
				if got := info.Header[ref[i]]; !slices.Equal(got, []string{ref[i+1]}) {
					x.tc.Failf("handler saw %s %q; want [%s] (compression state not maintained after an oversized field block)", ref[i], got, ref[i+1])
				}
			}
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "hpack-duplicate-entries",
			covers: "7541-2.3.2-1",
			desc:   "RFC 7541 §2.3.2: the dynamic table can contain duplicate entries, which MUST NOT be treated as an error by a decoder",
		},
		script: func(x *smRun) {
			// This connection doesn't use x.c's encoder, whose
			// dynamic table wouldn't match.
			blk := x.smHpackRequest(nil, "/info")
			blk = smHpackLiteral(blk, true, "x-dup", "same")
			blk = smHpackLiteral(blk, true, "x-dup", "same")
			x.c.writeHeaders(1, true, true, blk)
			check := func(stream uint32) {
				if x.expectStatus(stream, 200) == nil {
					return
				}
				body, _ := x.readBody(stream, false, reactionTimeout)
				var info InfoBody
				json.Unmarshal(body, &info)
				if got := info.Header["x-dup"]; !slices.Equal(got, []string{"same", "same"}) {
					x.tc.Failf("stream %d: handler saw x-dup %q; want [same same]", stream, got)
				}
			}
			check(1)
			// Reference both duplicate entries (dynamic table
			// indices 62 and 63, RFC 7541 §2.3.3).
			blk = x.smHpackRequest(nil, "/info")
			blk = smHpackInt(blk, 7, 0x80, 62)
			blk = smHpackInt(blk, 7, 0x80, 63)
			x.c.writeHeaders(3, true, true, blk)
			check(3)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "header-table-size-changed-twice",
			covers: "7541-4.2-3",
			desc:   "RFC 7541 §4.2: if SETTINGS_HEADER_TABLE_SIZE changes more than once between field blocks (here to 0, then to 4096), the encoder MUST signal the smallest size in a dynamic table size update",
		},
		script: func(x *smRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/hello")...)
			if x.expectStatus(1, 200) == nil {
				return
			}
			x.readBody(1, false, reactionTimeout)
			x.c.writeSettings(http2.Setting{ID: http2.SettingHeaderTableSize, Val: 0})
			x.c.writeSettings(http2.Setting{ID: http2.SettingHeaderTableSize, Val: 4096})
			// The PING ACK follows the ACKs of both SETTINGS.
			if !x.pingRoundTrip() {
				return
			}
			x.c.writeFields(3, true, x.reqFields("GET", "/hello")...)
			f := x.expectStatus(3, 200)
			if f == nil {
				return
			}
			if f.Type != ftHeaders || f.Flags&(flagPadded|flagPriority) != 0 {
				x.tc.Logf("not checking the encoding of %v", f)
				return
			}
			// The validator checks this too; see decodeFields.
			ups := smLeadingSizeUpdates(f.Payload)
			x.tc.Logf("response field block starts with dynamic table size updates %v", ups)
			if !slices.Contains(ups, 0) {
				x.tc.Failf("response field block starts with dynamic table size updates %v; want an update to 0, the smallest SETTINGS_HEADER_TABLE_SIZE since the last block (RFC 7541 §4.2: MUST)", ups)
			}
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "hpack-integer-too-long",
			covers: "7541-5.1-1",
			desc:   "RFC 7541 §5.1: an HPACK integer encoding that exceeds implementation limits in octet length (here a 12-octet index) MUST be treated as a decoding error (a connection error of type COMPRESSION_ERROR, RFC 9113 §4.3)",
		},
		script: func(x *smRun) {
			blk := x.c.block(x.reqFields("GET", "/hello")...)
			// An indexed field whose index has ten all-zero
			// continuation octets.
			blk = append(blk, 0xff)
			blk = append(blk, bytes.Repeat([]byte{0x80}, 10)...)
			blk = append(blk, 0x01)
			x.c.writeHeaders(1, true, true, blk)
			x.c.expectConnError(http2.ErrCodeCompression)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "hpack-integer-too-large",
			covers: "7541-5.1-1",
			desc:   "RFC 7541 §5.1: an HPACK integer encoding that exceeds implementation limits in value (here a 2^40-octet string length) MUST be treated as a decoding error (a connection error of type COMPRESSION_ERROR, RFC 9113 §4.3)",
		},
		script: func(x *smRun) {
			blk := x.c.block(x.reqFields("GET", "/hello")...)
			// A literal field whose name length is 2^40.
			blk = append(blk, 0x00)
			blk = smHpackInt(blk, 7, 0, 1<<40)
			blk = append(blk, "x-name"...)
			x.c.writeHeaders(1, true, true, blk)
			x.c.expectConnError(http2.ErrCodeCompression)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "response-field-order",
			covers: "7541-2.1-1",
			desc:   "RFC 7541 §2.1: an encoder MUST order field representations according to their order in the original field list; multiple values of a response field set by a handler arrive in order",
		},
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, v := range []string{"1", "2", "3"} {
				w.Header().Add("X-A", v)
			}
			io.WriteString(w, "ok\n")
		}),
		script: func(x *smRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/")...)
			f := x.expectStatus(1, 200)
			if f == nil {
				return
			}
			var got []string
			for _, hf := range f.Fields {
				if hf.Name == "x-a" {
					got = append(got, hf.Value)
				}
			}
			if !slices.Equal(got, []string{"1", "2", "3"}) {
				x.tc.Failf("response x-a fields %q; want [1 2 3] in that order", got)
			}
		},
	},

	// Frames on streams the server reset (RFC 9113 §5.1, §5.4.2, §6.4).
	{
		serverConfCase: serverConfCase{
			name:    "reset-stream-data-discarded",
			covers:  "9113-5.1-11 9113-5.4.2-1 9113-6.4-2 9113-6.9-5 9113-5.1-d1",
			desc:    "RFC 9113 §5.1, §6.4, §6.9: DATA that the client sent on a stream before receiving the server's RST_STREAM MUST be discarded without a connection error and MUST still count toward the connection flow control window; the connection stays usable",
			profile: goProfiles[1], // 64KB connection window, 16KB stream windows
		},
		script: func(x *smRun) {
			chunk := min(x.streamWindow(), 16384)
			// Fill the server's connection window with DATA on
			// streams the server resets: /rst aborts the handler
			// without reading the request body.
			for i := range 4 {
				s := uint32(2*i + 1)
				if !x.awaitConnCredit(int64(chunk), reactionTimeout) {
					x.tc.Failf("no connection flow control credit for stream %d (window %d)", s, x.connSendWin)
					return
				}
				x.c.writeFields(s, false, x.reqFields("POST", "/rst")...)
				f, _ := x.c.waitFor(reactionTimeout, func(f *rawFrame) bool {
					return f.Type == ftGoAway || f.Type == ftRSTStream && f.Stream == s
				})
				if f == nil || f.Type == ftGoAway {
					x.tc.Failf("got %v; want RST_STREAM on stream %d from the /rst handler", f, s)
					return
				}
				// DATA that the client had sent before it saw the
				// RST_STREAM.
				x.sendData(s, true, make([]byte, chunk))
			}
			if !x.pingRoundTrip() {
				return
			}
			// The server should return the connection-level credit
			// for the discarded DATA.
			if !x.awaitConnCredit(65535, reactionTimeout) {
				x.tc.Failf("client's connection send window is %d after the server discarded %d bytes of DATA on reset streams; want at least 65535: the server didn't return the connection flow control credit", x.connSendWin, 4*chunk)
			}
			x.upload(9, chunk)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "reset-stream-trailers-discarded",
			covers: "9113-5.1-11 9113-6.4-2 9113-5.1-d1",
			desc:   "RFC 9113 §5.1, §6.4: a HEADERS frame (trailers) that the client sent on a stream before receiving the server's RST_STREAM MUST be minimally processed (updating the compression state) and discarded, not treated as a connection error",
		},
		script: func(x *smRun) {
			x.c.writeFields(1, false, x.reqFields("POST", "/rst", "trailer", "x-sm-trailer")...)
			f, _ := x.c.waitFor(reactionTimeout, func(f *rawFrame) bool {
				return f.Type == ftGoAway || f.Type == ftRSTStream && f.Stream == 1
			})
			if f == nil || f.Type == ftGoAway {
				x.tc.Failf("got %v; want RST_STREAM on stream 1 from the /rst handler", f)
				return
			}
			// Trailers the client had sent before it saw the
			// RST_STREAM. They insert x-sm-trailer into the
			// dynamic table.
			x.sendData(1, false, []byte("body"))
			x.c.writeFields(1, true, "x-sm-trailer", "discarded-trailer-value")
			if !x.pingRoundTrip() {
				x.tc.Failf("Go server treated trailers on a stream it had reset as a connection error (RFC 9113 §5.1, §6.4: MUST minimally process and discard frames sent before the peer received the RST_STREAM)")
				return
			}
			// This block references the dynamic table entry.
			x.c.writeFields(3, true, x.reqFields("GET", "/info", "x-sm-trailer", "discarded-trailer-value")...)
			if x.expectStatus(3, 200) == nil {
				return
			}
			body, _ := x.readBody(3, false, reactionTimeout)
			var info InfoBody
			json.Unmarshal(body, &info)
			if got := info.Header["x-sm-trailer"]; !slices.Equal(got, []string{"discarded-trailer-value"}) {
				x.tc.Failf("handler saw x-sm-trailer %q; want [discarded-trailer-value] (compression state not maintained)", got)
			}
		},
	},

	// Flow control.
	{
		serverConfCase: serverConfCase{
			name:    "handler-not-reading-body",
			covers:  "9113-5.2.2-1",
			desc:    "RFC 9113 §5.2.2: endpoints MUST read and process frames as soon as data is available; the server keeps processing frames while handlers don't read their request bodies",
			profile: goProfiles[1], // 64KB connection window, 16KB stream windows
		},
		script: func(x *smRun) {
			chunk := min(x.streamWindow(), 16384)
			// /delay doesn't read the request body.
			for _, s := range []uint32{1, 3, 5} {
				x.c.writeFields(s, false, x.reqFields("POST", "/delay/60000")...)
				if !x.awaitConnCredit(int64(chunk), reactionTimeout) {
					x.tc.Failf("no connection flow control credit for stream %d (window %d)", s, x.connSendWin)
					return
				}
				x.sendData(s, false, make([]byte, chunk))
			}
			if !x.pingRoundTrip() {
				return
			}
			// The rest of the connection window is enough for an
			// upload on another stream. (The server may not have
			// raised its connection window above 65535, and it
			// returns no credit for unread bodies.)
			n := min(chunk, int(x.connSendWin))
			if n < 1000 {
				x.tc.Errorf("only %d bytes of connection window left", n)
				return
			}
			x.upload(7, n)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "initial-window-size-overflow",
			covers: "9113-6.9.2-3",
			desc:   "RFC 9113 §6.9.2: a SETTINGS_INITIAL_WINDOW_SIZE change that causes a stream's flow control window to exceed 2^31-1 MUST be treated as a connection error of type FLOW_CONTROL_ERROR",
		},
		script: func(x *smRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/delay/10000")...)
			// Bring the server's send window for stream 1 to
			// exactly 2^31-1, which is allowed.
			x.c.writeWindowUpdate(1, 1<<31-1-65535)
			if !x.pingRoundTrip() {
				return
			}
			x.c.writeSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 65536})
			x.c.expectConnError(http2.ErrCodeFlowControl)
		},
	},

	// SETTINGS (RFC 9113 §6.5).
	{
		serverConfCase: serverConfCase{
			name:     "max-frame-size-16384",
			covers:   "9113-6.5-2",
			desc:     "RFC 9113 §6.5: implementations MUST support all settings; the server sends no frame larger than the client's SETTINGS_MAX_FRAME_SIZE of 16384",
			settings: []http2.Setting{{ID: http2.SettingMaxFrameSize, Val: 16384}, {ID: http2.SettingInitialWindowSize, Val: 4 << 20}},
		},
		script: func(x *smRun) { x.maxFrameSize(16384) },
	},
	{
		serverConfCase: serverConfCase{
			name:     "max-frame-size-65536",
			covers:   "9113-6.5-2",
			desc:     "RFC 9113 §6.5: implementations MUST support all settings; the server sends no frame larger than the client's SETTINGS_MAX_FRAME_SIZE of 65536",
			settings: []http2.Setting{{ID: http2.SettingMaxFrameSize, Val: 65536}, {ID: http2.SettingInitialWindowSize, Val: 4 << 20}},
		},
		script: func(x *smRun) { x.maxFrameSize(65536) },
	},
	{
		serverConfCase: serverConfCase{
			name:     "max-header-list-size-advisory",
			covers:   "9113-6.5-2",
			desc:     "RFC 9113 §6.5.2: SETTINGS_MAX_HEADER_LIST_SIZE advises the peer of the largest field section the client accepts; the server should not send a larger response field section",
			settings: []http2.Setting{{ID: http2.SettingMaxHeaderListSize, Val: 1000}},
		},
		script: func(x *smRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/bigheader/4000")...)
			f, closed := x.response(1, reactionTimeout)
			switch {
			case f == nil:
				x.tc.Failf("no response within %v (closed=%v)", reactionTimeout, closed)
			case f.Fields != nil:
				var size uint32
				for _, hf := range f.Fields {
					size += hf.Size()
				}
				if size > 1000 {
					x.tc.Warnf("Go server sent a response field section of %d bytes, exceeding the client's SETTINGS_MAX_HEADER_LIST_SIZE of 1000 (RFC 9113 §6.5.2, advisory)", size)
				}
			default:
				x.tc.Logf("server didn't send the response: %v", f)
			}
		},
	},

	// GOAWAY (RFC 9113 §5.4.1, §6.8, §9.1).
	{
		serverConfCase: serverConfCase{
			name:   "goaway-on-shutdown-idle",
			covers: "9113-5.4.1-4 9113-6.8-2 9113-9.1-3",
			desc:   "RFC 9113 §6.8, §9.1: a server SHOULD send GOAWAY before closing a connection; here an idle connection on Server.Shutdown",
		},
		script: func(x *smRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/hello")...)
			if x.expectStatus(1, 200) == nil {
				return
			}
			x.readBody(1, false, reactionTimeout)
			if !x.pingRoundTrip() {
				return
			}
			x.shutdown()
			x.checkGoAwayBeforeClose("Server.Shutdown", 5*time.Second)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "goaway-on-shutdown-active",
			covers: "9113-5.4.1-4 9113-6.8-2 9113-6.8-7 9113-9.1-3",
			desc:   "RFC 9113 §6.8: a server gracefully shutting down a connection SHOULD first send GOAWAY with last-stream-ID 2^31-1 and NO_ERROR; here Server.Shutdown with a request in flight",
		},
		script: func(x *smRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/delay/1000")...)
			if !x.pingRoundTrip() {
				return
			}
			x.shutdown()
			g, closed := x.c.waitFor(reactionTimeout, isType(ftGoAway))
			if g == nil {
				x.tc.Warnf("no GOAWAY within %v of Server.Shutdown (closed=%v) (RFC 9113 §6.8: SHOULD send GOAWAY)", reactionTimeout, closed)
				return
			}
			x.tc.Logf("first GOAWAY: %v", g)
			if g.ErrCode() != http2.ErrCodeNo {
				x.tc.Warnf("graceful shutdown started with GOAWAY %v; want NO_ERROR (RFC 9113 §6.8: SHOULD)", g.ErrCode())
			}
			if last := smGoAwayLastStream(g); last != 1<<31-1 {
				x.tc.Warnf("graceful shutdown started with GOAWAY last-stream-ID %d; want 2^31-1, so that streams the client is opening concurrently aren't refused (RFC 9113 §6.8: SHOULD)", last)
			}
			if x.expectStatus(1, 200) != nil {
				if _, ok := x.readBodyGraceful(1, 5*time.Second); !ok {
					x.tc.Warnf("the in-flight response didn't complete during graceful shutdown")
				}
			}
			x.checkGoAwayBeforeClose("Server.Shutdown", 5*time.Second, g)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "goaway-on-idle-timeout",
			covers: "9113-5.4.1-4 9113-6.8-2 9113-9.1-3",
			desc:   "RFC 9113 §6.8, §9.1: a server SHOULD send GOAWAY before closing a connection; here an idle connection when Server.IdleTimeout expires",
		},
		configure: func(srv *http.Server) { srv.IdleTimeout = 300 * time.Millisecond },
		script: func(x *smRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/hello")...)
			if x.expectStatus(1, 200) == nil {
				return
			}
			x.readBody(1, false, reactionTimeout)
			x.checkGoAwayBeforeClose("the idle timeout", 5*time.Second)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "goaway-on-close",
			covers: "9113-5.4.1-4 9113-6.8-2 9113-9.1-3",
			desc:   "RFC 9113 §6.8, §9.1: a server SHOULD send GOAWAY before closing a connection, if circumstances permit; here Server.Close",
		},
		script: func(x *smRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/hello")...)
			if x.expectStatus(1, 200) == nil {
				return
			}
			x.readBody(1, false, reactionTimeout)
			if !x.pingRoundTrip() {
				return
			}
			x.srv.Close()
			x.checkGoAwayBeforeClose("Server.Close", reactionTimeout)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "goaway-after-client-goaway",
			covers: "9113-6.8-3",
			desc:   "RFC 9113 §6.8: a receiver of a GOAWAY that has no more use for the connection SHOULD still send a GOAWAY frame before terminating the connection",
		},
		script: func(x *smRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/hello")...)
			if x.expectStatus(1, 200) == nil {
				return
			}
			x.readBody(1, false, reactionTimeout)
			x.c.writeGoAway(0, http2.ErrCodeNo)
			x.checkGoAwayBeforeClose("the client's GOAWAY", 5*time.Second)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "goaway-hpack-state-maintained",
			covers: "9113-6.8-9 9113-4.3-d1",
			desc:   "RFC 9113 §6.8: after sending GOAWAY, HEADERS frames on streams above the last-stream-ID MUST be minimally processed to keep the compression state consistent, and their DATA MUST count toward the connection window",
		},
		script: func(x *smRun) {
			// Stream 1's upload is in progress through the
			// graceful shutdown.
			x.c.writeFields(1, false, x.reqFields("POST", "/upload", "trailer", "x-sm-late")...)
			x.sendData(1, false, []byte("body"))
			if !x.pingRoundTrip() {
				return
			}
			x.shutdown()
			g, _ := x.c.waitFor(reactionTimeout, isType(ftGoAway))
			if g == nil || g.ErrCode() != http2.ErrCodeNo {
				x.tc.Errorf("got %v; want a graceful GOAWAY", g)
				return
			}
			if last := smGoAwayLastStream(g); last >= 3 {
				x.tc.Logf("GOAWAY last-stream-ID %d admits stream 3", last)
			}
			// The client opened stream 3 before it received the
			// GOAWAY. Its field block inserts x-sm-late into the
			// dynamic table.
			if !x.awaitConnCredit(16384, reactionTimeout) {
				x.tc.Errorf("no connection flow control credit")
				return
			}
			x.c.writeFields(3, false, x.reqFields("POST", "/upload", "x-sm-late", "inserted-by-stream-3")...)
			before := x.connSendWin
			x.sendData(3, true, make([]byte, 16384))
			// Stream 1's trailers reference that entry.
			x.c.writeFields(1, true, "x-sm-late", "inserted-by-stream-3")
			if x.expectStatus(1, 200) == nil {
				return
			}
			body, _ := x.readBodyGraceful(1, reactionTimeout)
			var u UploadBody
			json.Unmarshal(body, &u)
			if got := u.Trailer["x-sm-late"]; !slices.Equal(got, []string{"inserted-by-stream-3"}) {
				x.tc.Failf("handler saw trailer x-sm-late %q; want [inserted-by-stream-3] (compression state not maintained for a stream above the GOAWAY's last-stream-ID)", got)
			}
			if !x.awaitConnCredit(before, reactionTimeout) {
				x.tc.Warnf("server returned connection flow control credit for %d of the 16384 bytes of DATA on stream 3 after GOAWAY", 16384-(before-x.connSendWin))
			}
		},
	},

	// Connection-specific fields (RFC 9113 §8.2.2).
	smConnSpecificCase("Connection", "keep-alive"),
	smConnSpecificCase("Keep-Alive", "timeout=5"),
	smConnSpecificCase("Proxy-Connection", "keep-alive"),
	smConnSpecificCase("Transfer-Encoding", "chunked"),
	smConnSpecificCase("Upgrade", "h2c"),
	{
		serverConfCase: serverConfCase{
			name:   "response-connection-specific-transfer-encoding-trailers",
			covers: "9113-8.2.2-1",
			desc:   "RFC 9113 §8.2.2: an endpoint MUST NOT generate an HTTP/2 message containing connection-specific header fields; the server must not send Transfer-Encoding: trailers set by a handler (only TE may carry \"trailers\")",
		},
		handler: smConnSpecificHandler(),
		script: func(x *smRun) {
			x.c.writeFields(1, true, x.reqFields("GET", "/?name=Transfer-Encoding&value=trailers")...)
			f := x.expectStatus(1, 200)
			if f == nil {
				return
			}
			for _, hf := range f.Fields {
				if hf.Name == "transfer-encoding" {
					x.tc.Failf("Go server sent connection-specific response field %q: %q set by the handler (RFC 9113 §8.2.2: MUST NOT)", hf.Name, hf.Value)
				}
			}
		},
	},

	// CONNECT (RFC 9113 §8.5).
	smConnectTunnelHeadersCase("connect-tunnel-headers", true),
	smConnectTunnelHeadersCase("connect-tunnel-headers-no-end-stream", false),

	// Extended CONNECT (RFC 8441), which needs GODEBUG=http2xconnect=1.
	{
		serverConfCase: serverConfCase{
			name:   "extended-connect-reaches-handler",
			covers: "8441-4-3",
			desc:   "RFC 8441 §4: the server MUST NOT create a tunnel to the :authority of an extended CONNECT request; the request reaches the handler for its :path",
		},
		godebug: "http2xconnect=1",
		script: func(x *smRun) {
			if !x.extendedConnectEnabled() {
				return
			}
			x.c.writeFields(1, false, ":method", "CONNECT", ":protocol", "websocket", ":scheme", "http",
				":authority", "tunnel-target.invalid:443", ":path", "/info")
			if x.expectStatus(1, 200) == nil {
				return
			}
			body, _ := x.readBody(1, false, reactionTimeout)
			var info InfoBody
			if err := json.Unmarshal(body, &info); err != nil || info.Method != "CONNECT" || info.Path != "/info" {
				x.tc.Failf("extended CONNECT didn't reach the /info handler as a request: response %q", body)
			}
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "extended-connect-without-path",
			covers: "8441-4-2",
			desc:   "RFC 8441 §4: requests with the :protocol pseudo-header MUST include :scheme and :path; an extended CONNECT without :path is malformed",
		},
		godebug: "http2xconnect=1",
		script: func(x *smRun) {
			if !x.extendedConnectEnabled() {
				return
			}
			x.c.writeFields(1, false, ":method", "CONNECT", ":protocol", "websocket", ":scheme", "http", ":authority", x.authority)
			x.expectMalformed(1)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "extended-connect-without-scheme",
			covers: "8441-4-2",
			desc:   "RFC 8441 §4: requests with the :protocol pseudo-header MUST include :scheme and :path; an extended CONNECT without :scheme is malformed",
		},
		godebug: "http2xconnect=1",
		script: func(x *smRun) {
			if !x.extendedConnectEnabled() {
				return
			}
			x.c.writeFields(1, false, ":method", "CONNECT", ":protocol", "websocket", ":authority", x.authority, ":path", "/info")
			x.expectMalformed(1)
		},
	},

	// RFC 9218 priorities.
	{
		serverConfCase: serverConfCase{
			name:   "priority-parameters-ignored",
			covers: "9218-4-2",
			desc:   "RFC 9218 §4: unknown priority parameters, out-of-range values, and values of unexpected types MUST be ignored, in the priority header field and in PRIORITY_UPDATE",
		},
		script: func(x *smRun) {
			x.c.writeRaw(ftPriorityUpdate, 0, 0, append(binary.BigEndian.AppendUint32(nil, 1), `u=-1, i="yes", foo=1.5, bar`...))
			x.c.writeFields(1, true, x.reqFields("GET", "/hello", "priority", "u=9, i=2, baz=(1 2), q=?0")...)
			x.expectStatus(1, 200)
			x.c.writeFields(3, true, x.reqFields("GET", "/delay/100", "priority", `u="0", i=?1;x=1, zz=:aGVsbG8=:`)...)
			x.c.writeRaw(ftPriorityUpdate, 0, 0, append(binary.BigEndian.AppendUint32(nil, 3), `u=8, i=1, u2=0`...))
			x.expectStatus(3, 200)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:    "priority-update-idle-limit",
			covers:  "9218-7.1-5",
			desc:    "RFC 9218 §7.1: the number of streams prioritized by PRIORITY_UPDATE in the idle state plus the active streams MUST NOT exceed SETTINGS_MAX_CONCURRENT_STREAMS; servers MUST treat exceeding it as a connection error of type PROTOCOL_ERROR",
			profile: goProfiles[1], // MaxConcurrentStreams 4
		},
		script: func(x *smRun) {
			mcs, ok := x.serverSettings[setMaxConcurrentStreams]
			if !ok || mcs < 3 || mcs > 100 {
				x.tc.Errorf("server's SETTINGS_MAX_CONCURRENT_STREAMS is %d (present=%v); want a small limit", mcs, ok)
				return
			}
			// Two active streams.
			x.c.writeFields(1, true, x.reqFields("GET", "/delay/30000")...)
			x.c.writeFields(3, true, x.reqFields("GET", "/delay/30000")...)
			pu := func(s uint32) {
				x.c.writeRaw(ftPriorityUpdate, 0, 0, append(binary.BigEndian.AppendUint32(nil, s), "u=1"...))
			}
			// Prioritize idle streams up to the limit.
			s := uint32(5)
			for range mcs - 2 {
				pu(s)
				s += 2
			}
			if !x.pingRoundTrip() {
				return
			}
			// One more exceeds it.
			pu(s)
			f, closed := x.c.waitFor(reactionTimeout, isType(ftGoAway))
			switch {
			case f != nil && f.ErrCode() == http2.ErrCodeProtocol:
				x.tc.Logf("got expected connection error: %v", f)
			case f != nil:
				x.tc.Failf("got %v; want GOAWAY PROTOCOL_ERROR", f)
			default:
				x.tc.Failf("Go server accepted PRIORITY_UPDATE frames for %d idle streams with %d active streams, exceeding its SETTINGS_MAX_CONCURRENT_STREAMS of %d (closed=%v); want connection error PROTOCOL_ERROR (RFC 9218 §7.1: MUST)", mcs-1, 2, mcs, closed)
			}
		},
	},
	{
		serverConfCase: serverConfCase{
			name:     "priority-urgency-order",
			covers:   "9218-10-1",
			desc:     "RFC 9218 §10: it is RECOMMENDED that servers send higher-urgency responses before lower-urgency responses",
			settings: []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1 << 20}},
		},
		script: func(x *smRun) {
			x.priorityOrder("u=7", "u=0", 5, "RFC 9218 §10: RECOMMENDED to send higher-urgency responses first")
		},
	},
	{
		serverConfCase: serverConfCase{
			name:     "priority-same-urgency-stream-order",
			covers:   "9218-10-3",
			desc:     "RFC 9218 §10: non-incremental responses of the same urgency SHOULD be served in ascending order of stream ID",
			settings: []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1 << 20}},
		},
		script: func(x *smRun) {
			x.priorityOrder("u=3", "u=3", 3, "RFC 9218 §10: non-incremental responses of the same urgency SHOULD be served in stream ID order")
		},
	},
}

func smHeadersTooLarge(x *smRun) {
	max, ok := x.serverSettings[setMaxFrameSize]
	if !ok {
		max = 16384
	}
	x.tc.Logf("server's SETTINGS_MAX_FRAME_SIZE is %d", max)
	target := int(max) + 1
	blk := x.c.block(x.reqFields("GET", "/hello")...)
	// Pad the block with a field literal (not indexed) so that the
	// HEADERS frame payload is one octet too large.
	rest := target - len(blk)
	vlen := rest
	for vlen > 0 && len(smHpackLiteral(nil, false, "x-pad", ""))+len(smHpackInt(nil, 7, 0, uint64(vlen)))-1+vlen != rest {
		vlen--
	}
	blk = smHpackLiteral(blk, false, "x-pad", strings.Repeat("a", vlen))
	if len(blk) != target {
		x.tc.Errorf("built a %d-byte field block; want %d", len(blk), target)
		return
	}
	x.c.writeHeaders(1, true, true, blk)
	x.c.expectConnError(http2.ErrCodeFrameSize)
}

// maxFrameSize checks that a large response uses no frame larger
// than limit, the client's SETTINGS_MAX_FRAME_SIZE.
func (x *smRun) maxFrameSize(limit int) {
	const n = 500000
	x.c.writeWindowUpdate(0, 4<<20)
	x.c.writeFields(1, true, x.reqFields("GET", "/bytes/"+strconv.Itoa(n))...)
	if x.expectStatus(1, 200) == nil {
		return
	}
	got, largest := 0, 0
	for {
		f, _ := x.c.waitFor(reactionTimeout, func(f *rawFrame) bool {
			return f.Stream == 1 && (f.Type == ftData || f.Type == ftRSTStream) || f.Type == ftGoAway
		})
		if f == nil || f.Type != ftData {
			x.tc.Failf("after %d body bytes: got %v", got, f)
			return
		}
		got += len(f.Payload)
		largest = max(largest, len(f.Payload))
		if len(f.Payload) > limit {
			x.tc.Failf("Go server sent a DATA frame of %d bytes, exceeding the client's SETTINGS_MAX_FRAME_SIZE of %d (RFC 9113 §4.2, §6.5: MUST)", len(f.Payload), limit)
		}
		if f.Flags&flagEndStream != 0 {
			break
		}
	}
	x.tc.Logf("got %d body bytes; largest DATA frame %d bytes (limit %d)", got, largest, limit)
	if got != n {
		x.tc.Failf("got %d body bytes; want %d", got, n)
	}
}

// shutdown starts a graceful Server.Shutdown.
func (x *smRun) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer cancel()
		err := x.srv.Shutdown(ctx)
		x.tc.Logf("Server.Shutdown returned %v", err)
	}()
	x.tc.Cleanup(func() {
		cancel()
		<-done
	})
}

// extendedConnectEnabled reports whether the server advertised
// SETTINGS_ENABLE_CONNECT_PROTOCOL=1, reporting an error if not.
func (x *smRun) extendedConnectEnabled() bool {
	if x.serverSettings[setEnableConnectProtocol] != 1 {
		x.tc.Errorf("server didn't advertise SETTINGS_ENABLE_CONNECT_PROTOCOL=1; the GODEBUG setting had no effect")
		return false
	}
	return true
}
