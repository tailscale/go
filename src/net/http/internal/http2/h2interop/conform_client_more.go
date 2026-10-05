// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/internal/http2"
	"net/textproto"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// This file has more frame-level conformance tests of the Go client,
// in the style of conform_client.go. Its cases may configure the Go
// client's Transport and may drive the client with their own code
// (for example, to leave a response body unread, or to make requests
// that the Req type can't express).
//
// The cases are registered by this file's init function, which runs
// after conform_client.go's (files are initialized in file name
// order), so that they are registered as tests exactly once.

// clientMoreCase is a conform/client case run by runClientMore.
type clientMoreCase struct {
	clientConfCase
	// tweak, if non-nil, adjusts the Go client's Transport. Its
	// HTTP2 field is already set to a copy of the profile's config.
	tweak func(tr *http.Transport)
	// client, if non-nil, drives the Go client instead of making the
	// requests in reqs. It runs in its own goroutine.
	client func(ctx context.Context, y *clientMoreRun) []*Result
	// run is the script, used instead of clientConfCase.script if set.
	run func(y *clientMoreRun)
	// noConnect, if non-nil, is called instead of failing the test
	// if the client finishes without connecting.
	noConnect func(y *clientMoreRun)
	// timeout overrides the default per-test timeout.
	timeout time.Duration
}

// clientMoreRun is the state of a running clientMoreCase.
type clientMoreRun struct {
	*clientConfRun
	url string // base URL of the tap in front of the scripted server
	tr  *http.Transport

	toScript   chan struct{} // signals from the client driver to the script
	toClient   chan struct{} // signals from the script to the client driver
	scriptDone chan struct{} // closed when the script returns
}

func init() {
	for _, c := range clientMoreCases {
		extraTests = append(extraTests, &Test{
			Name:    "conform/client/" + c.name,
			Desc:    c.desc,
			Timeout: c.timeout,
			Run: func(ctx context.Context, tc *testCtx) {
				runClientMore(ctx, tc, c)
			},
		})
		clientConfCases = append(clientConfCases, &c.clientConfCase)
	}
}

func runClientMore(ctx context.Context, tc *testCtx, c *clientMoreCase) {
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
	profile := c.profile
	if profile == nil {
		profile = goProfiles[0]
	}
	cfg := profile.HTTP2
	cfg.CountError = func(token string) { tc.countError("Go client", token) }
	protos := new(http.Protocols)
	protos.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols:             protos,
		HTTP2:                 &cfg,
		ExpectContinueTimeout: 5 * time.Second,
		DisableCompression:    true,
	}
	if c.tweak != nil {
		c.tweak(tr)
	}
	defer tr.CloseIdleConnections()

	x := &clientConfRun{tc: tc, ln: ln, results: make(chan []*Result, 1), conns: make(chan net.Conn, 100)}
	y := &clientMoreRun{
		clientConfRun: x,
		url:           "http://" + tap.Addr(),
		tr:            tr,
		toScript:      make(chan struct{}, 10),
		toClient:      make(chan struct{}, 10),
		scriptDone:    make(chan struct{}),
	}
	go x.acceptLoop()
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	go func() {
		if c.client != nil {
			x.results <- c.client(cctx, y)
			return
		}
		reqs := c.reqs
		if reqs == nil {
			reqs = []*Req{{Method: "GET", Path: "/"}}
		}
		spec := &ClientSpec{URL: y.url, H2C: true, Requests: reqs, Concurrent: c.concurrent}
		x.results <- doRequests(cctx, tc, tr, spec).Results
	}()

	// Wait for a connection, noticing if the client fails first.
	select {
	case conn := <-x.conns:
		x.conns <- conn // for accept
	case x.got = <-x.results:
		if c.noConnect != nil {
			c.noConnect(y)
			return
		}
		for i, r := range x.got {
			tc.Logf("Go client result %d: status=%d err=%q", i, r.Status, r.Error)
		}
		tc.Failf("Go client finished without connecting")
		return
	case <-time.After(5 * time.Second):
	}
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
	if c.run != nil {
		c.run(y)
	} else {
		c.script(x)
	}
	close(y.scriptDone)
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

// The following helpers are for client drivers, which run on their own
// goroutine.

// signalScript tells the script that the client driver reached a
// point the script waits for (see waitClient).
func (y *clientMoreRun) signalScript() { y.toScript <- struct{}{} }

// waitScript waits for the script to call releaseClient, or for the
// script to finish.
func (y *clientMoreRun) waitScript(ctx context.Context) {
	select {
	case <-y.toClient:
	case <-y.scriptDone:
	case <-ctx.Done():
	}
}

// newRequest returns a request. It panics if rawURL is invalid.
func (y *clientMoreRun) newRequest(ctx context.Context, method, rawURL string, body io.Reader) *http.Request {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		panic(err) // the URLs are constructed by the tests
	}
	return req
}

// roundTrip performs req with y's Transport, reads the whole response
// body, and closes it.
func (y *clientMoreRun) roundTrip(req *http.Request) *Result {
	res := &Result{}
	resp, err := y.tr.RoundTrip(req)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	res.Proto = protoName(resp)
	res.Header = sortedFields(textproto.MIMEHeader(resp.Header))
	b, err := io.ReadAll(resp.Body)
	res.Body = string(b)
	res.BodyLen = int64(len(b))
	if err != nil {
		res.Error = err.Error()
	}
	return res
}

// The following helpers are for scripts.

// waitClient waits for the client driver to call signalScript.
func (y *clientMoreRun) waitClient(what string) bool {
	select {
	case <-y.toScript:
		return true
	case y.got = <-y.results:
		y.tc.Failf("Go client finished before %s", what)
	case <-time.After(5 * time.Second):
		y.tc.Failf("timeout waiting for the Go client: %s", what)
	}
	return false
}

// releaseClient lets the client driver continue past waitScript.
func (y *clientMoreRun) releaseClient() { y.toClient <- struct{}{} }

// syncPing sends a PING and waits for its ACK. Endpoints process
// frames in order, so once it returns, the client has processed every
// frame the server sent before.
func (y *clientMoreRun) syncPing(sc *rawConn, data [8]byte) bool {
	sc.writePing(false, data)
	f, closed := sc.waitFor(reactionTimeout, func(f *rawFrame) bool {
		return f.Type == ftPing && f.Flags&flagAck != 0 && bytes.Equal(f.Payload, data[:])
	})
	if f == nil {
		y.tc.Failf("no PING ACK for %x (closed=%v)", data, closed)
		return false
	}
	return true
}

// syncSettings sends SETTINGS and waits until the client has
// processed them.
func (y *clientMoreRun) syncSettings(sc *rawConn, s ...http2.Setting) bool {
	sc.writeSettings(s...)
	return y.syncPing(sc, [8]byte{'s', 'e', 't', 't', 'i', 'n', 'g', 's'})
}

// serveAll responds "ok" to every request the client sends on sc
// until the client driver finishes, calling check on each request's
// field block, including y.req.
func (y *clientMoreRun) serveAll(sc *rawConn, check func(f *rawFrame)) {
	if y.req != nil {
		check(y.req)
		y.respond(sc, y.req.Stream, "ok")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case y.got = <-y.results:
			return
		default:
		}
		f, closed := sc.waitFor(100*time.Millisecond, func(f *rawFrame) bool { return f.Type == ftHeaders && f.Fields != nil })
		switch {
		case f != nil:
			check(f)
			y.respond(sc, f.Stream, "ok")
		case closed:
			y.result()
			return
		}
	}
	y.tc.Failf("Go client requests did not finish")
}

// field returns the values of the named field in f's field block.
func (f *rawFrame) field(name string) []string {
	var vv []string
	for _, hf := range f.Fields {
		if hf.Name == name {
			vv = append(vv, hf.Value)
		}
	}
	return vv
}

// resultField returns the values of the named (lowercase) response
// field in r.
func resultField(r *Result, name string) []string {
	var vv []string
	for _, kv := range r.Header {
		if kv[0] == name {
			vv = append(vv, kv[1])
		}
	}
	return vv
}

// sendWindows tracks the Go client's flow-control windows for data
// the scripted server sends.
type sendWindows struct {
	sc      *rawConn
	conn    int64
	initial int64
	streams map[uint32]int64
}

func newSendWindows(x *clientConfRun) *sendWindows {
	w := &sendWindows{sc: x.sc, conn: 65535, initial: 65535, streams: map[uint32]int64{}}
	if v, ok := x.clientSettings[setInitialWindowSize]; ok {
		w.initial = int64(v)
	}
	return w
}

func (w *sendWindows) apply(f *rawFrame) {
	incr := int64(binary.BigEndian.Uint32(f.Payload) & (1<<31 - 1))
	if f.Stream == 0 {
		w.conn += incr
	} else {
		w.streams[f.Stream] = w.stream(f.Stream) + incr
	}
}

func (w *sendWindows) stream(id uint32) int64 {
	if v, ok := w.streams[id]; ok {
		return v
	}
	return w.initial
}

// update applies the WINDOW_UPDATE frames received so far.
func (w *sendWindows) update() {
	for _, f := range w.sc.seen(isType(ftWindowUpdate)) {
		w.apply(f)
	}
	w.sc.mu.Lock()
	w.sc.pending = slices.DeleteFunc(w.sc.pending, isType(ftWindowUpdate))
	w.sc.mu.Unlock()
}

// send sends n bytes of DATA on stream as the windows permit, waiting
// up to timeout for the client to open them. It returns the number of
// bytes sent.
func (w *sendWindows) send(stream uint32, n int, endStream bool, timeout time.Duration) int {
	buf := make([]byte, 16384)
	deadline := time.Now().Add(timeout)
	sent := 0
	for sent < n {
		w.update()
		k := int(min(int64(len(buf)), int64(n-sent), w.conn, w.stream(stream)))
		if k <= 0 {
			left := time.Until(deadline)
			if left <= 0 {
				return sent
			}
			if f, _ := w.sc.waitFor(left, isType(ftWindowUpdate)); f != nil {
				w.apply(f)
			}
			continue
		}
		w.sc.writeData(stream, endStream && sent+k == n, buf[:k])
		sent += k
		w.conn -= int64(k)
		w.streams[stream] = w.stream(stream) - int64(k)
	}
	return sent
}

// countRequestData reads DATA frames on stream without granting flow
// control credit until n bytes have arrived, and returns the number
// of bytes received.
func countRequestData(sc *rawConn, stream uint32, n int) int {
	got := 0
	for got < n {
		f, _ := sc.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Type == ftData && f.Stream == stream })
		if f == nil {
			return got
		}
		got += len(f.Payload)
	}
	return got
}

// expectGoAwayBeforeClose checks that the client sends GOAWAY before
// it closes the connection (RFC 9113 §6.8, §9.1: SHOULD), waiting up
// to d for the client to close it.
func (y *clientMoreRun) expectGoAwayBeforeClose(sc *rawConn, d time.Duration, why string) {
	f, closed := sc.waitFor(d, isType(ftGoAway))
	switch {
	case f != nil:
		y.tc.Logf("client sent GOAWAY: %v", f)
		if _, closed := sc.waitFor(d, func(*rawFrame) bool { return false }); !closed {
			y.tc.Logf("connection still open %v after the client's GOAWAY", d)
		}
	case closed:
		y.tc.Warnf("Go client closed the connection (%s) without sending GOAWAY first (RFC 9113 §6.8, §9.1: endpoints SHOULD send GOAWAY before closing a connection)", why)
	default:
		y.tc.Errorf("Go client did not close the connection within %v (%s)", d, why)
	}
}

// fieldBlockFragment returns the field block fragment of a HEADERS frame.
func fieldBlockFragment(f *rawFrame) []byte {
	p := f.Payload
	if f.Flags&flagPadded != 0 && len(p) > 0 {
		pad := int(p[0])
		p = p[1:]
		if pad <= len(p) {
			p = p[:len(p)-pad]
		}
	}
	if f.Flags&flagPriority != 0 && len(p) >= 5 {
		p = p[5:]
	}
	return p
}

// hpackInt decodes an HPACK integer with an n-bit prefix (RFC 7541 §5.1).
func hpackInt(b []byte, n uint) (v uint64, rest []byte, ok bool) {
	if len(b) == 0 {
		return 0, nil, false
	}
	mask := uint64(1)<<n - 1
	v = uint64(b[0]) & mask
	b = b[1:]
	if v < mask {
		return v, b, true
	}
	for m := uint(0); len(b) > 0 && m < 63; m += 7 {
		c := b[0]
		b = b[1:]
		v += uint64(c&127) << m
		if c&128 == 0 {
			return v, b, true
		}
	}
	return 0, nil, false
}

// leadingSizeUpdates returns the dynamic table size updates at the
// start of a field block (RFC 7541 §6.3).
func leadingSizeUpdates(block []byte) []uint64 {
	var sizes []uint64
	for len(block) > 0 && block[0]&0xe0 == 0x20 {
		v, rest, ok := hpackInt(block, 5)
		if !ok {
			break
		}
		sizes = append(sizes, v)
		block = rest
	}
	return sizes
}

// tunnelClient returns a client driver that makes a CONNECT request
// (an extended CONNECT if protocol is set) for authority, writes "hi"
// on the tunnel, and reads the response body until it ends. It
// doesn't end the request body until the script finishes.
func tunnelClient(path, authority, protocol string) func(ctx context.Context, y *clientMoreRun) []*Result {
	return func(ctx context.Context, y *clientMoreRun) []*Result {
		pr, pw := io.Pipe()
		defer pw.Close()
		req := y.newRequest(ctx, "CONNECT", y.url+path, pr)
		if authority != "" {
			req.Host = authority
		}
		if protocol != "" {
			req.Header[":protocol"] = []string{protocol}
		}
		res := &Result{}
		resp, err := y.tr.RoundTrip(req)
		if err != nil {
			res.Error = err.Error()
			return []*Result{res}
		}
		defer resp.Body.Close()
		res.Status = resp.StatusCode
		res.Header = sortedFields(textproto.MIMEHeader(resp.Header))
		go pw.Write([]byte("hi"))
		b, err := io.ReadAll(resp.Body)
		res.Body = string(b)
		res.BodyLen = int64(len(b))
		if err != nil {
			res.Error = err.Error()
		}
		if len(resp.Trailer) > 0 {
			res.Trailer = sortedFields(textproto.MIMEHeader(resp.Trailer))
		}
		y.signalScript()
		y.waitScript(ctx)
		return []*Result{res}
	}
}

// expectTunnelData checks that the client sends "hi" on the tunnel.
func (y *clientMoreRun) expectTunnelData(sc *rawConn, stream uint32) {
	f, _ := sc.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Type == ftData && f.Stream == stream && len(f.Payload) > 0 })
	if f == nil {
		y.tc.Failf("client did not send data on the tunnel")
	} else if string(f.Payload) != "hi" {
		y.tc.Failf("tunnel data %q; want %q", f.Payload, "hi")
	}
}

// smallConnWindow is a Go client profile whose connection receive
// window is smaller than its stream receive window, so that one
// stream can use the whole connection window.
var smallConnWindow = &goProfile{
	Name: "small-conn-window",
	HTTP2: http.HTTP2Config{
		MaxReceiveBufferPerConnection: 65535,
		MaxReceiveBufferPerStream:     1 << 20,
	},
}

func strictMaxConcurrent(tr *http.Transport) { tr.HTTP2.StrictMaxConcurrentRequests = true }

// smallSocketBuffers makes the Go client use small TCP socket
// buffers, which disables their autotuning, so that a peer that
// floods the client sees its backpressure without first filling
// megabytes of kernel buffers.
func smallSocketBuffers(tr *http.Transport) {
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := new(net.Dialer).DialContext(ctx, network, addr)
		if tc, ok := c.(*net.TCPConn); ok {
			tc.SetReadBuffer(16 << 10)
			tc.SetWriteBuffer(16 << 10)
		}
		return c, err
	}
}

// maxConcurrentZero is the script of the SETTINGS_MAX_CONCURRENT_STREAMS=0
// cases. It sets the limit to 0 while the first request is open, and
// raises it after checking that the second request neither failed nor
// opened a stream on the connection. Using another connection for
// the second request is fine: that's what the Go client does with any
// exhausted limit, unless StrictMaxConcurrentRequests is set.
func maxConcurrentZero(y *clientMoreRun) {
	sc := y.sc
	if !y.syncSettings(sc, http2.Setting{ID: http2.SettingMaxConcurrentStreams, Val: 0}) {
		return
	}
	y.respond(sc, 1, "a")
	isReq := func(f *rawFrame) bool { return f.Type == ftHeaders && f.Fields != nil }
	serveNewConn := func(c net.Conn, when string) {
		y.tc.Logf("client opened a new connection for its second request %s", when)
		sc2 := newRawConn(y.tc, "server2", c)
		sc2.startReading()
		sc2.writeSettings()
		if f := y.waitRequest(sc2, 1); f != nil {
			y.respond(sc2, 1, "b")
		}
		y.expectRequestOK("")
	}
	// Give the client time to make (and misuse) its second request.
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case c := <-y.conns:
		serveNewConn(c, "while SETTINGS_MAX_CONCURRENT_STREAMS=0")
		return
	case y.got = <-y.results:
		y.tc.Warnf("Go client finished while SETTINGS_MAX_CONCURRENT_STREAMS=0; want the second request to wait (RFC 9113 §6.5.2: 0 SHOULD NOT be treated as special)")
		return
	case <-timer.C:
	}
	if fs := sc.seen(isReq); len(fs) > 0 {
		y.tc.Failf("client opened stream %d while SETTINGS_MAX_CONCURRENT_STREAMS=0 (RFC 9113 §5.1.2)", fs[0].Stream)
		return
	}
	y.tc.Logf("second request is waiting; raising SETTINGS_MAX_CONCURRENT_STREAMS")
	sc.writeSettings(http2.Setting{ID: http2.SettingMaxConcurrentStreams, Val: 100})
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		select {
		case c := <-y.conns:
			serveNewConn(c, "after the server raised SETTINGS_MAX_CONCURRENT_STREAMS")
			return
		default:
		}
		if f, _ := sc.waitFor(100*time.Millisecond, isReq); f != nil {
			y.respond(sc, f.Stream, "b")
			y.expectRequestOK("")
			return
		}
	}
	// With a nonzero limit, the request would be sent once another
	// stream ended; with 0, no stream can end.
	y.tc.Warnf("client did not send its waiting request within 5s after the server raised SETTINGS_MAX_CONCURRENT_STREAMS from 0 to 100, so 0 behaves as a permanent block (RFC 9113 §6.5.2: 0 SHOULD NOT be treated as special)")
}

// hpackError returns a script that sends the response field block
// blk and expects a COMPRESSION_ERROR.
func hpackError(blk ...byte) func(x *clientConfRun) {
	return func(x *clientConfRun) {
		x.sc.writeHeaders(1, true, true, blk)
		x.sc.expectConnError(http2.ErrCodeCompression)
		x.expectRequestError("HPACK decoding error")
	}
}

var clientMoreCases = []*clientMoreCase{
	// These cases test HPACK and the processing of field blocks.
	{
		clientConfCase: clientConfCase{
			name:    "hpack-missing-size-update-after-ack",
			covers:  "9113-4.3.1-2",
			desc:    "RFC 9113 §4.3.1: an endpoint MUST treat a field block that follows an acknowledgment of the reduction to the maximum dynamic table size as a connection error of type COMPRESSION_ERROR if it does not start with a conformant Dynamic Table Size Update instruction",
			profile: goProfiles[1], // constrained: SETTINGS_HEADER_TABLE_SIZE=256
			script: func(x *clientConfRun) {
				if v, ok := x.clientSettings[setHeaderTableSize]; !ok || v != 256 {
					x.tc.Errorf("client's SETTINGS_HEADER_TABLE_SIZE is %d (set=%v); want 256", v, ok)
					return
				}
				// The server acknowledged the client's SETTINGS in accept.
				// Its encoder still uses a 4096-byte table and
				// doesn't signal the reduction.
				x.sc.writeFields(1, true, ":status", "200", "x-big", strings.Repeat("b", 500), "x-a", "1")
				x.sc.expectConnError(http2.ErrCodeCompression)
				x.expectRequestError("field block without the required dynamic table size update")
			},
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "hpack-integer-too-many-octets",
			covers: "9113-4.3-3 7541-5.1-1",
			desc:   "RFC 7541 §5.1: integer encodings that exceed implementation limits, in value or octet length, MUST be treated as decoding errors (a name index of 15 padded with 20 continuation octets)",
			script: hpackError(append(append([]byte{0x88, 0x0f}, bytes.Repeat([]byte{0x80}, 20)...), 0x00, 0x01, 'x')...),
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "hpack-integer-overflow",
			covers: "9113-4.3-3 7541-5.1-1",
			desc:   "RFC 7541 §5.1: integer encodings that exceed implementation limits in value MUST be treated as decoding errors (an index above 2^64)",
			script: hpackError(append(append([]byte{0x88, 0xff}, bytes.Repeat([]byte{0xff}, 10)...), 0x7f)...),
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "hpack-huffman-padding-too-long",
			covers: "9113-4.3-3 7541-5.2-1",
			desc:   "RFC 7541 §5.2: a Huffman padding strictly longer than 7 bits MUST be treated as a decoding error",
			// Literal without indexing, name "x-a", Huffman value
			// "a" (00011) followed by 11 bits of 1s.
			script: hpackError(0x88, 0x00, 0x03, 'x', '-', 'a', 0x82, 0x1f, 0xff),
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "hpack-huffman-padding-not-eos",
			covers: "9113-4.3-3 7541-5.2-2",
			desc:   "RFC 7541 §5.2: a Huffman padding not corresponding to the most significant bits of the code for the EOS symbol MUST be treated as a decoding error",
			// Huffman value "a" (00011) followed by padding 000.
			script: hpackError(0x88, 0x00, 0x03, 'x', '-', 'a', 0x81, 0x18),
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "hpack-index-zero",
			covers: "9113-4.3-3 7541-6.1-1",
			desc:   "RFC 7541 §6.1: the index value of 0 is not used; it MUST be treated as a decoding error if found in an indexed header field representation",
			script: hpackError(0x88, 0x80),
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "hpack-duplicate-entries",
			covers: "7541-2.3.2-1",
			desc:   "RFC 7541 §2.3.2: the dynamic table can contain duplicate entries; duplicate entries MUST NOT be treated as an error by a decoder",
			script: func(x *clientConfRun) {
				// The block adds "x-dup: v" to the dynamic table
				// twice and then references both entries. It's
				// written by hand, so the server's encoder
				// mustn't be used afterwards on this connection.
				lit := []byte{0x40, 5, 'x', '-', 'd', 'u', 'p', 1, 'v'}
				blk := []byte{0x88}
				blk = append(blk, lit...)
				blk = append(blk, lit...)
				blk = append(blk, 0x80|62, 0x80|63)
				x.sc.writeHeaders(1, false, true, blk)
				x.sc.writeData(1, true, []byte("ok"))
				x.expectRequestOK("ok")
				if rs := x.result(); len(rs) > 0 && rs[0].Error == "" {
					if got := resultField(rs[0], "x-dup"); !slices.Equal(got, []string{"v", "v", "v", "v"}) {
						x.tc.Failf("response x-dup values %q; want [v v v v]", got)
					}
				}
			},
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "response-repeated-field-order",
			covers: "7541-2.1-2",
			desc:   "RFC 7541 §2.1: a decoder MUST order header fields in the decoded header list according to their ordering in the header block",
			script: func(x *clientConfRun) {
				x.sc.writeFields(1, false, ":status", "200", "x-a", "1", "x-b", "z", "x-a", "2", "content-length", "2", "x-a", "3")
				x.sc.writeData(1, true, []byte("ok"))
				x.expectRequestOK("ok")
				if rs := x.result(); len(rs) > 0 && rs[0].Error == "" {
					if got := resultField(rs[0], "x-a"); !slices.Equal(got, []string{"1", "2", "3"}) {
						x.tc.Failf("response x-a values %q; want [1 2 3]", got)
					}
				}
			},
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "header-table-size-changed-twice",
			covers: "7541-4.2-3",
			desc:   "RFC 7541 §4.2: if SETTINGS_HEADER_TABLE_SIZE changes more than once between field blocks, the smallest maximum table size that occurs in that interval MUST be signaled in a dynamic table size update",
			reqs:   []*Req{{Method: "GET", Path: "/a"}, {Method: "GET", Path: "/b"}},
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			sc.writeSettings(http2.Setting{ID: http2.SettingHeaderTableSize, Val: 0})
			if !y.syncSettings(sc, http2.Setting{ID: http2.SettingHeaderTableSize, Val: 4096}) {
				return
			}
			y.respond(sc, 1, "a")
			f := y.waitRequest(sc, 3)
			if f == nil {
				return
			}
			// The validator checks this too; see decodeFields.
			sizes := leadingSizeUpdates(fieldBlockFragment(f))
			y.tc.Logf("dynamic table size updates at the start of the second request: %v", sizes)
			if len(sizes) == 0 || slices.Min(sizes) != 0 {
				y.tc.Failf("second request's field block starts with size updates %v; want one to 0 (the smallest size since the previous field block)", sizes)
			}
			y.respond(sc, 3, "b")
			y.expectRequestOK("")
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "oversized-response-headers-hpack-state",
			covers: "9113-10.5.1-2",
			desc:   "RFC 9113 §10.5.1: a field block exceeding the client's limit MUST still be processed to ensure a consistent connection state, unless the connection is closed",
			reqs:   []*Req{{Method: "GET", Path: "/big"}, {Method: "GET", Path: "/next"}},
		},
		tweak: func(tr *http.Transport) { tr.MaxResponseHeaderBytes = 2048 },
		run: func(y *clientMoreRun) {
			sc := y.sc
			pad := func(i int) string { return strings.Repeat(strconv.Itoa(i), 300) }
			// About 3400 bytes of fields, each added to the dynamic
			// table, with x-ref added last.
			kv := []string{":status", "200"}
			for i := range 10 {
				kv = append(kv, fmt.Sprintf("x-pad-%d", i), pad(i))
			}
			kv = append(kv, "x-ref", "hello")
			sc.writeFields(1, true, kv...)
			f, closed := sc.waitFor(5*time.Second, func(f *rawFrame) bool { return f.Type == ftHeaders && f.Fields != nil && f.Stream == 3 })
			if f == nil {
				if !closed {
					y.tc.Failf("client did not send its second request")
					return
				}
				y.tc.Logf("client closed the connection after the oversized field block, which the requirement permits")
				sc2 := y.accept(false, nil)
				if sc2 == nil || y.waitRequest(sc2, 1) == nil {
					return
				}
				y.respond(sc2, 1, "next")
				y.result()
				return
			}
			// These are encoded as references to entries the
			// oversized block added.
			sc.writeFields(3, false, ":status", "200", "x-ref", "hello", "x-pad-3", pad(3), "content-length", "4")
			sc.writeData(3, true, []byte("next"))
			rs := y.result()
			if len(rs) < 2 {
				return
			}
			if rs[0].Error == "" {
				y.tc.Logf("client accepted the oversized response (status %d)", rs[0].Status)
			}
			switch r := rs[1]; {
			case r.Error != "":
				y.tc.Failf("second response failed after an oversized field block: %s", r.Error)
			case !slices.Equal(resultField(r, "x-ref"), []string{"hello"}) || !slices.Equal(resultField(r, "x-pad-3"), []string{pad(3)}):
				y.tc.Failf("second response's fields decoded wrongly: x-ref=%q x-pad-3 ok=%v", resultField(r, "x-ref"), slices.Equal(resultField(r, "x-pad-3"), []string{pad(3)}))
			}
		},
	},

	// These cases send HEADERS, PUSH_PROMISE, and CONTINUATION frames on stream 0.
	{
		clientConfCase: clientConfCase{
			name:   "headers-on-stream-0",
			covers: "9113-6.2-6",
			desc:   "RFC 9113 §6.2: a HEADERS frame with stream ID 0 MUST be treated as a connection error of type PROTOCOL_ERROR",
			script: connError(ftHeaders, flagEndHeaders|flagEndStream, 0, []byte{0x88}, http2.ErrCodeProtocol),
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "push-promise-on-stream-0",
			covers: "9113-6.6-7",
			desc:   "RFC 9113 §6.6: a PUSH_PROMISE frame with stream ID 0 MUST be treated as a connection error of type PROTOCOL_ERROR",
			script: connError(ftPushPromise, flagEndHeaders, 0, []byte{0, 0, 0, 2, 0x82, 0x86, 0x84}, http2.ErrCodeProtocol),
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "continuation-on-stream-0",
			covers: "9113-6.10-4",
			desc:   "RFC 9113 §6.10: a CONTINUATION frame with stream ID 0 MUST be treated as a connection error of type PROTOCOL_ERROR",
			script: func(x *clientConfRun) {
				blk := x.sc.block(":status", "200", "x-a", "1")
				x.sc.writeHeaders(1, true, false, blk[:1])
				x.sc.writeRaw(ftContinuation, flagEndHeaders, 0, blk[1:])
				x.sc.expectConnError(http2.ErrCodeProtocol)
			},
		},
	},

	// These cases test the handling of SETTINGS.
	{
		clientConfCase: clientConfCase{
			name:   "settings-no-rfc7540-priorities-invalid",
			covers: "9218-2.1-2",
			desc:   "RFC 9218 §2.1: a SETTINGS_NO_RFC7540_PRIORITIES value other than 0 or 1 MUST be treated as a connection error of type PROTOCOL_ERROR",
			script: connError(ftSettings, 0, 0, settingsPayload(setNoRFC7540Priorities, 2), http2.ErrCodeProtocol),
		},
	},
	{
		clientConfCase: clientConfCase{
			name:     "max-header-list-size-respected",
			covers:   "9113-6.5-2",
			desc:     "RFC 9113 §6.5: implementations MUST support all of the settings defined by the specification; a request larger than the server's advisory SETTINGS_MAX_HEADER_LIST_SIZE shouldn't be sent",
			settings: []http2.Setting{{ID: http2.SettingMaxHeaderListSize, Val: 1024}},
			reqs: []*Req{
				{Method: "GET", Path: "/small"},
				{Method: "GET", Path: "/big", Header: [][2]string{{"x-big", strings.Repeat("b", 2000)}}},
			},
		},
		run: func(y *clientMoreRun) {
			// By the time the client sees the response, it has
			// processed the server's SETTINGS.
			y.respond(y.sc, 1, "small")
			rs := y.result()
			if fs := y.sc.seen(func(f *rawFrame) bool { return f.Type == ftHeaders && f.Stream == 3 }); len(fs) > 0 {
				size := 0
				if fs[0].Fields != nil {
					for _, hf := range fs[0].Fields {
						size += int(hf.Size())
					}
				}
				y.tc.Warnf("client sent a request with a field section of %d bytes, exceeding the server's SETTINGS_MAX_HEADER_LIST_SIZE of 1024", size)
				y.sc.writeRST(3, http2.ErrCodeRefusedStream)
			}
			if len(rs) == 2 {
				if rs[0].Error != "" {
					y.tc.Failf("small request failed: %s", rs[0].Error)
				}
				y.tc.Logf("large request: err=%q", rs[1].Error)
			}
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "max-concurrent-streams-zero",
			covers: "9113-6.5.2-7",
			desc:   "RFC 9113 §6.5.2: a value of 0 for SETTINGS_MAX_CONCURRENT_STREAMS SHOULD NOT be treated as special; a new request waits (or uses another connection) like with any exhausted limit",
			reqs:   []*Req{{Method: "GET", Path: "/a"}, {Method: "GET", Path: "/b"}},
		},
		run: maxConcurrentZero,
	},
	{
		clientConfCase: clientConfCase{
			name:   "max-concurrent-streams-zero-strict",
			covers: "9113-6.5.2-7",
			desc:   "RFC 9113 §6.5.2: a value of 0 for SETTINGS_MAX_CONCURRENT_STREAMS SHOULD NOT be treated as special; with StrictMaxConcurrentRequests, a new request waits until the server raises the limit",
			reqs:   []*Req{{Method: "GET", Path: "/a"}, {Method: "GET", Path: "/b"}},
		},
		tweak: strictMaxConcurrent,
		run:   maxConcurrentZero,
	},
	{
		clientConfCase: clientConfCase{
			name:   "initial-window-increase-overflow",
			covers: "9113-6.9.2-3",
			desc:   "RFC 9113 §6.9.2: a change to SETTINGS_INITIAL_WINDOW_SIZE that causes any flow-control window to exceed the maximum size MUST be treated as a connection error of type FLOW_CONTROL_ERROR",
			reqs:   uploadReq(200000),
			script: func(x *clientConfRun) {
				// Let the client use its whole initial window (the
				// connection window is the same size), so that its
				// stream window stays where the server puts it.
				if n := countRequestData(x.sc, 1, 65535); n != 65535 {
					x.tc.Errorf("client sent %d bytes of its 65535-byte window", n)
					return
				}
				x.sc.writeWindowUpdate(1, 1<<31-1) // stream window: 2^31-1
				x.sc.writeSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 65536})
				x.sc.expectConnError(http2.ErrCodeFlowControl)
			},
		},
	},

	// These cases test the handling of PING.
	{
		clientConfCase: clientConfCase{
			name:   "ping-ack-not-answered",
			covers: "9113-6.7-5",
			desc:   "RFC 9113 §6.7: an endpoint MUST NOT respond to PING frames containing the ACK flag",
		},
		run: func(y *clientMoreRun) {
			unsolicited := [8]byte{'u', 'n', 's', 'o', 'l', 'i', 'c', 't'}
			y.sc.writePing(true, unsolicited)
			if !y.syncPing(y.sc, [8]byte{'a', 'f', 't', 'e', 'r'}) {
				return
			}
			if fs := y.sc.seen(func(f *rawFrame) bool { return f.Type == ftPing && bytes.Equal(f.Payload, unsolicited[:]) }); len(fs) > 0 {
				y.tc.Failf("client responded to a PING ACK: %v", fs[0])
			}
			y.respond(y.sc, 1, "ok")
			y.sc.expectNoError(200 * time.Millisecond)
			y.expectRequestOK("ok")
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "ping-while-body-unread",
			covers: "9113-5.2.2-1",
			desc:   "RFC 9113 §5.2.2: endpoints MUST read and process HTTP/2 frames from the TCP receive buffer as soon as data is available, even while the application isn't reading a response body",
		},
		client: func(ctx context.Context, y *clientMoreRun) []*Result {
			res := &Result{}
			resp, err := y.tr.RoundTrip(y.newRequest(ctx, "GET", y.url+"/", nil))
			if err != nil {
				res.Error = err.Error()
				return []*Result{res}
			}
			defer resp.Body.Close()
			res.Status = resp.StatusCode
			// Don't read the body until the script says so.
			y.signalScript()
			y.waitScript(ctx)
			b, err := io.ReadAll(resp.Body)
			res.Body, res.BodyLen = string(b), int64(len(b))
			if err != nil {
				res.Error = err.Error()
			}
			return []*Result{res}
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			const n = 32768
			sc.writeFields(1, false, ":status", "200", "content-length", strconv.Itoa(n+2))
			sc.writeData(1, false, make([]byte, n/2))
			if !y.waitClient("response headers") {
				return
			}
			sc.writeData(1, false, make([]byte, n/2))
			// The client must process this PING, queued behind
			// DATA that the application hasn't read.
			if !y.syncPing(sc, [8]byte{'u', 'n', 'r', 'e', 'a', 'd'}) {
				return
			}
			y.releaseClient()
			sc.writeData(1, true, []byte("ok"))
			if rs := y.result(); len(rs) > 0 {
				if rs[0].Error != "" || rs[0].BodyLen != n+2 {
					y.tc.Failf("response body: %d bytes, err=%q; want %d bytes", rs[0].BodyLen, rs[0].Error, n+2)
				}
			}
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "ping-settings-flood",
			covers: "9113-10.5-2",
			desc:   "RFC 9113 §10.5: implementations SHOULD track the use of features like PING and SETTINGS and set limits on their use; a client shouldn't queue unbounded responses to a peer that doesn't read them",
		},
		tweak: smallSocketBuffers,
		run:   pingSettingsFlood,
		// Ending the flood cleanly takes a while: the client
		// answers the queued PINGs one write at a time.
		timeout: 2 * time.Minute,
	},

	// These cases test GOAWAY and closing connections.
	{
		clientConfCase: clientConfCase{
			name:   "goaway-before-close-idle",
			covers: "9113-5.4.1-4 9113-6.8-2 9113-9.1-3",
			desc:   "RFC 9113 §6.8, §9.1: endpoints SHOULD send a GOAWAY frame before closing a connection (here, when Transport.CloseIdleConnections closes it)",
		},
		client: func(ctx context.Context, y *clientMoreRun) []*Result {
			res := y.roundTrip(y.newRequest(ctx, "GET", y.url+"/", nil))
			y.tr.CloseIdleConnections()
			return []*Result{res}
		},
		run: func(y *clientMoreRun) {
			y.respond(y.sc, 1, "ok")
			y.expectGoAwayBeforeClose(y.sc, reactionTimeout, "after CloseIdleConnections")
			y.expectRequestOK("ok")
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "goaway-before-idle-timeout-close",
			covers: "9113-5.4.1-4 9113-6.8-2 9113-9.1-3",
			desc:   "RFC 9113 §6.8, §9.1: endpoints SHOULD send a GOAWAY frame before closing a connection (here, when Transport.IdleConnTimeout expires)",
		},
		tweak: func(tr *http.Transport) { tr.IdleConnTimeout = 300 * time.Millisecond },
		run: func(y *clientMoreRun) {
			y.respond(y.sc, 1, "ok")
			y.expectRequestOK("ok")
			y.expectGoAwayBeforeClose(y.sc, reactionTimeout, "on idle timeout")
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "goaway-after-server-goaway",
			covers: "9113-6.8-3",
			desc:   "RFC 9113 §6.8: a receiver of a GOAWAY that has no more use for the connection SHOULD still send a GOAWAY frame before terminating the connection",
		},
		run: func(y *clientMoreRun) {
			y.sc.writeGoAway(1, http2.ErrCodeNo)
			y.respond(y.sc, 1, "ok")
			y.expectRequestOK("ok")
			y.expectGoAwayBeforeClose(y.sc, reactionTimeout, "after the server's GOAWAY, with no streams left")
		},
	},

	// These cases test DATA, WINDOW_UPDATE, and stream states.
	{
		clientConfCase: clientConfCase{
			name:   "padded-data-and-headers",
			covers: "9113-6.1-1",
			desc:   "RFC 9113 §6.1, §6.2: DATA frames (and HEADERS frames) MAY contain padding",
			script: func(x *clientConfRun) {
				pad := func(n int, content []byte) []byte {
					p := append([]byte{byte(n)}, content...)
					return append(p, make([]byte, n)...)
				}
				blk := x.sc.block(":status", "200", "content-length", "5")
				x.sc.writeRaw(ftHeaders, flagEndHeaders|flagPadded, 1, pad(10, blk))
				x.sc.writeRaw(ftData, flagPadded, 1, pad(20, []byte("hel")))
				x.sc.writeRaw(ftData, flagPadded, 1, pad(255, nil))
				x.sc.writeRaw(ftData, flagPadded|flagEndStream, 1, pad(0, []byte("lo")))
				x.sc.expectNoError(200 * time.Millisecond)
				x.expectRequestOK("hello")
			},
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "data-on-closed-stream",
			covers: "9113-6.1-6",
			desc:   "RFC 9113 §6.1: a DATA frame on a stream not in the \"open\" or \"half-closed (local)\" state MUST be treated as a stream error of type STREAM_CLOSED (RFC 9113 §5.1 also permits a connection error)",
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			y.respond(sc, 1, "ok")
			// Both endpoints have sent END_STREAM: the stream is closed.
			sc.writeData(1, false, []byte("junk"))
			f, closed := sc.waitFor(reactionTimeout, func(f *rawFrame) bool {
				return f.Type == ftGoAway || (f.Type == ftRSTStream && f.Stream == 1)
			})
			switch {
			case f != nil && f.Type == ftRSTStream && f.ErrCode() == http2.ErrCodeStreamClosed:
				y.tc.Logf("got expected stream error: %v", f)
			case f != nil && f.Type == ftGoAway && (f.ErrCode() == http2.ErrCodeStreamClosed || f.ErrCode() == http2.ErrCodeProtocol):
				y.tc.Logf("got connection error: %v", f)
			case f != nil:
				y.tc.Failf("got %v; want RST_STREAM STREAM_CLOSED", f)
			case closed:
				y.tc.Failf("connection closed; want RST_STREAM STREAM_CLOSED")
			default:
				y.tc.Failf("client ignored DATA on a closed stream; want RST_STREAM STREAM_CLOSED within %v", reactionTimeout)
			}
			y.expectRequestOK("ok")
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "window-update-after-end-stream",
			covers: "9113-6.9-4 9113-6.9-d1",
			desc:   "RFC 9113 §6.9: a WINDOW_UPDATE frame on a half-closed or closed stream (from a peer that has sent END_STREAM) MUST NOT be treated as an error",
			reqs:   []*Req{{Method: "GET", Path: "/a"}, {Method: "GET", Path: "/b"}},
			script: func(x *clientConfRun) {
				x.sc.writeWindowUpdate(1, 1000) // client is half-closed (local)
				x.respond(x.sc, 1, "a")
				x.sc.writeWindowUpdate(1, 1000) // stream 1 is closed
				if f := x.waitRequest(x.sc, 3); f == nil {
					return
				}
				x.respond(x.sc, 3, "b")
				x.sc.expectNoError(200 * time.Millisecond)
				x.expectRequestOK("")
			},
		},
	},
	{
		clientConfCase: clientConfCase{
			name:    "data-after-rst-counts-against-connection-window",
			covers:  "9113-6.9-5 9113-5.1-d1",
			desc:    "RFC 9113 §6.9: a receiver MUST always account for a flow-controlled frame's contribution against the connection flow-control window (including DATA on a stream it reset), and so return that credit",
			profile: smallConnWindow,
			reqs:    []*Req{{Method: "GET", Path: "/cancel", CancelAfter: 1}, {Method: "GET", Path: "/after"}},
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			w := newSendWindows(y.clientConfRun)
			w.update()
			y.tc.Logf("client windows: connection %d, stream %d", w.conn, w.initial)
			sc.writeFields(1, false, ":status", "200")
			w.send(1, 1000, false, time.Second)
			if f, _ := sc.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Type == ftRSTStream && f.Stream == 1 }); f == nil {
				y.tc.Errorf("client did not reset the canceled stream")
				return
			}
			// Use the rest of the connection window on the reset
			// stream, as if this DATA had been in flight.
			w.update()
			junk := int(w.conn)
			w.send(1, junk, false, time.Second)
			y.tc.Logf("sent %d bytes on the reset stream", junk)
			f := y.waitRequest(sc, 3)
			if f == nil {
				return
			}
			const n = 131070 // the client's entire connection window
			sc.writeFields(3, false, ":status", "200", "content-length", strconv.Itoa(n))
			if sent := w.send(3, n, true, 5*time.Second); sent != n {
				y.tc.Failf("client's connection window stayed closed after %d of %d response bytes: it didn't return connection flow-control credit for the %d bytes of DATA on the reset stream", sent, n, junk)
				return
			}
			if rs := y.result(); len(rs) == 2 && (rs[1].Error != "" || rs[1].BodyLen != n) {
				y.tc.Failf("second response: %d bytes, err=%q; want %d bytes", rs[1].BodyLen, rs[1].Error, n)
			}
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "unknown-error-codes",
			covers: "9113-7-1",
			desc:   "RFC 9113 §7: unknown or unsupported error codes (in RST_STREAM and GOAWAY) MUST NOT trigger any special behavior",
			reqs:   []*Req{{Method: "GET", Path: "/1"}, {Method: "GET", Path: "/2"}, {Method: "GET", Path: "/3"}},
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			sc.writeRST(1, 0xff)
			f := y.waitRequest(sc, 3)
			if f == nil {
				return
			}
			// Stream 3 is at or below the GOAWAY's last stream ID,
			// so its response is still accepted.
			sc.writeGoAway(3, 0xabcd)
			y.respond(sc, 3, "two")
			sc2 := y.accept(false, nil)
			if sc2 == nil || y.waitRequest(sc2, 1) == nil {
				return
			}
			y.respond(sc2, 1, "three")
			rs := y.result()
			if len(rs) != 3 {
				return
			}
			if rs[0].Error == "" {
				y.tc.Failf("request reset with error code 0xff succeeded")
			}
			for i, want := range []string{"", "two", "three"} {
				if i > 0 && (rs[i].Error != "" || rs[i].Body != want) {
					y.tc.Failf("request %d: body %q, err=%q; want %q", i+1, rs[i].Body, rs[i].Error, want)
				}
			}
		},
	},

	// These cases test the fields of requests.
	{
		clientConfCase: clientConfCase{
			name:   "request-connection-specific-fields",
			covers: "9113-8.2.2-1",
			desc:   "RFC 9113 §8.2.2: an endpoint MUST NOT generate an HTTP/2 message containing connection-specific header fields, even if the application sets them",
			reqs: []*Req{
				{Method: "GET", Path: "/1", Header: [][2]string{
					{"connection", "keep-alive"}, {"keep-alive", "timeout=5"}, {"proxy-connection", "keep-alive"},
					{"transfer-encoding", "chunked"}, {"upgrade", ""},
				}},
				{Method: "GET", Path: "/2", Header: [][2]string{{"upgrade", "websocket"}}},
				{Method: "GET", Path: "/3", Header: [][2]string{{"connection", "close"}}},
			},
		},
		run: func(y *clientMoreRun) {
			y.serveAll(y.sc, func(f *rawFrame) {
				for _, name := range []string{"connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade"} {
					if vv := f.field(name); len(vv) > 0 {
						y.tc.Failf("request on stream %d includes connection-specific field %s: %q", f.Stream, name, vv)
					}
				}
			})
			rs := y.result()
			for i, r := range rs {
				y.tc.Logf("request %d: status=%d err=%q", i+1, r.Status, r.Error)
			}
			if len(rs) == 3 && (rs[0].Error != "" || rs[2].Error != "") {
				y.tc.Failf("requests with ignorable connection-specific fields failed: %q, %q", rs[0].Error, rs[2].Error)
			}
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "request-te-not-trailers",
			covers: "9113-8.2.2-3",
			desc:   "RFC 9113 §8.2.2: the TE header field MAY be present in an HTTP/2 request; when it is, it MUST NOT contain any value other than \"trailers\"",
			reqs: []*Req{
				{Method: "GET", Path: "/trailers", Header: [][2]string{{"te", "trailers"}}},
				{Method: "GET", Path: "/gzip", Header: [][2]string{{"te", "gzip"}}},
				{Method: "GET", Path: "/both", Header: [][2]string{{"te", "trailers, deflate"}}},
			},
		},
		run: func(y *clientMoreRun) {
			y.serveAll(y.sc, func(f *rawFrame) {
				for _, v := range f.field("te") {
					if v != "trailers" {
						y.tc.Failf("request %s includes te: %q", f.field(":path"), v)
					}
				}
			})
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "request-host-matches-authority",
			covers: "9113-8.3.1-3",
			desc:   "RFC 9113 §8.3.1: clients MUST NOT generate a request with a Host header field that differs from the \":authority\" pseudo-header field",
		},
		client: func(ctx context.Context, y *clientMoreRun) []*Result {
			r1 := y.newRequest(ctx, "GET", y.url+"/header", nil)
			r1.Header.Set("Host", "header.example")
			r2 := y.newRequest(ctx, "GET", y.url+"/host", nil)
			r2.Host = "virtual.example:8080"
			r3 := y.newRequest(ctx, "GET", y.url+"/both", nil)
			r3.Host = "virtual.example"
			r3.Header.Set("Host", "header.example")
			return []*Result{y.roundTrip(r1), y.roundTrip(r2), y.roundTrip(r3)}
		},
		run: func(y *clientMoreRun) {
			y.serveAll(y.sc, func(f *rawFrame) {
				auth := f.field(":authority")
				for _, h := range f.field("host") {
					if len(auth) != 1 || h != auth[0] {
						y.tc.Failf("request %s: host %q differs from :authority %q", f.field(":path"), h, auth)
					}
				}
				y.tc.Logf("request %s: :authority %q, host %q", f.field(":path"), auth, f.field("host"))
			})
			y.expectRequestOK("ok")
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "request-authority-without-userinfo",
			covers: "9113-8.3.1-9",
			desc:   "RFC 9113 §8.3.1: \":authority\" MUST NOT include the deprecated userinfo subcomponent for \"http\" or \"https\" schemed URIs",
		},
		client: func(ctx context.Context, y *clientMoreRun) []*Result {
			u := strings.Replace(y.url, "http://", "http://user:secret@", 1) + "/"
			return []*Result{y.roundTrip(y.newRequest(ctx, "GET", u, nil))}
		},
		run: func(y *clientMoreRun) {
			y.serveAll(y.sc, func(f *rawFrame) {
				for _, name := range []string{":authority", "host"} {
					for _, v := range f.field(name) {
						if strings.Contains(v, "@") || strings.Contains(v, "secret") {
							y.tc.Failf("%s %q includes userinfo", name, v)
						}
					}
				}
			})
			y.expectRequestOK("ok")
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "request-empty-path",
			covers: "9113-8.3.1-10",
			desc:   "RFC 9113 §8.3.1: \":path\" MUST NOT be empty for \"http\" or \"https\" URIs; URIs without a path component MUST include a value of '/'",
		},
		client: func(ctx context.Context, y *clientMoreRun) []*Result {
			return []*Result{
				y.roundTrip(y.newRequest(ctx, "GET", y.url, nil)),
				y.roundTrip(y.newRequest(ctx, "GET", y.url+"?q=1", nil)),
			}
		},
		run: func(y *clientMoreRun) {
			want := []string{"/", "/?q=1"}
			i := 0
			y.serveAll(y.sc, func(f *rawFrame) {
				if got := f.field(":path"); i < len(want) && !slices.Equal(got, want[i:i+1]) {
					y.tc.Failf("request %d: :path %q; want %q", i+1, got, want[i])
				}
				i++
			})
			y.expectRequestOK("ok")
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "request-options-asterisk",
			covers: "9113-8.3.1-11",
			desc:   "RFC 9113 §8.3.1: an OPTIONS request for an \"http\" or \"https\" URI that does not include a path component MUST include a \":path\" pseudo-header field with a value of '*'",
		},
		client: func(ctx context.Context, y *clientMoreRun) []*Result {
			r1 := y.newRequest(ctx, "OPTIONS", y.url, nil)
			r1.URL.Opaque = "*"
			r2 := y.newRequest(ctx, "OPTIONS", y.url, nil)
			r2.URL.Path = "*"
			return []*Result{y.roundTrip(r1), y.roundTrip(r2)}
		},
		run: func(y *clientMoreRun) {
			y.serveAll(y.sc, func(f *rawFrame) {
				if m, p := f.field(":method"), f.field(":path"); !slices.Equal(m, []string{"OPTIONS"}) || !slices.Equal(p, []string{"*"}) {
					y.tc.Failf("stream %d: :method %q :path %q; want OPTIONS *", f.Stream, m, p)
				}
			})
			y.expectRequestOK("ok")
		},
	},

	// These cases test CONNECT requests.
	{
		clientConfCase: clientConfCase{
			name:   "connect-pseudo-headers",
			covers: "9113-8.5-1",
			desc:   "RFC 9113 §8.5: in a CONNECT request, the \":scheme\" and \":path\" pseudo-header fields MUST be omitted, and \":authority\" contains the host and port to connect to",
		},
		client: tunnelClient("/", "tunnel.example:443", ""),
		run: func(y *clientMoreRun) {
			sc, f := y.sc, y.req
			if m := f.field(":method"); !slices.Equal(m, []string{"CONNECT"}) {
				y.tc.Failf(":method %q; want CONNECT", m)
			}
			for _, name := range []string{":scheme", ":path"} {
				if vv := f.field(name); len(vv) > 0 {
					y.tc.Failf("CONNECT request includes %s %q", name, vv)
				}
			}
			if a := f.field(":authority"); !slices.Equal(a, []string{"tunnel.example:443"}) {
				y.tc.Failf(":authority %q; want tunnel.example:443", a)
			}
			sc.writeFields(1, false, ":status", "200")
			y.expectTunnelData(sc, 1)
			sc.writeData(1, true, []byte("ho"))
			if !y.waitClient("tunnel response read") {
				return
			}
			y.releaseClient()
			if rs := y.result(); len(rs) > 0 && (rs[0].Status != 200 || rs[0].Body != "ho") {
				y.tc.Failf("tunnel: status %d, data %q, err=%q; want 200, %q", rs[0].Status, rs[0].Body, rs[0].Error, "ho")
			}
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "connect-tunnel-headers",
			covers: "9113-8.5-2",
			desc:   "RFC 9113 §8.5: frame types other than DATA or stream management frames MUST NOT be sent on a connected stream and MUST be treated as a stream error if received (here, a HEADERS frame after the 2xx response)",
		},
		client: tunnelClient("/", "tunnel.example:443", ""),
		run: func(y *clientMoreRun) {
			sc := y.sc
			sc.writeFields(1, false, ":status", "200")
			y.expectTunnelData(sc, 1)
			sc.writeData(1, false, []byte("ho"))
			sc.writeFields(1, true, "x-trailer", "1")
			f, closed := sc.waitFor(reactionTimeout, func(f *rawFrame) bool {
				return f.Type == ftGoAway || (f.Type == ftRSTStream && f.Stream == 1)
			})
			switch {
			case f != nil && f.ErrCode() != http2.ErrCodeNo:
				y.tc.Logf("got expected error: %v", f)
			case f != nil:
				y.tc.Failf("got %v; want a stream error", f)
			case closed:
				y.tc.Failf("connection closed; want a stream error")
			default:
				y.tc.Failf("client did not treat HEADERS on an established CONNECT tunnel as a stream error within %v", reactionTimeout)
			}
			if y.waitClient("tunnel response read") {
				y.releaseClient()
				if rs := y.result(); len(rs) > 0 {
					y.tc.Logf("client's tunnel read: data %q, trailer %q, err=%q", rs[0].Body, rs[0].Trailer, rs[0].Error)
				}
			}
		},
	},
	{
		clientConfCase: clientConfCase{
			name:     "extended-connect",
			desc:     "RFC 8441 §3, §4: after SETTINGS_ENABLE_CONNECT_PROTOCOL=1, a client MAY use the Extended CONNECT, which has :method CONNECT, a :protocol, and :scheme, :path, and :authority",
			settings: []http2.Setting{{ID: http2.SettingEnableConnectProtocol, Val: 1}},
		},
		client: tunnelClient("/chat?room=1", "", "websocket"),
		noConnect: func(y *clientMoreRun) {
			// Extended CONNECT is optional (MAY), but if the Go
			// client doesn't use it, the reason should be that
			// it doesn't support it.
			if len(y.got) > 0 && strings.Contains(y.got[0].Error, ":protocol") {
				y.tc.Skipf("the Go client doesn't support Extended CONNECT: RoundTrip failed before connecting: %s", y.got[0].Error)
				return
			}
			y.tc.Failf("Go client finished without connecting: %v", y.got)
		},
		run: func(y *clientMoreRun) {
			sc, f := y.sc, y.req
			auth := strings.TrimPrefix(y.url, "http://")
			for _, want := range [][2]string{{":method", "CONNECT"}, {":protocol", "websocket"}, {":scheme", "http"}, {":path", "/chat?room=1"}, {":authority", auth}} {
				if got := f.field(want[0]); !slices.Equal(got, want[1:]) {
					y.tc.Failf("extended CONNECT %s %q; want %q", want[0], got, want[1])
				}
			}
			sc.writeFields(1, false, ":status", "200")
			y.expectTunnelData(sc, 1)
			sc.writeData(1, true, []byte("ho"))
			if !y.waitClient("tunnel response read") {
				return
			}
			y.releaseClient()
			if rs := y.result(); len(rs) > 0 && (rs[0].Status != 200 || rs[0].Body != "ho") {
				y.tc.Failf("tunnel: status %d, data %q, err=%q; want 200, %q", rs[0].Status, rs[0].Body, rs[0].Error, "ho")
			}
		},
	},
}

// pingSettingsFlood sends the client PING frames (and some SETTINGS
// frames) without reading the client's responses, and checks that the
// client stops reading (or closes the connection) rather than queueing
// responses without limit.
func pingSettingsFlood(y *clientMoreRun) {
	sc := y.sc
	y.respond(sc, 1, "ok")
	y.expectRequestOK("ok")
	// Small socket buffers (see smallSocketBuffers for the client's)
	// make the client's backpressure visible sooner.
	if tc, ok := sc.conn.(*net.TCPConn); ok {
		tc.SetReadBuffer(16 << 10)
		tc.SetWriteBuffer(16 << 10)
	}
	// The server's frame reader stops reading once its queue is
	// full, since nothing consumes it until the flood ends.
	const (
		maxPings   = 16 << 20 // 272 MB of PING frames
		chunkPings = 4096
	)
	sent, stalled := 0, false
	var werr error
	var rest []byte // the unwritten part of the chunk whose write stalled
	start := time.Now()
	buf := make([]byte, 0, chunkPings*17+chunkPings/64*9)
	sc.wmu.Lock()
	defer sc.wmu.Unlock()
	for sent < maxPings {
		buf = buf[:0]
		for i := range chunkPings {
			if i%64 == 0 {
				buf = append(buf, 0, 0, 0, ftSettings, 0, 0, 0, 0, 0)
			}
			buf = append(buf, 0, 0, 8, ftPing, 0, 0, 0, 0, 0)
			buf = binary.BigEndian.AppendUint64(buf, uint64(sent+i)|0xf1<<56)
		}
		sc.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		var n int
		if n, werr = sc.conn.Write(buf); werr != nil {
			stalled = errors.Is(werr, os.ErrDeadlineExceeded)
			rest = buf[n:]
			break
		}
		sent += chunkPings
	}
	mb := float64(sent*17) / (1 << 20)
	switch {
	case stalled:
		y.tc.Logf("server's writes stalled after %d PINGs (%.1f MB) in %v: the client stopped reading while its responses were unread", sent, mb, time.Since(start).Round(time.Millisecond))
	case werr != nil:
		y.tc.Logf("client closed the connection after %d PINGs (%.1f MB): %v", sent, mb, werr)
	default:
		y.tc.Warnf("client kept reading %d PINGs and %d SETTINGS (%.1f MB) while the server read none of its responses: it appears to queue them without limit (RFC 9113 §10.5: SHOULD limit)", sent, sent/64, mb)
	}

	// End the flood cleanly, so that the validator doesn't see a
	// frame cut off by an abrupt close: finish the partly written
	// chunk, and read and discard the client's responses until it
	// answers a final PING. When the writes stalled, the server's
	// frame reader has long been blocked on its full queue, so
	// reading the connection here doesn't race with it.
	if stalled && len(sc.frames) == cap(sc.frames) {
		final := [8]byte{'f', 'l', 'o', 'o', 'd', 'e', 'n', 'd'}
		done := make(chan error, 1)
		go func() { done <- discardUntilPingAck(sc.conn, final) }()
		sc.conn.SetWriteDeadline(time.Now().Add(90 * time.Second))
		rest = append(rest, 0, 0, 8, ftPing, 0, 0, 0, 0, 0)
		rest = append(rest, final[:]...)
		if _, err := sc.conn.Write(rest); err != nil {
			y.tc.Logf("finishing the flood: %v", err)
		} else if err := <-done; err != nil {
			y.tc.Logf("reading the client's responses after the flood: %v", err)
		} else {
			y.tc.Logf("client answered all PINGs once the server read its responses, %v after the flood started", time.Since(start).Round(time.Millisecond))
		}
	}
	sc.conn.Close()
	// Let the server's frame reader finish, now that it can't read more.
	go func() {
		for range sc.frames {
		}
	}()
}

// discardUntilPingAck reads frames from c until a PING ACK with data.
func discardUntilPingAck(c net.Conn, data [8]byte) error {
	c.SetReadDeadline(time.Now().Add(90 * time.Second))
	br := bufio.NewReaderSize(c, 64<<10)
	hdr := make([]byte, 9)
	payload := make([]byte, 1<<14)
	for {
		if _, err := io.ReadFull(br, hdr); err != nil {
			return err
		}
		n := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
		if n > len(payload) {
			payload = make([]byte, n)
		}
		if _, err := io.ReadFull(br, payload[:n]); err != nil {
			return err
		}
		if hdr[3] == ftPing && hdr[4]&flagAck != 0 && bytes.Equal(payload[:n], data[:]) {
			return nil
		}
	}
}
