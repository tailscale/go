// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/internal/http2"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2/hpack"
)

// This file has frame-level conformance tests of the Go server for
// the DESCRIPTIVE requirements of requirements.txt: statements of
// the RFCs without BCP 14 keywords that still constrain the server,
// such as "the entire DATA frame payload is included in flow
// control". The tests are named conform/server/<name>, like those in
// conform_server.go, and run with runSMConf (see
// conform_server_more.go).

func init() {
	for _, c := range sdCases {
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

// Go server profiles used by the tests in this file.
var (
	// sdProfileMCS2 allows two concurrent streams.
	sdProfileMCS2 = &goProfile{Name: "sd-mcs2", HTTP2: http.HTTP2Config{MaxConcurrentStreams: 2}}
	// sdProfileWin64K has 64KB connection and stream receive windows.
	sdProfileWin64K = &goProfile{Name: "sd-win64k", HTTP2: http.HTTP2Config{
		MaxReceiveBufferPerConnection: 65536,
		MaxReceiveBufferPerStream:     65536,
	}}
)

// sdFlow tracks the client's connection and stream send windows, as
// granted by the server's SETTINGS_INITIAL_WINDOW_SIZE and
// WINDOW_UPDATE frames. It also counts the WINDOW_UPDATE frames.
type sdFlow struct {
	x       *smRun
	conn    int64
	initial int64
	streams map[uint32]int64
	updates int  // WINDOW_UPDATE frames absorbed
	broken  bool // await saw a connection error or RST_STREAM
}

func newSDFlow(x *smRun) *sdFlow {
	return &sdFlow{x: x, conn: 65535, initial: int64(x.streamWindow()), streams: map[uint32]int64{}}
}

func (fl *sdFlow) streamWin(stream uint32) int64 {
	if v, ok := fl.streams[stream]; ok {
		return v
	}
	return fl.initial
}

// avail returns the number of bytes the client may send on stream.
func (fl *sdFlow) avail(stream uint32) int64 {
	return min(fl.conn, fl.streamWin(stream))
}

func (fl *sdFlow) absorb(f *rawFrame) {
	if f.Type != ftWindowUpdate || len(f.Payload) != 4 {
		return
	}
	fl.updates++
	incr := int64(binary.BigEndian.Uint32(f.Payload) & (1<<31 - 1))
	if f.Stream == 0 {
		fl.conn += incr
	} else {
		fl.streams[f.Stream] = fl.streamWin(f.Stream) + incr
	}
}

// poll absorbs the WINDOW_UPDATE frames received so far, without
// waiting.
func (fl *sdFlow) poll() {
	rc := fl.x.c
	for _, f := range rc.seen(isType(ftWindowUpdate)) {
		fl.absorb(f)
	}
	rc.mu.Lock()
	rc.pending = slices.DeleteFunc(rc.pending, isType(ftWindowUpdate))
	rc.mu.Unlock()
}

// await waits up to timeout until the client may send need bytes on
// stream. A connection error or a RST_STREAM on stream that arrives
// first fails the test.
func (fl *sdFlow) await(stream uint32, need int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		fl.poll()
		if fl.avail(stream) >= need {
			return true
		}
		d := time.Until(deadline)
		if d <= 0 {
			return false
		}
		f, closed := fl.x.c.waitFor(d, func(f *rawFrame) bool {
			return f.Type == ftWindowUpdate ||
				f.Type == ftGoAway && f.ErrCode() != http2.ErrCodeNo ||
				f.Type == ftRSTStream && f.Stream == stream
		})
		switch {
		case f == nil && closed:
			fl.broken = true
			fl.x.tc.Failf("connection closed while waiting for flow control credit on stream %d", stream)
			return false
		case f == nil:
			return false
		case f.Type != ftWindowUpdate:
			fl.broken = true
			fl.x.tc.Failf("while waiting for flow control credit on stream %d: got %v", stream, f)
			return false
		}
		fl.absorb(f)
	}
}

// sent accounts for a DATA frame with payload length n on stream.
func (fl *sdFlow) sent(stream uint32, n int) {
	fl.conn -= int64(n)
	fl.streams[stream] = fl.streamWin(stream) - int64(n)
}

// sdUnread returns f, a frame consumed by waitFor, to the front of
// the unconsumed frames.
func (x *smRun) sdUnread(f *rawFrame) {
	x.c.mu.Lock()
	defer x.c.mu.Unlock()
	x.c.pending = slices.Insert(x.c.pending, 0, f)
}

// sdInfo reads the /info response on stream.
func (x *smRun) sdInfo(stream uint32) (*InfoBody, bool) {
	if x.expectStatus(stream, 200) == nil {
		return nil, false
	}
	body, ok := x.readBody(stream, false, reactionTimeout)
	if !ok {
		x.tc.Failf("stream %d: incomplete /info response %q", stream, body)
		return nil, false
	}
	var info InfoBody
	if err := json.Unmarshal(body, &info); err != nil {
		x.tc.Failf("stream %d: bad /info response %q: %v", stream, body, err)
		return nil, false
	}
	return &info, true
}

// sdUpload reads the /upload response on stream.
func (x *smRun) sdUpload(stream uint32) (*UploadBody, bool) {
	if x.expectStatus(stream, 200) == nil {
		return nil, false
	}
	body, ok := x.readBody(stream, false, reactionTimeout)
	if !ok {
		x.tc.Failf("stream %d: incomplete /upload response %q", stream, body)
		return nil, false
	}
	var u UploadBody
	if err := json.Unmarshal(body, &u); err != nil {
		x.tc.Failf("stream %d: bad /upload response %q: %v", stream, body, err)
		return nil, false
	}
	return &u, true
}

// sdCheckHeader checks that the handler saw the field name with
// exactly the values want.
func (x *smRun) sdCheckHeader(stream uint32, info *InfoBody, name string, want ...string) {
	if got := info.Header[name]; !slices.Equal(got, want) {
		x.tc.Failf("stream %d: handler saw %s %s; want %s", stream, sdShort(name), sdShortList(got), sdShortList(want))
	}
}

// sdShort abbreviates long strings in messages.
func sdShort(s string) string {
	if len(s) <= 40 {
		return strconv.Quote(s)
	}
	return fmt.Sprintf("%q...(%d bytes)", s[:20], len(s))
}

func sdShortList(l []string) string {
	var s []string
	for _, v := range l {
		s = append(s, sdShort(v))
	}
	return "[" + strings.Join(s, " ") + "]"
}

// sdExpectDecodingError checks that the server treats the field block
// on stream as a decoding error: a connection error of type
// COMPRESSION_ERROR (RFC 9113 §4.3). The failure message for a
// response instead includes what the handler saw.
func (x *smRun) sdExpectDecodingError(stream uint32, why string) {
	f, closed := x.response(stream, reactionTimeout)
	switch {
	case f == nil:
		x.tc.Failf("no reaction to %s within %v (closed=%v); want connection error COMPRESSION_ERROR", why, reactionTimeout, closed)
	case f.Type == ftGoAway && f.ErrCode() == http2.ErrCodeCompression:
		x.tc.Logf("got expected connection error: %v", f)
	case f.Type == ftGoAway || f.Type == ftRSTStream:
		x.tc.Failf("got %v after %s; want connection error COMPRESSION_ERROR (RFC 9113 §4.3)", f, why)
	default:
		body, _ := x.readBody(stream, false, reactionTimeout)
		x.tc.Failf("Go server accepted %s: got :status %s, handler saw %s; want connection error COMPRESSION_ERROR", why, f.Status(), body)
	}
}

// HPACK encoding helpers (RFC 7541 §5, §6), for field blocks built
// by hand. Connections using them must not also use rawConn.block,
// whose encoder keeps its own dynamic table.

// sdHpackString appends a string literal (RFC 7541 §5.2), Huffman
// encoded if huff is set.
func sdHpackString(b []byte, s string, huff bool) []byte {
	if huff {
		b = smHpackInt(b, 7, 0x80, hpack.HuffmanEncodeLength(s))
		return hpack.AppendHuffmanString(b, s)
	}
	b = smHpackInt(b, 7, 0, uint64(len(s)))
	return append(b, s...)
}

// sdHpackInsert appends a literal field with incremental indexing and
// a literal name (RFC 7541 §6.2.1), with a Huffman-encoded value if
// huff is set.
func sdHpackInsert(b []byte, name, value string, huff bool) []byte {
	b = append(b, 0x40)
	b = sdHpackString(b, name, false)
	return sdHpackString(b, value, huff)
}

// sdHpackInsertIndexedName appends a literal field with incremental
// indexing whose name is that of the table entry at index (RFC 7541
// §6.2.1).
func sdHpackInsertIndexedName(b []byte, index uint64, value string) []byte {
	b = smHpackInt(b, 6, 0x40, index)
	return sdHpackString(b, value, false)
}

// sdHpackIndexed appends an indexed field (RFC 7541 §6.1).
func sdHpackIndexed(b []byte, index uint64) []byte {
	return smHpackInt(b, 7, 0x80, index)
}

// sdHpackSizeUpdate appends a dynamic table size update (RFC 7541
// §6.3).
func sdHpackSizeUpdate(b []byte, size uint64) []byte {
	return smHpackInt(b, 5, 0x20, size)
}

// sdSplit splits b into n nearly equal parts.
func sdSplit(b []byte, n int) [][]byte {
	var parts [][]byte
	for i := range n {
		parts = append(parts, b[i*len(b)/n:(i+1)*len(b)/n])
	}
	return parts
}

// sdWriteSplitBlock writes block on stream as a HEADERS frame with the
// END_STREAM flag set as given and without END_HEADERS, followed by
// n-1 CONTINUATION frames, the last with END_HEADERS.
func (x *smRun) sdWriteSplitBlock(stream uint32, endStream bool, block []byte, n int) {
	parts := sdSplit(block, n)
	x.c.writeHeaders(stream, endStream, false, parts[0])
	for i, p := range parts[1:] {
		x.c.writeContinuation(stream, i == len(parts)-2, p)
	}
}

// sdConnectEchoHandler answers CONNECT requests with 200 and echoes
// the tunnel's bytes. When the request body ends (the client's
// END_STREAM), it writes "[eof]" and returns, ending the response.
func sdConnectEchoHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			http.Error(w, "want CONNECT", http.StatusMethodNotAllowed)
			return
		}
		rc := http.NewResponseController(w)
		w.WriteHeader(200)
		rc.Flush()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				rc.Flush()
			}
			if errors.Is(err, io.EOF) {
				io.WriteString(w, "[eof]")
				return
			}
			if err != nil {
				io.WriteString(w, "[error: "+err.Error()+"]")
				return
			}
		}
	})
}

// sdReadTunnel reads DATA on stream until it has want bytes or the
// stream ends, returning the bytes and whether the stream ended.
func (x *smRun) sdReadTunnel(stream uint32, want int) ([]byte, bool) {
	var got []byte
	for len(got) < want {
		f, closed := x.c.waitFor(reactionTimeout, func(f *rawFrame) bool {
			return f.Stream == stream && (f.Type == ftData || f.Type == ftRSTStream || f.Fields != nil) ||
				f.Type == ftGoAway && f.ErrCode() != http2.ErrCodeNo
		})
		switch {
		case f == nil:
			x.tc.Failf("tunnel: got %q, then nothing within %v (closed=%v); want %d bytes", got, reactionTimeout, closed, want)
			return got, false
		case f.Type == ftData:
			got = append(got, f.Payload...)
			if f.Flags&flagEndStream != 0 {
				return got, true
			}
		case f.Fields != nil:
			x.tc.Failf("tunnel: got a field block %v on the connected stream; want only DATA (RFC 9113 §8.5)", f)
			return got, f.EndStream
		default:
			x.tc.Failf("tunnel: got %v after %q", f, got)
			return got, false
		}
	}
	return got, false
}

var sdCases = []*smCase{
	// Compression state (RFC 9113 §4.3).
	{
		serverConfCase: serverConfCase{
			name:    "refused-stream-hpack-state",
			covers:  "9113-4.3-d1",
			desc:    "RFC 9113 §4.3: \"An endpoint receiving HEADERS, PUSH_PROMISE, or CONTINUATION frames needs to reassemble field blocks and perform decompression even if the frames are to be discarded.\" A request refused for exceeding SETTINGS_MAX_CONCURRENT_STREAMS still updates the dynamic table, and a later request referencing its entries decodes correctly",
			profile: sdProfileMCS2,
		},
		script: func(x *smRun) {
			mcs, ok := x.serverSettings[setMaxConcurrentStreams]
			if !ok || mcs != 2 {
				x.tc.Errorf("server's SETTINGS_MAX_CONCURRENT_STREAMS is %d (present=%v); want 2", mcs, ok)
				return
			}
			// Streams 1 and 3 use up the limit: their uploads stay
			// open until the client ends them.
			for _, s := range []uint32{1, 3} {
				x.c.writeFields(s, false, x.reqFields("POST", "/upload")...)
			}
			if !x.pingRoundTrip() {
				return
			}
			// Stream 5 exceeds the limit. Its field block inserts
			// the x-sd-refused fields into the dynamic table.
			ref := []string{"x-sd-refused-a", "inserted-by-refused-stream-a", "x-sd-refused-b", "inserted-by-refused-stream-b"}
			x.c.writeFields(5, true, x.reqFields("GET", "/info", ref...)...)
			f, closed := x.response(5, reactionTimeout)
			switch {
			case f == nil:
				x.tc.Errorf("no response or RST_STREAM on stream 5 within %v (closed=%v)", reactionTimeout, closed)
				return
			case f.Type == ftRSTStream && (f.ErrCode() == http2.ErrCodeProtocol || f.ErrCode() == http2.ErrCodeRefusedStream):
				x.tc.Logf("server refused stream 5: %v", f)
			case f.Type == ftGoAway:
				x.tc.Failf("got %v for a stream exceeding SETTINGS_MAX_CONCURRENT_STREAMS; want a stream error (RFC 9113 §5.1.2)", f)
				return
			default:
				x.tc.Errorf("got %v on stream 5; want RST_STREAM PROTOCOL_ERROR or REFUSED_STREAM for exceeding SETTINGS_MAX_CONCURRENT_STREAMS=2", f)
				return
			}
			// Finish the uploads, freeing their streams.
			for _, s := range []uint32{1, 3} {
				x.c.writeData(s, true, nil)
				if _, ok := x.sdUpload(s); !ok {
					return
				}
			}
			if !x.pingRoundTrip() {
				return
			}
			blk := x.c.block(x.reqFields("GET", "/info", ref...)...)
			if !hpackUsesDynamicTable(blk) {
				x.tc.Errorf("the field block for stream 7 doesn't use the dynamic table")
				return
			}
			x.c.writeHeaders(7, true, true, blk)
			f, closed = x.response(7, reactionTimeout)
			switch {
			case f == nil:
				x.tc.Failf("no response on stream 7 within %v (closed=%v)", reactionTimeout, closed)
				return
			case f.Type == ftGoAway && f.ErrCode() == http2.ErrCodeCompression:
				x.tc.Failf("Go server treated a field block referencing dynamic table entries inserted by a refused stream's field block as a decoding error (%v): it didn't decompress the refused block (RFC 9113 §4.3)", f)
				return
			case f.Type != ftHeaders:
				x.tc.Failf("got %v on stream 7; want a 200 response", f)
				return
			}
			x.sdUnread(f)
			info, ok := x.sdInfo(7)
			if !ok {
				return
			}
			for i := 0; i < len(ref); i += 2 {
				x.sdCheckHeader(7, info, ref[i], ref[i+1])
			}
		},
	},

	// Stream identifiers (RFC 9113 §5.1.1).
	{
		serverConfCase: serverConfCase{
			name:   "skipped-stream-closed",
			covers: "9113-5.1.1-d1",
			desc:   "RFC 9113 §5.1.1: \"an endpoint may skip a stream identifier, with the effect being that the skipped stream is immediately closed.\" After the client opens stream 5, PRIORITY, WINDOW_UPDATE, and RST_STREAM on stream 3 are frames on a closed stream (which RFC 9113 §5.1 lets the server ignore or treat as STREAM_CLOSED), not an idle stream (a PROTOCOL_ERROR connection error); stream 7 still works",
		},
		script: func(x *smRun) {
			x.c.writeFields(5, true, x.reqFields("GET", "/hello")...)
			if x.expectStatus(5, 200) == nil {
				return
			}
			x.readBody(5, false, reactionTimeout)
			frames := []struct {
				name  string
				write func()
			}{
				{"PRIORITY", func() { x.c.writeRaw(ftPriority, 0, 3, []byte{0, 0, 0, 0, 15}) }},
				{"WINDOW_UPDATE", func() { x.c.writeWindowUpdate(3, 1000) }},
				{"RST_STREAM", func() { x.c.writeRST(3, http2.ErrCodeCancel) }},
			}
			for _, fr := range frames {
				fr.write()
				x.pingSeq++
				data := [8]byte{'s', 'd', 'p', 'i', 'n', 'g', 0, x.pingSeq}
				x.c.writePing(false, data)
				f, closed := x.c.waitFor(reactionTimeout, func(f *rawFrame) bool {
					return f.Type == ftGoAway && f.ErrCode() != http2.ErrCodeNo ||
						f.Type == ftPing && f.Flags&flagAck != 0 && bytes.Equal(f.Payload, data[:])
				})
				switch {
				case f == nil:
					x.tc.Failf("no PING ACK after %s on skipped stream 3 within %v (closed=%v)", fr.name, reactionTimeout, closed)
					return
				case f.Type == ftGoAway && f.ErrCode() == http2.ErrCodeStreamClosed && fr.name != "PRIORITY":
					// RFC 9113 §5.1 "closed": "An endpoint MAY treat
					// receipt of any other type of frame on a closed
					// stream as a connection error of type
					// STREAM_CLOSED".
					x.tc.Logf("server treated %s on closed stream 3 as a connection error STREAM_CLOSED (permitted): %v", fr.name, f)
					return
				case f.Type == ftGoAway && f.ErrCode() == http2.ErrCodeProtocol:
					x.tc.Failf("Go server treated %s on stream 3, which the client skipped by opening stream 5, as a frame on an idle stream (%v); the skipped stream is closed (RFC 9113 §5.1.1)", fr.name, f)
					return
				case f.Type == ftGoAway:
					x.tc.Failf("Go server sent %v after %s on skipped (closed) stream 3", f, fr.name)
					return
				}
				for _, r := range x.c.seen(func(f *rawFrame) bool { return f.Type == ftRSTStream && f.Stream == 3 }) {
					if fr.name == "PRIORITY" || r.ErrCode() != http2.ErrCodeStreamClosed {
						x.tc.Failf("Go server sent %v after %s on skipped (closed) stream 3; want no reaction or STREAM_CLOSED", r, fr.name)
					} else {
						x.tc.Logf("server sent %v after %s on closed stream 3 (permitted)", r, fr.name)
					}
				}
				x.c.mu.Lock()
				x.c.pending = slices.DeleteFunc(x.c.pending, func(f *rawFrame) bool { return f.Type == ftRSTStream && f.Stream == 3 })
				x.c.mu.Unlock()
			}
			x.c.writeFields(7, true, x.reqFields("GET", "/hello")...)
			if x.expectStatus(7, 200) != nil {
				x.readBody(7, false, reactionTimeout)
			}
		},
	},

	// DATA padding (RFC 9113 §6.1).
	{
		serverConfCase: serverConfCase{
			name:    "padded-data-flow-control",
			covers:  "9113-6.1-d1",
			desc:    "RFC 9113 §6.1: \"The entire DATA frame payload is included in flow control, including the Pad Length and Padding fields if present.\" An upload in heavily padded DATA frames, whose payload far exceeds the server's 64KB connection and 16KB stream windows, completes because the server returns credit for the padding",
			profile: goProfiles[1], // 64KB connection window, 16KB stream windows
			timeout: 30 * time.Second,
		},
		script: func(x *smRun) {
			const (
				chunk  = 16
				frames = 2000
				n      = chunk * frames
			)
			fl := newSDFlow(x)
			pad := make([]byte, 255)
			payload := 1 + chunk + len(pad)
			x.c.writeFields(1, false, x.reqFields("POST", "/upload", "content-length", strconv.Itoa(n))...)
			body := Pattern(n)
			x.tc.Logf("sending %d DATA frames, each with %d data bytes and %d padding bytes (%d bytes of payload in all)", frames, chunk, len(pad), frames*payload)
			for i := range frames {
				if !fl.await(1, int64(payload), reactionTimeout) {
					x.tc.Failf("upload stalled after %d of %d padded DATA frames (%d bytes of payload, %d of them padding): client send windows conn=%d stream=%d; the server didn't return flow control credit for padding (RFC 9113 §6.1)",
						i, frames, i*payload, i*(1+len(pad)), fl.conn, fl.streamWin(1))
					return
				}
				x.c.wmu.Lock()
				err := x.c.fr.WriteDataPadded(1, i == frames-1, body[i*chunk:(i+1)*chunk], pad)
				x.c.wmu.Unlock()
				if err != nil {
					x.tc.Errorf("writing DATA: %v", err)
					return
				}
				fl.sent(1, payload)
			}
			u, ok := x.sdUpload(1)
			if !ok {
				return
			}
			if u.Len != n || u.SHA256 != PatternSHA256(n) {
				x.tc.Failf("handler read %d bytes (sha256 %s); want %d bytes of the pattern", u.Len, u.SHA256, n)
			}
		},
	},

	// HEADERS with END_STREAM and CONTINUATION (RFC 9113 §6.2, §8.1).
	{
		serverConfCase: serverConfCase{
			name:   "headers-end-stream-continuation",
			covers: "9113-6.2-d1",
			desc:   "RFC 9113 §6.2: \"a HEADERS frame with the END_STREAM flag set can be followed by CONTINUATION frames on the same stream.\" A request in a HEADERS frame with END_STREAM but not END_HEADERS, followed by two CONTINUATION frames, gets a response",
		},
		script: func(x *smRun) {
			blk := x.c.block(x.reqFields("GET", "/info", "x-sd-a", strings.Repeat("a", 100), "x-sd-b", strings.Repeat("b", 100))...)
			x.sdWriteSplitBlock(1, true, blk, 3)
			info, ok := x.sdInfo(1)
			if !ok {
				return
			}
			x.sdCheckHeader(1, info, "x-sd-a", strings.Repeat("a", 100))
			x.sdCheckHeader(1, info, "x-sd-b", strings.Repeat("b", 100))
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "request-trailers-continuation",
			covers: "9113-8.1-d1",
			desc:   "RFC 9113 §8.1: a message is complete after \"a frame with the END_STREAM flag set (including any CONTINUATION frames needed to complete a field block)\". Declared request trailers in a HEADERS frame with END_STREAM but not END_HEADERS, followed by CONTINUATION frames, all reach the handler",
		},
		script: func(x *smRun) {
			x.c.writeFields(1, false, x.reqFields("POST", "/upload", "trailer", "x-sd-ta, x-sd-tb, x-sd-tc")...)
			x.c.writeData(1, false, []byte("body"))
			tr := []string{"x-sd-ta", "alpha-" + strings.Repeat("a", 50), "x-sd-tb", "beta-" + strings.Repeat("b", 50), "x-sd-tc", "gamma-" + strings.Repeat("c", 50)}
			x.sdWriteSplitBlock(1, true, x.c.block(tr...), 3)
			u, ok := x.sdUpload(1)
			if !ok {
				return
			}
			if u.Len != 4 {
				x.tc.Failf("handler read %d body bytes; want 4", u.Len)
			}
			for i := 0; i < len(tr); i += 2 {
				if got := u.Trailer[tr[i]]; !slices.Equal(got, []string{tr[i+1]}) {
					x.tc.Failf("handler saw trailer %s %s; want [%s] (all trailers: %v)", tr[i], sdShortList(got), sdShort(tr[i+1]), u.Trailer)
				}
			}
		},
	},

	// Flow control (RFC 9113 §6.9).
	{
		serverConfCase: serverConfCase{
			name:    "data-in-error-flow-control",
			covers:  "9113-6.9-d2",
			desc:    "RFC 9113 §6.9: a receiver must account for DATA in flow control \"even if the frame is in error. The sender counts the frame toward the flow-control window, but if the receiver does not, the flow-control window at the sender and receiver can become different.\" DATA sent after END_STREAM (a stream error) fills the connection window; the server returns the credit, and a later upload doesn't stall",
			profile: sdProfileWin64K,
		},
		script: func(x *smRun) {
			// Stream 1 is half-closed (remote) while its handler
			// waits.
			x.c.writeFields(1, true, x.reqFields("GET", "/delay/30000")...)
			if !x.pingRoundTrip() {
				return
			}
			n := int(x.connSendWin)
			if n > x.streamWindow() || int64(n) > int64(x.serverSettings[setMaxFrameSize]) && x.serverSettings[setMaxFrameSize] != 0 {
				x.tc.Errorf("connection window %d doesn't fit in stream 1's window %d or one frame", n, x.streamWindow())
				return
			}
			// DATA after END_STREAM uses all of the connection
			// window.
			x.sendData(1, false, make([]byte, n))
			f, closed := x.c.waitFor(reactionTimeout, func(f *rawFrame) bool {
				return f.Type == ftGoAway && f.ErrCode() != http2.ErrCodeNo || f.Type == ftRSTStream && f.Stream == 1
			})
			switch {
			case f == nil:
				x.tc.Failf("no stream error within %v (closed=%v) for DATA after END_STREAM; want RST_STREAM STREAM_CLOSED (RFC 9113 §5.1)", reactionTimeout, closed)
				return
			case f.Type == ftGoAway:
				x.tc.Logf("server treated DATA after END_STREAM as a connection error: %v", f)
				if f.ErrCode() != http2.ErrCodeStreamClosed {
					x.tc.Failf("got %v for DATA after END_STREAM; want STREAM_CLOSED (RFC 9113 §5.1)", f)
				}
				return
			case f.ErrCode() != http2.ErrCodeStreamClosed:
				x.tc.Failf("got %v for DATA after END_STREAM; want STREAM_CLOSED (RFC 9113 §5.1)", f)
			default:
				x.tc.Logf("got expected stream error: %v", f)
			}
			if !x.pingRoundTrip() {
				return
			}
			if !x.awaitConnCredit(int64(n), reactionTimeout) {
				x.tc.Failf("client's connection send window is %d after the server rejected %d bytes of DATA sent after END_STREAM; want %d: the server didn't count the DATA in error toward the connection window and return the credit (RFC 9113 §6.9)", x.connSendWin, n, n)
				return
			}
			x.upload(3, n)
		},
	},
	{
		serverConfCase: serverConfCase{
			name:    "window-update-rate",
			covers:  "9113-6.9.1-d1",
			desc:    "RFC 9113 §6.9.1: \"Receivers are advised to have mechanisms in place to avoid sending WINDOW_UPDATE frames with very small increments\"; an 8MB upload in 1MB DATA frames shouldn't draw many WINDOW_UPDATE frames per DATA frame (RFC 9113 §10.5: peers may treat excessive control frames as abusive)",
			timeout: 60 * time.Second,
		},
		script: func(x *smRun) {
			const total = 8 << 20
			frame := int(x.serverSettings[setMaxFrameSize])
			if frame == 0 {
				frame = 16384
			}
			x.tc.Logf("server's SETTINGS_MAX_FRAME_SIZE is %d, SETTINGS_INITIAL_WINDOW_SIZE %d", frame, x.streamWindow())
			fl := newSDFlow(x)
			// Absorb the initial connection WINDOW_UPDATE.
			if !x.pingRoundTrip() {
				return
			}
			fl.poll()
			fl.updates = 0
			x.c.writeFields(1, false, x.reqFields("POST", "/upload", "content-length", strconv.Itoa(total))...)
			buf := make([]byte, frame)
			sent, frames, partial := 0, 0, 0
			for sent < total {
				want := min(frame, total-sent)
				// Wait for enough credit for a full frame; if the
				// server holds back a small remainder, send what
				// is available.
				if !fl.await(1, int64(want), time.Second) {
					if fl.broken {
						return
					}
					if fl.avail(1) <= 0 && !fl.await(1, 1, reactionTimeout) {
						x.tc.Failf("upload stalled after %d of %d bytes: client send windows conn=%d stream=%d", sent, total, fl.conn, fl.streamWin(1))
						return
					}
					want = int(min(int64(want), fl.avail(1)))
					partial++
				}
				x.c.writeData(1, sent+want == total, buf[:want])
				fl.sent(1, want)
				sent += want
				frames++
			}
			u, ok := x.sdUpload(1)
			if !ok {
				return
			}
			if u.Len != total {
				x.tc.Failf("handler read %d bytes; want %d", u.Len, total)
			}
			if !x.pingRoundTrip() {
				return
			}
			fl.poll()
			x.tc.Logf("Go server sent %d WINDOW_UPDATE frames for %d DATA frames of up to %d bytes (%d bytes; %d frames smaller than the available credit allowed)", fl.updates, frames, frame, total, partial)
			if fl.updates > 10*frames {
				x.tc.Warnf("Go server sent %d WINDOW_UPDATE frames for %d DATA frames (%.1f per frame, %.1f per MiB); Envoy's default flood protection allows about 10 per DATA frame",
					fl.updates, frames, float64(fl.updates)/float64(frames), float64(fl.updates)/float64(total>>20))
			}
		},
	},
	{
		serverConfCase: serverConfCase{
			name:     "settings-not-connection-window",
			covers:   "9113-6.9.2-d1",
			desc:     "RFC 9113 §6.9.2: \"A SETTINGS frame cannot alter the connection flow-control window.\" With SETTINGS_INITIAL_WINDOW_SIZE=2^31-1 and no connection-level WINDOW_UPDATE from the client, the server sends at most 65535 bytes of response DATA until the client grants connection credit",
			settings: []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 1<<31 - 1}},
		},
		script: func(x *smRun) {
			const n = 1000000
			x.c.writeFields(1, true, x.reqFields("GET", "/bytes/"+strconv.Itoa(n))...)
			if x.expectStatus(1, 200) == nil {
				return
			}
			got := 0
			for {
				f, _ := x.c.waitFor(time.Second, func(f *rawFrame) bool {
					return f.Stream == 1 && (f.Type == ftData || f.Type == ftRSTStream) || f.Type == ftGoAway
				})
				if f == nil {
					break // stalled
				}
				if f.Type != ftData {
					x.tc.Failf("while reading the response body: %v", f)
					return
				}
				got += len(f.Payload)
				if f.Flags&flagEndStream != 0 {
					break
				}
			}
			x.tc.Logf("server sent %d bytes of DATA before stalling", got)
			switch {
			case got > 65535:
				x.tc.Failf("Go server sent %d bytes of DATA with no connection-level WINDOW_UPDATE; the connection window is 65535 regardless of SETTINGS_INITIAL_WINDOW_SIZE=2^31-1 (RFC 9113 §6.9.2)", got)
				return
			case got < 65535:
				x.tc.Logf("server stalled after %d bytes, short of the 65535-byte connection window", got)
			}
			x.c.writeWindowUpdate(0, n)
			rest, ok := x.readBody(1, false, 5*time.Second)
			if !ok || got+len(rest) != n {
				x.tc.Failf("got %d body bytes (complete=%v) after granting connection credit; want %d", got+len(rest), ok, n)
			}
		},
	},

	// Request control data (RFC 9113 §8.3.1).
	{
		serverConfCase: serverConfCase{
			name:   "request-scheme-other",
			covers: "9113-8.3.1-d1",
			desc:   "RFC 9113 §8.3.1: \"':scheme' is not restricted to 'http' and 'https' schemed URIs.\" A request with :scheme \"foo\" isn't malformed; any HTTP response is acceptable",
		},
		script: func(x *smRun) {
			x.c.writeFields(1, true, ":method", "GET", ":scheme", "foo", ":authority", x.authority, ":path", "/info")
			f, closed := x.response(1, reactionTimeout)
			switch {
			case f == nil:
				x.tc.Failf("no response within %v (closed=%v)", reactionTimeout, closed)
			case f.ErrCode() == http2.ErrCodeProtocol:
				x.tc.Failf("Go server treated a request with :scheme \"foo\" as malformed (%v); :scheme is not restricted to http and https (RFC 9113 §8.3.1)", f)
			case f.Type == ftGoAway:
				x.tc.Failf("Go server sent %v for a request with :scheme \"foo\"", f)
			case f.Type == ftRSTStream:
				x.tc.Warnf("Go server reset a request with :scheme \"foo\" (%v) instead of answering it", f)
			default:
				body, _ := x.readBody(1, false, reactionTimeout)
				x.tc.Logf("response %s: %s", f.Status(), body)
			}
		},
	},

	// CONNECT (RFC 9113 §8.5).
	{
		serverConfCase: serverConfCase{
			name:   "connect-tunnel-data",
			covers: "9113-8.5-d1",
			desc:   "RFC 9113 §8.5: \"After the initial HEADERS frame sent by each peer, all subsequent DATA frames correspond to data sent on the TCP connection. The END_STREAM flag on a DATA frame is treated as being equivalent to the TCP FIN bit.\" Tunnel bytes flow both ways as DATA; the client's END_STREAM ends the handler's request body, and the handler returning ends the stream",
		},
		handler: sdConnectEchoHandler(),
		script: func(x *smRun) {
			x.c.writeFields(1, false, ":method", "CONNECT", ":authority", "tunnel.example:443")
			f := x.expectStatus(1, 200)
			if f == nil {
				return
			}
			if f.EndStream {
				x.tc.Failf("CONNECT response HEADERS has END_STREAM; the tunnel is closed")
				return
			}
			msg1 := []byte("first bytes through the tunnel")
			x.sendData(1, false, msg1)
			got, ended := x.sdReadTunnel(1, len(msg1))
			if !bytes.Equal(got, msg1) || ended {
				x.tc.Failf("tunnel echoed %q (ended=%v); want %q", got, ended, msg1)
				return
			}
			msg2 := []byte("last bytes, then FIN")
			x.sendData(1, true, msg2)
			want := append(msg2, "[eof]"...)
			got, ended = x.sdReadTunnel(1, len(want))
			if !bytes.Equal(got, want) {
				x.tc.Failf("tunnel returned %q after the client's END_STREAM; want %q (the handler's request body should end with io.EOF)", got, want)
				return
			}
			if !ended {
				// END_STREAM may come in a later, empty DATA frame.
				rest, ok := x.readBody(1, false, reactionTimeout)
				if !ok || len(rest) > 0 {
					x.tc.Failf("stream didn't end after the handler returned (complete=%v, extra bytes %q); want END_STREAM", ok, rest)
				}
			}
		},
	},

	// Dynamic table (RFC 7541 §4).
	{
		serverConfCase: serverConfCase{
			name:   "hpack-entry-size-huffman",
			covers: "7541-4.1-d1",
			desc:   "RFC 7541 §4.1: \"The size of an entry is the sum of its name's length in octets ..., its value's length in octets, and 32. The size of an entry is calculated using the length of its name and value without any Huffman encoding applied.\" Four entries with Huffman-encoded values fill the 4096-byte dynamic table exactly, and all of them can be referenced",
		},
		script: func(x *smRun) {
			// Each entry has a 6-octet name and a 986-octet value,
			// for a size of exactly 1024. The value's Huffman
			// encoding is longer than the value (RFC 7541 Appendix
			// B: '{' is 15 bits), so a decoder that used encoded
			// lengths would evict the oldest entry when adding the
			// fourth. A decoder that evicted when the table is
			// exactly full would too.
			names := []string{"x-sd-a", "x-sd-b", "x-sd-c", "x-sd-d"}
			values := make([]string, 4)
			blk := x.smHpackRequest(nil, "/info")
			size := 0
			for i, name := range names {
				values[i] = string(rune('0'+i)) + strings.Repeat("{", 985)
				size += len(name) + len(values[i]) + 32
				blk = sdHpackInsert(blk, name, values[i], true)
			}
			if size != 4096 {
				x.tc.Errorf("entries total %d bytes; want 4096", size)
				return
			}
			x.tc.Logf("each value is %d octets, %d Huffman-encoded", len(values[0]), hpack.HuffmanEncodeLength(values[0]))
			x.c.writeHeaders(1, true, true, blk)
			info, ok := x.sdInfo(1)
			if !ok {
				return
			}
			for i, name := range names {
				x.sdCheckHeader(1, info, name, values[i])
			}
			// Reference all four entries: index 62 is the newest
			// (x-sd-d) and 65 the oldest (x-sd-a).
			blk = x.smHpackRequest(nil, "/info")
			for idx := uint64(65); idx >= 62; idx-- {
				blk = sdHpackIndexed(blk, idx)
			}
			x.c.writeHeaders(3, true, true, blk)
			f, closed := x.response(3, reactionTimeout)
			switch {
			case f == nil:
				x.tc.Failf("no response within %v (closed=%v)", reactionTimeout, closed)
				return
			case f.Type == ftGoAway && f.ErrCode() == http2.ErrCodeCompression:
				x.tc.Failf("Go server treated a reference to the oldest of four entries totaling exactly 4096 bytes as a decoding error (%v): it computed entry sizes wrongly (RFC 7541 §4.1)", f)
				return
			case f.Type != ftHeaders:
				x.tc.Failf("got %v; want a 200 response", f)
				return
			}
			x.sdUnread(f)
			if info, ok = x.sdInfo(3); !ok {
				return
			}
			for i, name := range names {
				x.sdCheckHeader(3, info, name, values[i])
			}
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "hpack-size-update-evicts",
			covers: "7541-4.3-d1",
			desc:   "RFC 7541 §4.3: \"Whenever the maximum size for the dynamic table is reduced, entries are evicted from the end of the dynamic table until the size of the dynamic table is less than or equal to the maximum size.\" After a size update evicts the older of two entries, the newer one is still referenced correctly, and a reference to the evicted one is a decoding error",
		},
		script: func(x *smRun) {
			// Two 55-octet entries.
			blk := x.smHpackRequest(nil, "/info")
			blk = sdHpackInsert(blk, "x-sd-evict-old", "old-value", false)
			blk = sdHpackInsert(blk, "x-sd-evict-new", "new-value", false)
			x.c.writeHeaders(1, true, true, blk)
			if _, ok := x.sdInfo(1); !ok {
				return
			}
			// A size update to 60 evicts x-sd-evict-old. Index 62
			// is still x-sd-evict-new.
			blk = sdHpackSizeUpdate(nil, 60)
			blk = x.smHpackRequest(blk, "/info")
			blk = sdHpackIndexed(blk, 62)
			x.c.writeHeaders(3, true, true, blk)
			info, ok := x.sdInfo(3)
			if !ok {
				return
			}
			x.sdCheckHeader(3, info, "x-sd-evict-new", "new-value")
			x.sdCheckHeader(3, info, "x-sd-evict-old")
			// Index 63 was x-sd-evict-old.
			blk = x.smHpackRequest(nil, "/info")
			blk = sdHpackIndexed(blk, 63)
			x.c.writeHeaders(5, true, true, blk)
			x.sdExpectDecodingError(5, "a reference to a dynamic table entry evicted by a dynamic table size update")
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "hpack-entry-larger-than-table",
			covers: "7541-4.4-d1",
			desc:   "RFC 7541 §4.4: \"It is not an error to attempt to add an entry that is larger than the maximum size; an attempt to add an entry larger than the maximum size causes the table to be emptied of all existing entries and results in an empty table.\" The request with the oversized entry succeeds, and a later reference to an older entry is a decoding error",
		},
		script: func(x *smRun) {
			blk := x.smHpackRequest(nil, "/info")
			blk = sdHpackInsert(blk, "x-sd-small", "small-value", false)
			x.c.writeHeaders(1, true, true, blk)
			if _, ok := x.sdInfo(1); !ok {
				return
			}
			// An entry of 9+4100+32 octets, more than the 4096-byte
			// table.
			huge := strings.Repeat("h", 4100)
			blk = x.smHpackRequest(nil, "/info")
			blk = sdHpackInsert(blk, "x-sd-huge", huge, false)
			x.c.writeHeaders(3, true, true, blk)
			f, closed := x.response(3, reactionTimeout)
			switch {
			case f == nil:
				x.tc.Failf("no response within %v (closed=%v)", reactionTimeout, closed)
				return
			case f.ErrCode() == http2.ErrCodeCompression:
				x.tc.Failf("Go server treated adding an entry larger than the dynamic table as a decoding error (%v); it isn't an error (RFC 7541 §4.4)", f)
				return
			case f.Type != ftHeaders:
				x.tc.Failf("got %v; want a 200 response", f)
				return
			}
			x.sdUnread(f)
			info, ok := x.sdInfo(3)
			if !ok {
				return
			}
			x.sdCheckHeader(3, info, "x-sd-huge", huge)
			// The table is empty: index 62 was x-sd-small.
			blk = x.smHpackRequest(nil, "/info")
			blk = sdHpackIndexed(blk, 62)
			x.c.writeHeaders(5, true, true, blk)
			x.sdExpectDecodingError(5, "a reference to a dynamic table entry after adding an entry larger than the table emptied it")
		},
	},
	{
		serverConfCase: serverConfCase{
			name:   "hpack-insert-name-of-evicted",
			covers: "7541-4.4-d2",
			desc:   "RFC 7541 §4.4: \"A new entry can reference the name of an entry in the dynamic table that will be evicted when adding this new entry into the dynamic table.\" A literal with incremental indexing naming the only entry, which its insertion evicts, decodes with that name, and the new entry is referenced correctly",
		},
		script: func(x *smRun) {
			const name = "x-sd-evicted-name"
			// Shrink the table to 100 octets and add a 51-octet
			// entry.
			blk := sdHpackSizeUpdate(nil, 100)
			blk = x.smHpackRequest(blk, "/info")
			blk = sdHpackInsert(blk, name, "v1", false)
			x.c.writeHeaders(1, true, true, blk)
			info, ok := x.sdInfo(1)
			if !ok {
				return
			}
			x.sdCheckHeader(1, info, name, "v1")
			// A 79-octet entry named by index 62: adding it evicts
			// the entry it names (51+79 > 100).
			v2 := "second-value-" + strings.Repeat("z", 17)
			blk = x.smHpackRequest(nil, "/info")
			blk = sdHpackInsertIndexedName(blk, 62, v2)
			x.c.writeHeaders(3, true, true, blk)
			if info, ok = x.sdInfo(3); !ok {
				return
			}
			x.sdCheckHeader(3, info, name, v2)
			blk = x.smHpackRequest(nil, "/info")
			blk = sdHpackIndexed(blk, 62)
			x.c.writeHeaders(5, true, true, blk)
			if info, ok = x.sdInfo(5); !ok {
				return
			}
			x.sdCheckHeader(5, info, name, v2)
		},
	},

	// Integer representation (RFC 7541 §5.1).
	{
		serverConfCase: serverConfCase{
			name:   "hpack-integer-prefix-max",
			covers: "7541-5.1-d1",
			desc:   "RFC 7541 §5.1: if the value doesn't fit in the prefix, \"all the bits of the prefix are set to 1, and the value, decreased by 2^N-1, is encoded using a list of one or more octets.\" Values of exactly 2^N-1, encoded as an all-ones prefix and a 0x00 octet, decode correctly: an index of 127 (7-bit prefix), a name index of 63 (6-bit), a dynamic table size of 31 (5-bit), a name index of 15 (4-bit), and string lengths of 127",
		},
		script: func(x *smRun) {
			// 66 entries of 44 octets, so that index 127 is the
			// oldest one (index 62 is the newest).
			blk := x.smHpackRequest(nil, "/info")
			for i := range 66 {
				blk = sdHpackInsert(blk, fmt.Sprintf("x-sd-i-%02d", i), fmt.Sprintf("v%02d", i), false)
			}
			x.c.writeHeaders(1, true, true, blk)
			if _, ok := x.sdInfo(1); !ok {
				return
			}
			longName := "x-sd-" + strings.Repeat("n", 122)
			longValue := strings.Repeat("w", 127)
			blk = x.smHpackRequest(nil, "/info")
			start := len(blk)
			// Indexed field, index 127: 0xff 0x00.
			blk = sdHpackIndexed(blk, 127)
			// Literal with incremental indexing, name index 63
			// (x-sd-i-64): 0x7f 0x00, and a value of length 127:
			// 0x7f 0x00.
			blk = sdHpackInsertIndexedName(blk, 63, longValue)
			// Literal without indexing, name index 15
			// (accept-charset): 0x0f 0x00.
			blk = smHpackInt(blk, 4, 0x00, 15)
			blk = sdHpackString(blk, "sd-15", false)
			// Literal without indexing with a 127-octet name.
			blk = smHpackLiteral(blk, false, longName, "sd-127")
			want := []byte{0xff, 0x00, 0x7f, 0x00, 0x7f, 0x00}
			if got := blk[start : start+6]; !bytes.Equal(got, want) {
				x.tc.Errorf("block starts with % x; want % x", got, want)
				return
			}
			x.tc.Logf("field block representations: %s", hex.EncodeToString(blk[start:start+8]))
			x.c.writeHeaders(3, true, true, blk)
			info, ok := x.sdInfo(3)
			if !ok {
				return
			}
			x.sdCheckHeader(3, info, "x-sd-i-00", "v00")
			x.sdCheckHeader(3, info, "x-sd-i-64", longValue)
			x.sdCheckHeader(3, info, "accept-charset", "sd-15")
			x.sdCheckHeader(3, info, longName, "sd-127")
			// A dynamic table size update to 31: 0x3f 0x00. It
			// empties the table, so x-sd-i-65 is a literal.
			blk = sdHpackSizeUpdate(nil, 31)
			if !bytes.Equal(blk, []byte{0x3f, 0x00}) {
				x.tc.Errorf("size update encoded as % x; want 3f 00", blk)
				return
			}
			blk = x.smHpackRequest(blk, "/info")
			blk = smHpackLiteral(blk, false, "x-sd-after-update", "ok")
			x.c.writeHeaders(5, true, true, blk)
			if info, ok = x.sdInfo(5); !ok {
				return
			}
			x.sdCheckHeader(5, info, "x-sd-after-update", "ok")
		},
	},

	// Extended CONNECT setting (RFC 8441 §3).
	{
		serverConfCase: serverConfCase{
			name:     "client-enable-connect-protocol",
			covers:   "8441-3-d1",
			desc:     "RFC 8441 §3: SETTINGS_ENABLE_CONNECT_PROTOCOL: \"Receipt of this parameter by a server does not have any impact.\" After the client sends it with value 1, requests work normally",
			settings: []http2.Setting{{ID: http2.SettingEnableConnectProtocol, Val: 1}},
		},
		script: func(x *smRun) {
			if !x.pingRoundTrip() {
				return
			}
			x.c.writeFields(1, true, x.reqFields("GET", "/hello")...)
			if x.expectStatus(1, 200) == nil {
				return
			}
			body, ok := x.readBody(1, false, reactionTimeout)
			if !ok || string(body) != helloBody {
				x.tc.Failf("got body %q (complete=%v); want %q", body, ok, helloBody)
			}
			x.upload(3, 1000)
		},
	},
}
