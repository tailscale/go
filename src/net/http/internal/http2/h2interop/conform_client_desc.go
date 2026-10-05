// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/internal/http2"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2/hpack"
)

// This file has frame-level conformance tests of the Go client for
// requirements that RFC 9113 and RFC 7541 state descriptively,
// without BCP 14 keywords (the "-d" requirements in requirements.txt).
// The cases are run by runClientMore (see conform_client_more.go).
//
// Many of the HPACK cases write field blocks by hand with cdHPACK, so
// that they control exactly which representations and dynamic table
// entries are used. After a hand-written block, a connection must not
// use rawConn's own encoder (writeFields, respond), which doesn't
// know about the entries the hand-written block added.
//
// The cases are registered by this file's init function, which runs
// after conform_client.go's (files are initialized in file name
// order), so that they are registered as tests exactly once.

func init() {
	for _, c := range clientDescCases {
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

// cdHPACK builds an HPACK field block (RFC 7541 §6) by hand.
type cdHPACK struct{ b []byte }

func cdBlock() *cdHPACK { return &cdHPACK{} }

// int appends an integer with an n-bit prefix whose other bits are
// flags (RFC 7541 §5.1).
func (h *cdHPACK) int(flags byte, n uint, v int) *cdHPACK {
	max := 1<<n - 1
	if v < max {
		h.b = append(h.b, flags|byte(v))
		return h
	}
	h.b = append(h.b, flags|byte(max))
	for v -= max; v >= 128; v >>= 7 {
		h.b = append(h.b, byte(v&127)|0x80)
	}
	h.b = append(h.b, byte(v))
	return h
}

// str appends a string literal (RFC 7541 §5.2), Huffman-encoded if
// huff is set.
func (h *cdHPACK) str(s string, huff bool) *cdHPACK {
	if huff {
		h.int(0x80, 7, int(hpack.HuffmanEncodeLength(s)))
		h.b = hpack.AppendHuffmanString(h.b, s)
		return h
	}
	h.int(0, 7, len(s))
	h.b = append(h.b, s...)
	return h
}

// raw appends bytes as is.
func (h *cdHPACK) raw(b ...byte) *cdHPACK {
	h.b = append(h.b, b...)
	return h
}

// indexed appends an indexed field (RFC 7541 §6.1).
func (h *cdHPACK) indexed(i int) *cdHPACK { return h.int(0x80, 7, i) }

// status200 appends ":status: 200" (static table index 8).
func (h *cdHPACK) status200() *cdHPACK { return h.indexed(8) }

// insert appends a literal field with incremental indexing and a new
// name (RFC 7541 §6.2.1), which adds the field to the dynamic table.
func (h *cdHPACK) insert(name, value string, huff bool) *cdHPACK {
	h.b = append(h.b, 0x40)
	return h.str(name, huff).str(value, huff)
}

// insertNameIdx appends a literal field with incremental indexing
// whose name is that of the table entry at index nameIdx.
func (h *cdHPACK) insertNameIdx(nameIdx int, value string, huff bool) *cdHPACK {
	return h.int(0x40, 6, nameIdx).str(value, huff)
}

// sizeUpdate appends a dynamic table size update (RFC 7541 §6.3).
func (h *cdHPACK) sizeUpdate(n int) *cdHPACK { return h.int(0x20, 5, n) }

// cdEntrySize returns the size of a dynamic table entry (RFC 7541 §4.1).
func cdEntrySize(name, value string) int { return len(name) + len(value) + 32 }

// cdStep is one response of a cdHPACKCase: a field block, sent in one
// HEADERS frame with END_STREAM, and how the client must decode it.
type cdStep struct {
	block []byte
	// want are the response fields the client must report, by
	// lowercase name. A nil value means the field must be absent.
	want map[string][]string
	// decodeErr means the block must be rejected as a decoding
	// error: a connection error of type COMPRESSION_ERROR (RFC
	// 9113 §4.3).
	decodeErr bool
}

// cdGets returns n GET requests.
func cdGets(n int) []*Req {
	var rs []*Req
	for i := range n {
		rs = append(rs, &Req{Method: "GET", Path: "/" + strconv.Itoa(i+1)})
	}
	return rs
}

// cdHPACKCase returns a case in which the Go client makes one request
// per step, sequentially, and the scripted server answers each with
// its step's field block.
func cdHPACKCase(name, covers, desc string, steps ...cdStep) *clientMoreCase {
	return &clientMoreCase{
		clientConfCase: clientConfCase{
			name:   name,
			covers: covers,
			desc:   desc,
			reqs:   cdGets(len(steps)),
		},
		run: func(y *clientMoreRun) {
			if v, ok := y.clientSettings[setHeaderTableSize]; ok && v != 4096 {
				y.tc.Errorf("client's SETTINGS_HEADER_TABLE_SIZE is %d; the case assumes 4096", v)
				return
			}
			cdRunSteps(y, y.sc, 1, steps)
		},
	}
}

// cdRunSteps sends the steps' field blocks as responses on sc,
// starting with stream first, whose request has already arrived, and
// checks the client's results.
func cdRunSteps(y *clientMoreRun, sc *rawConn, first uint32, steps []cdStep) {
	for i, st := range steps {
		stream := first + uint32(2*i)
		if i > 0 && !cdAwaitRequest(y, sc, stream, fmt.Sprintf("response %d", i)) {
			return
		}
		sc.writeHeaders(stream, true, true, st.block)
		if st.decodeErr {
			sc.expectConnError(http2.ErrCodeCompression)
			rs := y.result()
			if len(rs) > i && rs[i].Error == "" {
				y.tc.Failf("response %d: client accepted a field block that must be a decoding error: fields %q", i+1, rs[i].Header)
			}
			cdCheckSteps(y, rs, steps[:i])
			return
		}
	}
	cdCheckSteps(y, y.result(), steps)
}

// cdAwaitRequest waits for a request on stream, failing the test if
// the client sends GOAWAY (as it would for a field block it couldn't
// decode) or no request. After describes what the server sent last.
// On failure, it closes the listener, so that the client's remaining
// requests fail quickly.
func cdAwaitRequest(y *clientMoreRun, sc *rawConn, stream uint32, after string) bool {
	f, closed := sc.waitFor(5*time.Second, func(f *rawFrame) bool {
		return f.Type == ftGoAway || (f.Fields != nil && f.Stream == stream)
	})
	switch {
	case f == nil:
		y.tc.Failf("client did not send its request on stream %d after %s (closed=%v)", stream, after, closed)
	case f.Type == ftGoAway:
		y.tc.Failf("client sent %v after %s, instead of its request on stream %d", f, after, stream)
	default:
		return true
	}
	y.ln.Close()
	return false
}

// cdCheckSteps checks the client's results against the steps.
func cdCheckSteps(y *clientMoreRun, rs []*Result, steps []cdStep) {
	for i, st := range steps {
		if i >= len(rs) {
			return
		}
		r := rs[i]
		if r.Error != "" || r.Status != 200 {
			y.tc.Failf("response %d: status %d, err=%q; want status 200", i+1, r.Status, r.Error)
			continue
		}
		for _, name := range sortedKeys(st.want) {
			want := st.want[name]
			if got := resultField(r, name); !slices.Equal(got, want) {
				y.tc.Failf("response %d: field %s decoded as %s; want %s", i+1, name, cdValues(got), cdValues(want))
			}
		}
	}
}

// cdValues formats field values for messages, abbreviating long ones.
func cdValues(vv []string) string {
	var s []string
	for _, v := range vv {
		if len(v) > 40 {
			v = fmt.Sprintf("%s...(%d bytes)", v[:20], len(v))
		}
		s = append(s, strconv.Quote(v))
	}
	return "[" + strings.Join(s, " ") + "]"
}

// cdNextConnRequest waits up to d for the Go client to open another
// connection, performs the server side of its preface, and waits for
// a request on stream 1. It returns nil if no connection arrives, and
// a nil frame if no request arrives.
func cdNextConnRequest(y *clientMoreRun, d time.Duration) (*rawConn, *rawFrame) {
	select {
	case c := <-y.conns:
		y.conns <- c // for accept
	case <-time.After(d):
		return nil, nil
	}
	sc := y.accept(false, nil)
	if sc == nil {
		return nil, nil
	}
	f, _ := sc.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Fields != nil && f.Stream == 1 })
	if f == nil {
		y.tc.Logf("client opened a new connection but sent no request on it")
	}
	return sc, f
}

// cdMethod returns the :method of a request field block.
func cdMethod(f *rawFrame) string {
	if m := f.field(":method"); len(m) == 1 {
		return m[0]
	}
	return fmt.Sprint(f.field(":method"))
}

// cdExpectNoRetry checks that the Go client doesn't retry a POST on
// a new connection within d.
func cdExpectNoRetry(y *clientMoreRun, d time.Duration, why string) {
	sc, f := cdNextConnRequest(y, d)
	if f == nil {
		y.tc.Logf("client did not retry the request")
		return
	}
	if m := cdMethod(f); m == "POST" {
		y.tc.Failf("client automatically retried the POST on a new connection after %s", why)
	} else {
		y.tc.Logf("client sent a %s request on a new connection", m)
	}
	n := y.drainRequestBody(sc, 1)
	y.respond(sc, 1, strconv.Itoa(n))
}

// cdAwaitData reads the request body on stream until want bytes
// have arrived (if endStream is false) or until END_STREAM (if
// endStream is true). It returns the data, or ok=false if the stream
// was reset, ended early, or stalled, which it reports as a failure.
func cdAwaitData(y *clientMoreRun, sc *rawConn, stream uint32, want string, endStream bool) (string, bool) {
	var got []byte
	for {
		f, closed := sc.waitFor(reactionTimeout, func(f *rawFrame) bool {
			return f.Stream == stream && (f.Type == ftData || f.Type == ftRSTStream) || f.Type == ftGoAway
		})
		switch {
		case f == nil:
			y.tc.Failf("client sent %q on the tunnel; want %q (end_stream=%v) (closed=%v)", got, want, endStream, closed)
			return string(got), false
		case f.Type != ftData:
			y.tc.Failf("client sent %v after %q on the tunnel; want %q (end_stream=%v)", f, got, want, endStream)
			return string(got), false
		}
		got = append(got, f.Payload...)
		if len(f.Payload) > 0 {
			sc.writeWindowUpdate(0, uint32(len(f.Payload)))
			sc.writeWindowUpdate(stream, uint32(len(f.Payload)))
		}
		if f.Flags&flagEndStream != 0 {
			if !endStream || string(got) != want {
				y.tc.Failf("client ended the tunnel stream after %q; want %q (end_stream=%v)", got, want, endStream)
				return string(got), false
			}
			return string(got), true
		}
		if !endStream && len(got) >= len(want) {
			if string(got) != want {
				y.tc.Failf("client sent %q on the tunnel; want %q", got, want)
				return string(got), false
			}
			return string(got), true
		}
	}
}

// cdTunnelClient returns a client driver that opens a CONNECT tunnel,
// exchanges "ping1"/"pong1" and "ping2"/"pong2", and then closes its
// side of the tunnel either before reading the rest of the server's
// data (serverFinFirst false) or after reading it to EOF, and then
// sending "late" (serverFinFirst true). The result's Body is the data
// read from the tunnel. The driver closes the response body only
// after the script finishes, since closing it earlier would abandon
// the stream.
func cdTunnelClient(serverFinFirst bool) func(ctx context.Context, y *clientMoreRun) []*Result {
	return func(ctx context.Context, y *clientMoreRun) []*Result {
		pr, pw := io.Pipe()
		defer pw.Close()
		req := y.newRequest(ctx, "CONNECT", y.url, pr)
		req.Host = "tunnel.example:443"
		resp, err := y.tr.RoundTrip(req)
		if err != nil {
			y.signalScript()
			return []*Result{{Error: err.Error()}}
		}
		res := cdTunnel(resp, pw, serverFinFirst)
		y.signalScript()
		y.waitScript(ctx)
		resp.Body.Close()
		return []*Result{res}
	}
}

// cdTunnel uses the tunnel for cdTunnelClient.
func cdTunnel(resp *http.Response, pw *io.PipeWriter, serverFinFirst bool) *Result {
	res := &Result{Status: resp.StatusCode}
	write := func(s string) error {
		errc := make(chan error, 1)
		go func() {
			_, err := pw.Write([]byte(s))
			errc <- err
		}()
		select {
		case err := <-errc:
			return err
		case <-time.After(reactionTimeout):
			pw.CloseWithError(errors.New("timeout"))
			return fmt.Errorf("writing %q to the tunnel timed out", s)
		}
	}
	var got []byte
	defer func() { res.Body = string(got) }()
	for _, s := range []string{"1", "2"} {
		if err := write("ping" + s); err != nil {
			res.Error = err.Error()
			return res
		}
		buf := make([]byte, 5)
		n, err := io.ReadFull(resp.Body, buf)
		got = append(got, buf[:n]...)
		if err != nil {
			res.Error = fmt.Sprintf("reading tunnel: %v", err)
			return res
		}
	}
	if !serverFinFirst {
		pw.Close()
	}
	rest, err := io.ReadAll(resp.Body)
	got = append(got, rest...)
	if err != nil {
		res.Error = fmt.Sprintf("reading tunnel: %v", err)
		return res
	}
	if serverFinFirst {
		if err := write("late"); err != nil {
			res.Error = fmt.Sprintf("after reading EOF from the tunnel: %v", err)
			return res
		}
		pw.Close()
	}
	return res
}

// cdTunnelScript returns the script for cdTunnelClient.
func cdTunnelScript(serverFinFirst bool) func(y *clientMoreRun) {
	return func(y *clientMoreRun) {
		sc, f := y.sc, y.req
		if f.EndStream {
			y.tc.Failf("CONNECT request's HEADERS frame has END_STREAM, which closes the client's side of the tunnel")
			return
		}
		sc.writeFields(1, false, ":status", "200")
		for _, s := range []string{"1", "2"} {
			if _, ok := cdAwaitData(y, sc, 1, "ping"+s, false); !ok {
				return
			}
			sc.writeData(1, false, []byte("pong"+s))
		}
		want := "pong1pong2"
		if serverFinFirst {
			// The server closes its side of the tunnel, which is
			// like a TCP FIN; the client can still send.
			sc.writeData(1, true, []byte("bye"))
			want += "bye"
			if _, ok := cdAwaitData(y, sc, 1, "late", true); !ok {
				y.tc.Logf("(RFC 9113 §8.5: END_STREAM is equivalent to the TCP FIN bit, so after the server's END_STREAM, the client's side of the tunnel stays open)")
			}
		} else {
			// The client's END_STREAM closes its side of the
			// tunnel; the server can still send.
			if _, ok := cdAwaitData(y, sc, 1, "", true); !ok {
				return
			}
			sc.writeData(1, false, []byte("after-"))
			sc.writeData(1, true, []byte("fin"))
			want += "after-fin"
		}
		if !y.waitClient("tunnel closed") {
			return
		}
		y.releaseClient()
		if rs := y.result(); len(rs) > 0 {
			if r := rs[0]; r.Error != "" || r.Status != 200 || r.Body != want {
				y.tc.Failf("client's tunnel: status %d, read %q, err=%q; want 200, %q, no error", r.Status, r.Body, r.Error, want)
			}
		}
	}
}

// cdNoContentData returns a script that sends a response with status
// and fields kv (and no END_STREAM), followed by DATA, and checks
// that the client doesn't deliver the DATA as response content.
func cdNoContentData(status string, kv ...string) func(y *clientMoreRun) {
	return func(y *clientMoreRun) {
		sc := y.sc
		sc.writeFields(1, false, append([]string{":status", status}, kv...)...)
		sc.writeData(1, true, []byte("not-content"))
		rs := y.result()
		if len(rs) == 0 {
			return
		}
		switch r := rs[0]; {
		case r.BodyLen > 0:
			y.tc.Failf("client delivered %d bytes (%q) of DATA as the content of a response that has no content (status %s, request %s)", r.BodyLen, truncate(r.Body, 50), status, cdMethod(y.req))
		case r.Error != "":
			y.tc.Logf("client reported an error: %s", r.Error)
		default:
			y.tc.Logf("client reported status %d with no content", r.Status)
		}
	}
}

var clientDescCases = []*clientMoreCase{
	// RFC 9113 §4.3, §5.1: field blocks on reset streams.
	{
		clientConfCase: clientConfCase{
			name:   "rst-stream-then-hpack-state",
			covers: "9113-4.3-d1 9113-5.1-d1",
			desc:   "RFC 9113 §4.3: an endpoint receiving HEADERS or CONTINUATION frames needs to reassemble field blocks and perform decompression even if the frames are to be discarded; §5.1: after sending RST_STREAM, an endpoint could receive any type of frame, which means updating header compression state for HEADERS",
		},
		client: func(ctx context.Context, y *clientMoreRun) []*Result {
			ctx1, cancel1 := context.WithCancel(ctx)
			defer cancel1()
			r1c := make(chan *Result, 1)
			go func() { r1c <- y.roundTrip(y.newRequest(ctx1, "GET", y.url+"/canceled", nil)) }()
			y.waitScript(ctx) // the script has the request's HEADERS
			cancel1()
			r1 := <-r1c
			y.signalScript()
			y.waitScript(ctx) // the script sent the discarded response
			r2 := y.roundTrip(y.newRequest(ctx, "GET", y.url+"/next", nil))
			return []*Result{r1, r2}
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			y.releaseClient()
			if f, _ := sc.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Type == ftRSTStream && f.Stream == 1 }); f == nil {
				y.tc.Errorf("client did not reset the canceled stream")
				return
			}
			if !y.waitClient("canceled request returned") {
				return
			}
			// The response on the reset stream adds two entries
			// to the dynamic table, in a field block split across
			// HEADERS and CONTINUATION.
			bval := strings.Repeat("b", 100)
			blk := cdBlock().status200().insert("x-reset-a", "alpha", false).insert("x-reset-b", bval, true).b
			sc.writeHeaders(1, false, false, blk[:5])
			sc.writeContinuation(1, true, blk[5:])
			sc.writeData(1, true, []byte("discarded"))
			y.releaseClient()
			if !cdAwaitRequest(y, sc, 3, "the response on the reset stream") {
				return
			}
			// This response refers to both entries.
			sc.writeHeaders(3, true, true, cdBlock().status200().indexed(63).indexed(62).b)
			rs := y.result()
			if len(rs) != 2 {
				return
			}
			y.tc.Logf("canceled request: err=%q", rs[0].Error)
			cdCheckSteps(y, rs[1:], []cdStep{{want: map[string][]string{"x-reset-a": {"alpha"}, "x-reset-b": {bval}}}})
		},
	},

	// RFC 9113 §4.3.1: SETTINGS_HEADER_TABLE_SIZE takes effect when acknowledged.
	{
		clientConfCase: clientConfCase{
			name:     "hpack-table-size-before-settings-ack",
			covers:   "9113-4.3.1-d1",
			desc:     "RFC 9113 §4.3.1: any change to the maximum value set using SETTINGS_HEADER_TABLE_SIZE takes effect when the endpoint acknowledges settings; until the server acknowledges a SETTINGS_HEADER_TABLE_SIZE of 256, it may use a 4096-byte dynamic table",
			profile:  goProfiles[1], // constrained: SETTINGS_HEADER_TABLE_SIZE=256
			rawStart: true,
			reqs:     cdGets(3),
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			sc.writeSettings()
			f, _ := sc.waitFor(reactionTimeout, func(f *rawFrame) bool { return f.Type == ftSettings && f.Flags&flagAck == 0 })
			if f == nil {
				y.tc.Failf("client did not send SETTINGS")
				return
			}
			hts := uint32(4096)
			for p := f.Payload; len(p) >= 6; p = p[6:] {
				if binary.BigEndian.Uint16(p) == setHeaderTableSize {
					hts = binary.BigEndian.Uint32(p[2:])
				}
			}
			if hts != 256 {
				y.tc.Errorf("client's SETTINGS_HEADER_TABLE_SIZE is %d; want 256", hts)
				return
			}
			// The server doesn't acknowledge the client's SETTINGS
			// yet, so its encoder may use the default 4096-byte
			// table: these two entries use 486 bytes of it.
			a, b := strings.Repeat("a", 200), strings.Repeat("b", 200)
			steps := []cdStep{
				{
					block: cdBlock().status200().insert("x-pre-ack-a", a, false).insert("x-pre-ack-b", b, false).b,
					want:  map[string][]string{"x-pre-ack-a": {a}, "x-pre-ack-b": {b}},
				},
				{
					block: cdBlock().status200().indexed(63).indexed(62).b,
					want:  map[string][]string{"x-pre-ack-a": {a}, "x-pre-ack-b": {b}},
				},
			}
			if y.waitRequest(sc, 1) == nil {
				return
			}
			for i, st := range steps {
				stream := uint32(1 + 2*i)
				if i > 0 && !cdAwaitRequest(y, sc, stream, "response 1 (adding entries of 243 bytes each)") {
					return
				}
				sc.writeHeaders(stream, true, true, st.block)
			}
			if !cdAwaitRequest(y, sc, 5, "response 2, before acknowledging the client's SETTINGS (references to two dynamic table entries using 486 bytes; the client's SETTINGS_HEADER_TABLE_SIZE=256 takes effect only when acknowledged, so the server may use the default 4096-byte table)") {
				return
			}
			// Now acknowledge, and signal the smaller table in the
			// next field block (RFC 7541 §4.2).
			sc.writeSettingsAck()
			last := cdStep{
				block: cdBlock().sizeUpdate(256).status200().insert("x-after-ack", "1", false).b,
				want:  map[string][]string{"x-after-ack": {"1"}},
			}
			sc.writeHeaders(5, true, true, last.block)
			cdCheckSteps(y, y.result(), append(steps, last))
		},
	},

	// RFC 9113 §5.4.3, §6.8, §8.7: retries.
	{
		clientConfCase: clientConfCase{
			name:   "post-not-retried-after-close",
			covers: "9113-5.4.3-d1",
			desc:   "RFC 9113 §5.4.3: if the TCP connection is closed or reset while streams remain in the \"open\" or \"half-closed\" states, then the affected streams cannot be automatically retried (here, a POST whose body could be replayed, and a close without GOAWAY)",
			reqs:   []*Req{{Method: "POST", Path: "/upload", BodyLen: 1000}},
		},
		run: func(y *clientMoreRun) {
			y.drainRequestBody(y.sc, 1)
			y.sc.conn.Close()
			cdExpectNoRetry(y, time.Second, "the connection closed without GOAWAY while the stream was half-closed (local)")
			y.expectRequestError("connection closed before the response")
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "post-not-retried-after-reset",
			covers: "9113-5.4.3-d1",
			desc:   "RFC 9113 §5.4.3: if the TCP connection is closed or reset while streams remain in the \"open\" or \"half-closed\" states, then the affected streams cannot be automatically retried (here, a POST whose body could be replayed, and a TCP reset)",
			reqs:   []*Req{{Method: "POST", Path: "/upload", BodyLen: 1000}},
		},
		run: func(y *clientMoreRun) {
			y.drainRequestBody(y.sc, 1)
			if tc, ok := y.sc.conn.(*net.TCPConn); ok {
				tc.SetLinger(0)
			}
			y.sc.conn.Close()
			cdExpectNoRetry(y, time.Second, "the connection was reset while the stream was half-closed (local)")
			y.expectRequestError("connection reset before the response")
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "goaway-processed-post-not-retried",
			covers: "9113-6.8-d2",
			desc:   "RFC 9113 §6.8: on streams with lower- or equal-numbered identifiers than a GOAWAY's last stream ID that were not closed completely prior to the connection being closed, reattempting requests is not possible, except for idempotent actions like HTTP GET, PUT, or DELETE",
			reqs:   []*Req{{Method: "POST", Path: "/upload", BodyLen: 1000}, {Method: "GET", Path: "/get"}},
		},
		run: func(y *clientMoreRun) {
			y.drainRequestBody(y.sc, 1)
			y.sc.writeGoAway(1, http2.ErrCodeNo)
			y.sc.conn.Close()
			// The next request is a retry of the POST or the GET.
			sc, f := cdNextConnRequest(y, 3*time.Second)
			if f == nil {
				y.tc.Failf("client made no request on a new connection")
				return
			}
			if cdMethod(f) == "POST" {
				y.tc.Failf("client automatically retried the POST on stream 1 after GOAWAY with last stream ID 1 and connection close")
				y.respond(sc, 1, strconv.Itoa(y.drainRequestBody(sc, 1)))
				if sc, f = cdNextConnRequest(y, 3*time.Second); f == nil {
					return
				}
			}
			// Do the same to the GET, whose retry is permitted.
			sc.writeGoAway(1, http2.ErrCodeNo)
			sc.conn.Close()
			if sc, f = cdNextConnRequest(y, time.Second); f != nil {
				y.tc.Logf("client retried the %s request on a new connection (permitted for idempotent methods)", cdMethod(f))
				y.respond(sc, 1, "retried")
			} else {
				y.tc.Logf("client did not retry the GET request (it was permitted to)")
			}
			rs := y.result()
			if len(rs) == 2 {
				if rs[0].Error == "" {
					y.tc.Failf("POST succeeded with status %d; want an error", rs[0].Status)
				}
				y.tc.Logf("GET: status %d, err=%q", rs[1].Status, rs[1].Error)
			}
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "goaway-unprocessed-post-retried",
			covers: "9113-6.8-d3",
			desc:   "RFC 9113 §6.8: any protocol activity that uses higher-numbered streams than a GOAWAY's last stream ID can be safely retried using a new connection (here, a POST whose body can be replayed with GetBody)",
			reqs:   uploadReq(100000),
		},
		run: func(y *clientMoreRun) {
			// Let the client send the part of the body its
			// windows allow.
			if n := countRequestData(y.sc, 1, 65535); n != 65535 {
				y.tc.Errorf("client sent %d bytes of its 65535-byte window", n)
				return
			}
			y.sc.writeGoAway(0, http2.ErrCodeNo)
			sc, f := cdNextConnRequest(y, 3*time.Second)
			if f == nil {
				y.tc.Logf("client did not retry the request while the first connection was open; closing it")
				y.sc.conn.Close()
				if sc, f = cdNextConnRequest(y, 3*time.Second); f != nil {
					y.tc.Logf("client retried after the connection closed")
				}
			}
			if f == nil {
				y.tc.Warnf("client did not retry the POST on stream 1 after GOAWAY with last stream ID 0, though the request can be safely retried")
				y.result()
				return
			}
			if m := cdMethod(f); m != "POST" {
				y.tc.Failf("client sent %s on the new connection; want the POST", m)
			}
			n := y.drainRequestBody(sc, 1)
			y.respond(sc, 1, strconv.Itoa(n))
			y.expectRequestOK("100000")
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "refused-stream-post-retried",
			covers: "9113-8.7-d1",
			desc:   "RFC 9113 §8.7: the REFUSED_STREAM error code can be included in a RST_STREAM frame to indicate that the stream is being closed prior to any processing having occurred; any request that was sent on the reset stream can be safely retried (here, a POST whose body can be replayed with GetBody)",
			reqs:   uploadReq(100000),
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			if n := countRequestData(sc, 1, 65535); n != 65535 {
				y.tc.Errorf("client sent %d bytes of its 65535-byte window", n)
				return
			}
			sc.writeRST(1, http2.ErrCodeRefusedStream)
			// Return the connection window used by the refused
			// stream, so a retry can use this connection.
			sc.writeWindowUpdate(0, 65535)
			var stream uint32
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && stream == 0; {
				select {
				case c := <-y.conns:
					y.conns <- c
					var f *rawFrame
					if sc, f = cdNextConnRequest(y, 0); f != nil {
						y.tc.Logf("client retried on a new connection")
						stream = 1
					}
				default:
				}
				if f, _ := sc.waitFor(100*time.Millisecond, func(f *rawFrame) bool { return f.Fields != nil && f.Stream > 1 }); f != nil {
					y.tc.Logf("client retried on stream %d", f.Stream)
					stream = f.Stream
				}
			}
			if stream == 0 {
				y.tc.Warnf("client did not retry the POST refused with REFUSED_STREAM within 3s, though it can be safely retried")
				y.result()
				return
			}
			n := y.drainRequestBody(sc, stream)
			y.respond(sc, stream, strconv.Itoa(n))
			y.expectRequestOK("100000")
		},
	},

	// RFC 9113 §6.1, §6.9, §6.9.2: flow control.
	{
		clientConfCase: clientConfCase{
			name:    "padded-data-flow-control",
			covers:  "9113-6.1-d1",
			desc:    "RFC 9113 §6.1: the entire DATA frame payload is included in flow control, including the Pad Length and Padding fields if present, so the receiver must return credit for padding",
			profile: goProfiles[1], // 64KB connection window, 16KB stream windows
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			w := newSendWindows(y.clientConfRun)
			// Each frame carries 1 byte of data and 254 of padding.
			const frames, flen = 1000, 256
			payload := make([]byte, flen)
			payload[0], payload[1] = 254, 'x'
			sc.writeFields(1, false, ":status", "200", "content-length", strconv.Itoa(frames))
			sc.logSend("%d padded DATA frames of %d bytes, 1 of them data", frames, flen)
			for i := range frames {
				for deadline := time.Now().Add(5 * time.Second); ; {
					w.update()
					if w.conn >= flen && w.stream(1) >= flen {
						break
					}
					left := time.Until(deadline)
					if left <= 0 {
						y.tc.Failf("client's flow-control windows (connection %d, stream %d) stayed closed after %d padded DATA frames (%d bytes of payload, %d of data): it doesn't return the credit for padding", w.conn, w.stream(1), i, i*flen, i)
						return
					}
					if f, _ := sc.waitFor(left, isType(ftWindowUpdate)); f != nil {
						w.apply(f)
					}
				}
				var flags byte = flagPadded
				if i == frames-1 {
					flags |= flagEndStream
				}
				sc.wmu.Lock()
				err := sc.fr.WriteRawFrame(http2.FrameType(ftData), http2.Flags(flags), 1, payload)
				sc.wmu.Unlock()
				if err != nil {
					y.tc.Errorf("writing DATA: %v", err)
					return
				}
				w.conn -= flen
				w.streams[1] = w.stream(1) - flen
			}
			y.expectRequestOK(strings.Repeat("x", frames))
		},
	},
	{
		clientConfCase: clientConfCase{
			name:    "data-on-closed-stream-returns-credit",
			covers:  "9113-6.9-d2",
			desc:    "RFC 9113 §6.9: flow control credit must be returned even if the frame is in error; the sender counts the frame toward the flow-control window, but if the receiver does not, the flow-control windows at the sender and receiver can become different (here, DATA on a stream closed by END_STREAM in both directions)",
			profile: smallConnWindow,
			reqs:    cdGets(2),
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			w := newSendWindows(y.clientConfRun)
			sc.writeFields(1, false, ":status", "200", "content-length", "2")
			w.send(1, 2, true, time.Second)
			// Stream 1 is closed. Use the rest of the
			// connection window for DATA on it.
			w.update()
			junk := int(w.conn)
			w.send(1, junk, false, time.Second)
			y.tc.Logf("sent %d bytes of DATA on closed stream 1", junk)
			f, closed := sc.waitFor(5*time.Second, func(f *rawFrame) bool {
				return f.Type == ftGoAway || (f.Fields != nil && f.Stream == 3)
			})
			switch {
			case f == nil:
				y.tc.Failf("client did not send its second request (closed=%v)", closed)
				return
			case f.Type == ftGoAway:
				// RFC 9113 §5.1 permits a connection error.
				y.tc.Skipf("client treated DATA on a closed stream as a connection error (%v), so its connection window can't be checked", f)
				return
			}
			n := 2 * (junk + 2) // about twice the client's connection window
			sc.writeFields(3, false, ":status", "200", "content-length", strconv.Itoa(n))
			if sent := w.send(3, n, true, 5*time.Second); sent != n {
				y.tc.Failf("client's connection window stayed closed after %d of %d response bytes: it didn't return connection flow-control credit for the %d bytes of DATA on the closed stream", sent, n, junk)
				return
			}
			if rs := y.result(); len(rs) == 2 && (rs[1].Error != "" || rs[1].BodyLen != int64(n)) {
				y.tc.Failf("second response: %d bytes, err=%q; want %d bytes", rs[1].BodyLen, rs[1].Error, n)
			}
		},
	},
	{
		clientConfCase: clientConfCase{
			name:     "settings-dont-alter-connection-window",
			covers:   "9113-6.9.2-d1",
			desc:     "RFC 9113 §6.9.2: a SETTINGS frame cannot alter the connection flow-control window; after SETTINGS_INITIAL_WINDOW_SIZE=2^31-1, the client may send only 65535 bytes on the connection until the server sends a connection-level WINDOW_UPDATE",
			settings: []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1<<31 - 1}},
			reqs:     uploadReq(200000),
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			isData := func(f *rawFrame) bool { return f.Type == ftData && f.Stream == 1 }
			got := 0
			for deadline := time.Now().Add(500 * time.Millisecond); ; {
				f, _ := sc.waitFor(time.Until(deadline), isData)
				if f == nil {
					break
				}
				got += len(f.Payload)
			}
			y.tc.Logf("client sent %d bytes of request body before the server granted connection window", got)
			if got > 65535 {
				y.tc.Failf("client sent %d bytes of DATA with a connection window of 65535: it let SETTINGS_INITIAL_WINDOW_SIZE alter the connection window", got)
			}
			// Grant connection window as the body arrives.
			sc.writeWindowUpdate(0, uint32(got))
			for {
				f, closed := sc.waitFor(5*time.Second, func(f *rawFrame) bool { return isData(f) || (f.Type == ftRSTStream && f.Stream == 1) })
				if f == nil || f.Type != ftData {
					y.tc.Failf("request body incomplete after %d bytes (%v, closed=%v)", got, f, closed)
					return
				}
				got += len(f.Payload)
				if len(f.Payload) > 0 {
					sc.writeWindowUpdate(0, uint32(len(f.Payload)))
				}
				if f.Flags&flagEndStream != 0 {
					break
				}
			}
			y.respond(sc, 1, strconv.Itoa(got))
			y.expectRequestOK("200000")
		},
	},

	// RFC 9113 §6.2, §8.1: field blocks with END_STREAM and CONTINUATION.
	{
		clientConfCase: clientConfCase{
			name:   "headers-end-stream-continuation",
			covers: "9113-6.2-d1",
			desc:   "RFC 9113 §6.2: a HEADERS frame with the END_STREAM flag set can be followed by CONTINUATION frames on the same stream",
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			av, cv := strings.Repeat("a", 50), strings.Repeat("c", 50)
			blk := sc.block(":status", "200", "x-a", av, "x-b", "bee", "x-c", cv)
			sc.writeHeaders(1, true, false, blk[:3])
			sc.writeContinuation(1, false, blk[3:40])
			sc.writeContinuation(1, true, blk[40:])
			cdCheckSteps(y, y.result(), []cdStep{{want: map[string][]string{"x-a": {av}, "x-b": {"bee"}, "x-c": {cv}}}})
			if rs := y.result(); len(rs) == 1 && rs[0].BodyLen != 0 {
				y.tc.Failf("response body is %d bytes; want none", rs[0].BodyLen)
			}
		},
	},
	{
		clientConfCase: clientConfCase{
			name:   "trailers-continuation",
			covers: "9113-8.1-d1",
			desc:   "RFC 9113 §8.1: an HTTP response is complete after the client receives a frame with the END_STREAM flag set, including any CONTINUATION frames needed to complete a field block (here, trailers)",
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			t1, t3 := strings.Repeat("1", 40), strings.Repeat("3", 40)
			sc.writeFields(1, false, ":status", "200", "trailer", "x-t1, x-t2, x-t3")
			sc.writeData(1, false, []byte("body"))
			blk := sc.block("x-t1", t1, "x-t2", "two", "x-t3", t3)
			sc.writeHeaders(1, true, false, blk[:4])
			sc.writeContinuation(1, false, blk[4:30])
			sc.writeContinuation(1, true, blk[30:])
			rs := y.result()
			if len(rs) != 1 {
				return
			}
			r := rs[0]
			if r.Error != "" || r.Body != "body" {
				y.tc.Failf("response: body %q, err=%q; want %q", r.Body, r.Error, "body")
				return
			}
			want := [][2]string{{"x-t1", t1}, {"x-t2", "two"}, {"x-t3", t3}}
			if !slices.Equal(r.Trailer, want) {
				y.tc.Failf("trailers %q; want %q", r.Trailer, want)
			}
		},
	},

	// RFC 9113 §6.3: PRIORITY in any stream state.
	{
		clientConfCase: clientConfCase{
			name:   "priority-any-stream-state",
			covers: "9113-6.3-d1",
			desc:   "RFC 9113 §6.3: the PRIORITY frame can be sent on a stream in any state, including \"idle\" or \"closed\"",
			reqs:   cdGets(2),
		},
		run: func(y *clientMoreRun) {
			sc := y.sc
			priority := func(stream uint32) {
				sc.writeRaw(ftPriority, 0, stream, []byte{0, 0, 0, 0, 15})
			}
			priority(1) // half-closed (local)
			priority(9) // idle
			priority(2) // idle, even-numbered
			if !y.syncPing(sc, [8]byte{'p', 'r', 'i', 'o'}) {
				return
			}
			y.respond(sc, 1, "a")
			if !cdAwaitRequest(y, sc, 3, "PRIORITY frames and response 1") {
				return
			}
			priority(1) // closed
			priority(3) // half-closed (local)
			y.respond(sc, 3, "b")
			sc.expectNoError(300 * time.Millisecond)
			y.expectRequestOK("")
		},
	},

	// RFC 9113 §8.1.1: responses without content.
	{
		clientConfCase: clientConfCase{
			name:   "response-data-to-head",
			covers: "9113-8.1.1-d2",
			desc:   "RFC 9113 §8.1.1: the response to a HEAD request contains no content, so DATA in it isn't delivered as content",
			reqs:   []*Req{{Method: "HEAD", Path: "/"}},
		},
		run: cdNoContentData("200", "content-length", "11"),
	},
	{
		clientConfCase: clientConfCase{
			name:   "response-data-in-204",
			covers: "9113-8.1.1-d2",
			desc:   "RFC 9113 §8.1.1: 204 responses contain no content, so DATA in one isn't delivered as content",
		},
		run: cdNoContentData("204"),
	},
	{
		clientConfCase: clientConfCase{
			name:   "response-data-in-304",
			covers: "9113-8.1.1-d2",
			desc:   "RFC 9113 §8.1.1: 304 responses contain no content, so DATA in one isn't delivered as content",
		},
		run: cdNoContentData("304"),
	},

	// RFC 9113 §8.5: CONNECT tunnels.
	{
		clientConfCase: clientConfCase{
			name:   "connect-tunnel-client-fin-first",
			covers: "9113-8.5-d1",
			desc:   "RFC 9113 §8.5: after the initial HEADERS frame sent by each peer, all subsequent DATA frames correspond to data sent on the TCP connection, and END_STREAM is equivalent to the TCP FIN bit (here, the client closes its side of the tunnel first)",
		},
		client: cdTunnelClient(false),
		run:    cdTunnelScript(false),
	},
	{
		clientConfCase: clientConfCase{
			name:   "connect-tunnel-server-fin-first",
			covers: "9113-8.5-d1",
			desc:   "RFC 9113 §8.5: after the initial HEADERS frame sent by each peer, all subsequent DATA frames correspond to data sent on the TCP connection, and END_STREAM is equivalent to the TCP FIN bit (here, the server closes its side of the tunnel first, and the client sends more before closing its side)",
		},
		client: cdTunnelClient(true),
		run:    cdTunnelScript(true),
	},

	// RFC 7541: the dynamic table.
	cdHPACKCase("hpack-entry-size-huffman-longer", "7541-4.1-d1",
		"RFC 7541 §4.1: the size of an entry is the sum of its name's length in octets, its value's length in octets, and 32, calculated without any Huffman encoding applied (here, Huffman encodings longer than the strings fill the table exactly)",
		cdFillSteps("\\", false)...),
	cdHPACKCase("hpack-entry-size-huffman-shorter", "7541-4.1-d1",
		"RFC 7541 §4.1: the size of an entry is the sum of its name's length in octets, its value's length in octets, and 32, calculated without any Huffman encoding applied (here, Huffman encodings shorter than the strings fill the table exactly, and one more entry evicts the oldest)",
		cdFillSteps("a", true)...),
	cdHPACKCase("hpack-two-size-updates", "7541-4.2-d1",
		"RFC 7541 §4.2: the final maximum size is always signaled, resulting in at most two dynamic table size updates at the start of a field block (here, 0 and then 4096)",
		cdStep{
			block: cdBlock().status200().insert("x-before", "1", false).b,
			want:  map[string][]string{"x-before": {"1"}},
		},
		cdStep{
			block: cdBlock().sizeUpdate(0).sizeUpdate(4096).status200().insert("x-after", "2", false).b,
			want:  map[string][]string{"x-after": {"2"}},
		},
		cdStep{
			block: cdBlock().status200().indexed(62).b,
			want:  map[string][]string{"x-after": {"2"}, "x-before": nil},
		},
	),
	cdHPACKCase("hpack-size-update-evicts", "7541-4.3-d1",
		"RFC 7541 §4.3: whenever the maximum size for the dynamic table is reduced, entries are evicted from the end of the dynamic table until the size of the dynamic table is less than or equal to the maximum size; a reference to an evicted entry is a decoding error",
		cdStep{
			block: cdBlock().status200().insert("x-old", "a", false).insert("x-new", "b", false).b,
			want:  map[string][]string{"x-old": {"a"}, "x-new": {"b"}},
		},
		cdStep{
			// Room for x-new only.
			block: cdBlock().sizeUpdate(cdEntrySize("x-new", "b")).status200().indexed(62).b,
			want:  map[string][]string{"x-new": {"b"}, "x-old": nil},
		},
		cdStep{
			block:     cdBlock().status200().indexed(63).b,
			decodeErr: true,
		},
	),
	cdHPACKCase("hpack-oversized-entry-empties-table", "7541-4.4-d1",
		"RFC 7541 §4.4: it is not an error to attempt to add an entry that is larger than the maximum size; doing so empties the table, so a later reference to an old entry is a decoding error",
		cdStep{
			block: cdBlock().status200().insert("x-a", "1", false).insert("x-b", "2", false).b,
			want:  map[string][]string{"x-a": {"1"}, "x-b": {"2"}},
		},
		cdStep{
			block: cdBlock().status200().insert("x-huge", strings.Repeat("h", 4100), false).b,
			want:  map[string][]string{"x-huge": {strings.Repeat("h", 4100)}},
		},
		cdStep{
			// The table is empty but still usable.
			block: cdBlock().status200().insert("x-c", "3", false).indexed(62).b,
			want:  map[string][]string{"x-c": {"3", "3"}},
		},
		cdStep{
			block:     cdBlock().status200().indexed(63).b,
			decodeErr: true,
		},
	),
	cdHPACKCase("hpack-name-ref-to-evicted-entry", "7541-4.4-d2",
		"RFC 7541 §4.4: a new entry can reference the name of an entry in the dynamic table that will be evicted when adding this new entry into the dynamic table",
		cdStep{
			// Entries of 2046 and 2035 bytes: 4081 of 4096.
			block: cdBlock().status200().insert("x-evicted-name", strings.Repeat("e", 2000), false).insert("x-b", strings.Repeat("b", 2000), false).b,
			want:  map[string][]string{"x-evicted-name": {strings.Repeat("e", 2000)}, "x-b": {strings.Repeat("b", 2000)}},
		},
		cdStep{
			// A 49-byte entry named by index 63 (x-evicted-name),
			// whose insertion evicts that entry.
			block: cdBlock().status200().insertNameIdx(63, "new", false).b,
			want:  map[string][]string{"x-evicted-name": {"new"}, "x-b": nil},
		},
		cdStep{
			block: cdBlock().status200().indexed(62).indexed(63).b,
			want:  map[string][]string{"x-evicted-name": {"new"}, "x-b": {strings.Repeat("b", 2000)}},
		},
	),
	cdHPACKCase("hpack-integer-prefix-max", "7541-5.1-d1",
		"RFC 7541 §5.1: an integer equal to 2^N-1 is encoded with all the bits of the prefix set to 1 followed by an octet of 0 (here, with 4-, 5-, 6-, and 7-bit prefixes: a name index of 15, a table size of 31, a name index of 63, an index of 127, and a string length of 127)",
		cdIntegerSteps()...),
}

// cdFillSteps returns the steps of the hpack-entry-size-huffman cases.
// They fill the 4096-byte dynamic table exactly with four 1024-byte
// entries whose names and values are Huffman-encoded, and whose
// values repeat c, and check that all four are usable. Then they add
// a small entry, which evicts exactly the oldest one. If evictCheck
// is set, a last step checks that a reference to the evicted entry is
// a decoding error.
func cdFillSteps(c string, evictCheck bool) []cdStep {
	names := []string{"x-fill-1", "x-fill-2", "x-fill-3", "x-fill-4"}
	value := strings.Repeat(c, 1024-32-len(names[0]))
	fill := cdBlock().status200()
	all := map[string][]string{}
	ref := cdBlock().status200()
	for i, name := range names {
		if cdEntrySize(name, value) != 1024 {
			panic("bad entry size")
		}
		fill.insert(name, value, true)
		all[name] = []string{value}
		ref.indexed(65 - i)
	}
	// Adding x-e evicts x-fill-1 (now index 66) and leaves x-fill-4,
	// x-fill-3, and x-fill-2 at indexes 63 to 65.
	evict := cdBlock().status200().insert("x-e", "e", true).indexed(62).indexed(63).indexed(64).indexed(65)
	steps := []cdStep{
		{block: fill.b, want: all},
		{block: ref.b, want: all},
		{block: evict.b, want: map[string][]string{
			"x-e": {"e", "e"}, "x-fill-1": nil,
			"x-fill-2": {value}, "x-fill-3": {value}, "x-fill-4": {value},
		}},
	}
	if evictCheck {
		steps = append(steps, cdStep{block: cdBlock().status200().indexed(66).b, decodeErr: true})
	}
	return steps
}

// cdIntegerSteps returns the steps of the hpack-integer-prefix-max case.
func cdIntegerSteps() []cdStep {
	// Insert 66 entries, so that index 127 is the oldest dynamic
	// table entry (x-i-00) and index 63 is the second newest
	// (x-i-64).
	first := cdBlock().status200()
	want1 := map[string][]string{}
	for i := range 66 {
		name, value := fmt.Sprintf("x-i-%02d", i), fmt.Sprintf("v%02d", i)
		first.insert(name, value, false)
		want1[name] = []string{value}
	}
	// A literal without indexing whose name is static index 15,
	// accept-charset (4-bit prefix: 0x0f 0x00).
	first.raw(0x0f, 0x00).str("utf-8", false)
	want1["accept-charset"] = []string{"utf-8"}
	// A literal without indexing whose value is 127 bytes long
	// (7-bit prefix: 0x7f 0x00).
	long := strings.Repeat("L", 127)
	first.raw(0x00).str("x-len127", false).raw(0x7f, 0x00).raw([]byte(long)...)
	want1["x-len127"] = []string{long}

	// An indexed field with index 127 (7-bit prefix: 0xff 0x00),
	// and a literal with incremental indexing whose name is index 63
	// (6-bit prefix: 0x7f 0x00).
	second := cdBlock().status200().raw(0xff, 0x00).raw(0x7f, 0x00).str("named", false)

	// A dynamic table size update to 31 (5-bit prefix: 0x3f 0x00),
	// then back to 4096.
	third := cdBlock().raw(0x3f, 0x00).sizeUpdate(4096).status200()

	return []cdStep{
		{block: first.b, want: want1},
		{block: second.b, want: map[string][]string{"x-i-00": {"v00"}, "x-i-64": {"named"}}},
		{block: third.b, want: map[string][]string{"x-i-00": nil}},
	}
}
