// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http/internal/http2"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2/hpack"
)

// rawConn is a scripted HTTP/2 endpoint used by the conformance tests
// to send arbitrary (including invalid) frames to the Go client or
// server and observe its reaction. It writes frames using the
// http2.Framer (with AllowIllegalWrites), and reads frames with its
// own minimal parser so that it doesn't share bugs with the
// implementation under test.
type rawConn struct {
	tc   *testCtx
	who  string // "server" or "client": which side this rawConn plays
	conn net.Conn

	wmu    sync.Mutex
	fr     *http2.Framer
	encBuf bytes.Buffer
	enc    *hpack.Encoder

	frames chan *rawFrame // closed when the connection is closed

	mu      sync.Mutex
	pending []*rawFrame // frames read but not yet consumed by waitFor
}

// rawFrame is a frame received from the implementation under test.
type rawFrame struct {
	Type    byte
	Flags   byte
	Stream  uint32
	Payload []byte

	// Fields is set for the HEADERS, PUSH_PROMISE, or CONTINUATION
	// frame that completes a field block.
	Fields    []hpack.HeaderField
	HPACKErr  error
	EndStream bool // for field blocks: the HEADERS frame had END_STREAM
}

func (f *rawFrame) String() string {
	s := fmt.Sprintf("%s stream=%d flags=0x%x len=%d", frameTypeName(f.Type), f.Stream, f.Flags, len(f.Payload))
	switch f.Type {
	case ftGoAway:
		if len(f.Payload) >= 8 {
			s += fmt.Sprintf(" last=%d code=%s debug=%q", binary.BigEndian.Uint32(f.Payload)&(1<<31-1),
				errCodeName(binary.BigEndian.Uint32(f.Payload[4:])), f.Payload[8:])
		}
	case ftRSTStream:
		if len(f.Payload) == 4 {
			s += " code=" + errCodeName(binary.BigEndian.Uint32(f.Payload))
		}
	case ftWindowUpdate:
		if len(f.Payload) == 4 {
			s += fmt.Sprintf(" incr=%d", binary.BigEndian.Uint32(f.Payload))
		}
	}
	if f.Fields != nil {
		s += " " + fieldsTrace(f.Fields)
	}
	return s
}

// ErrCode returns the error code of a GOAWAY or RST_STREAM frame.
func (f *rawFrame) ErrCode() http2.ErrCode {
	switch {
	case f.Type == ftGoAway && len(f.Payload) >= 8:
		return http2.ErrCode(binary.BigEndian.Uint32(f.Payload[4:]))
	case f.Type == ftRSTStream && len(f.Payload) == 4:
		return http2.ErrCode(binary.BigEndian.Uint32(f.Payload))
	}
	return 0
}

// Status returns the :status of a field block, or "".
func (f *rawFrame) Status() string {
	for _, hf := range f.Fields {
		if hf.Name == ":status" {
			return hf.Value
		}
	}
	return ""
}

func newRawConn(tc *testCtx, who string, c net.Conn) *rawConn {
	rc := &rawConn{
		tc:     tc,
		who:    who,
		conn:   c,
		frames: make(chan *rawFrame, 1000),
	}
	rc.fr = http2.NewFramer(c, nil)
	rc.fr.AllowIllegalWrites = true
	rc.enc = hpack.NewEncoder(&rc.encBuf)
	tc.Cleanup(func() { c.Close() })
	return rc
}

// startReading starts reading frames, after any preface has been consumed.
func (rc *rawConn) startReading() {
	go func() {
		defer close(rc.frames)
		dec := hpack.NewDecoder(4096, nil)
		dec.SetMaxStringLength(16 << 20)
		var block []byte
		var blockStart *rawFrame
		hdr := make([]byte, 9)
		for {
			if _, err := io.ReadFull(rc.conn, hdr); err != nil {
				rc.tc.Logf("raw %s: read: %v", rc.who, err)
				return
			}
			n := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
			f := &rawFrame{Type: hdr[3], Flags: hdr[4], Stream: binary.BigEndian.Uint32(hdr[5:]) & (1<<31 - 1)}
			f.Payload = make([]byte, n)
			if _, err := io.ReadFull(rc.conn, f.Payload); err != nil {
				rc.tc.Logf("raw %s: read: %v", rc.who, err)
				return
			}
			switch f.Type {
			case ftHeaders, ftPushPromise:
				frag := f.Payload
				if f.Flags&flagPadded != 0 && len(frag) > 0 {
					pad := int(frag[0])
					frag = frag[1:]
					if pad <= len(frag) {
						frag = frag[:len(frag)-pad]
					}
				}
				if f.Type == ftHeaders && f.Flags&flagPriority != 0 && len(frag) >= 5 {
					frag = frag[5:]
				}
				if f.Type == ftPushPromise && len(frag) >= 4 {
					frag = frag[4:]
				}
				block = append([]byte(nil), frag...)
				blockStart = f
			case ftContinuation:
				block = append(block, f.Payload...)
			}
			if (f.Type == ftHeaders || f.Type == ftPushPromise || f.Type == ftContinuation) && f.Flags&flagEndHeaders != 0 && blockStart != nil {
				f.Fields, f.HPACKErr = dec.DecodeFull(block)
				if f.Fields == nil {
					f.Fields = []hpack.HeaderField{}
				}
				f.EndStream = blockStart.Type == ftHeaders && blockStart.Flags&flagEndStream != 0
				blockStart = nil
			}
			rc.tc.Logf("raw %s: recv %v", rc.who, f)
			rc.frames <- f
		}
	}()
}

// waitFor returns the first unconsumed frame for which match returns
// true, waiting up to timeout. Frames that don't match are left for
// later calls. It returns nil and closed=true if the connection
// closed first, or nil and closed=false on timeout.
func (rc *rawConn) waitFor(timeout time.Duration, match func(*rawFrame) bool) (f *rawFrame, closed bool) {
	rc.mu.Lock()
	for i, f := range rc.pending {
		if match(f) {
			rc.pending = slices.Delete(rc.pending, i, i+1)
			rc.mu.Unlock()
			return f, false
		}
	}
	rc.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-rc.frames:
			if !ok {
				return nil, true
			}
			if match(f) {
				return f, false
			}
			rc.mu.Lock()
			rc.pending = append(rc.pending, f)
			rc.mu.Unlock()
		case <-timer.C:
			return nil, false
		}
	}
}

// seen returns all frames received so far that match, without
// consuming them.
func (rc *rawConn) seen(match func(*rawFrame) bool) []*rawFrame {
	rc.drain()
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []*rawFrame
	for _, f := range rc.pending {
		if match(f) {
			out = append(out, f)
		}
	}
	return out
}

// drain moves frames that have been read to rc.pending.
func (rc *rawConn) drain() {
	for {
		select {
		case f, ok := <-rc.frames:
			if !ok {
				return
			}
			rc.mu.Lock()
			rc.pending = append(rc.pending, f)
			rc.mu.Unlock()
		default:
			return
		}
	}
}

const reactionTimeout = 3 * time.Second

func isType(t byte) func(*rawFrame) bool {
	return func(f *rawFrame) bool { return f.Type == t }
}

func codesString(codes []http2.ErrCode) string {
	var s []string
	for _, c := range codes {
		s = append(s, c.String())
	}
	return strings.Join(s, " or ")
}

// expectConnError expects the implementation to send GOAWAY with one
// of codes (RFC 9113 §5.4.1).
func (rc *rawConn) expectConnError(codes ...http2.ErrCode) {
	f, closed := rc.waitFor(reactionTimeout, isType(ftGoAway))
	switch {
	case f != nil && slices.Contains(codes, f.ErrCode()):
		rc.tc.Logf("got expected connection error: %v", f)
		// RFC 9113 §5.4.1: "After sending the GOAWAY frame for an
		// error condition, the endpoint MUST close the TCP connection."
		if _, closed := rc.waitFor(reactionTimeout, func(*rawFrame) bool { return false }); !closed {
			rc.tc.Failf("connection still open %v after GOAWAY %v (RFC 9113 §5.4.1: MUST close the TCP connection)", reactionTimeout, f.ErrCode())
		}
	case f != nil:
		rc.tc.Failf("got GOAWAY %v; want connection error %s", f.ErrCode(), codesString(codes))
	case closed:
		rc.tc.Warnf("connection closed without GOAWAY; want connection error %s (RFC 9113 §5.4.1: SHOULD send GOAWAY)", codesString(codes))
	default:
		rc.tc.Failf("no connection error within %v; want GOAWAY %s", reactionTimeout, codesString(codes))
	}
}

// expectStreamError expects the implementation to reset stream with
// one of codes (RFC 9113 §5.4.2). A connection error with one of the
// codes is accepted too.
func (rc *rawConn) expectStreamError(stream uint32, codes ...http2.ErrCode) {
	f, closed := rc.waitFor(reactionTimeout, func(f *rawFrame) bool {
		return f.Type == ftGoAway || (f.Type == ftRSTStream && f.Stream == stream)
	})
	switch {
	case f != nil && f.Type == ftRSTStream && slices.Contains(codes, f.ErrCode()):
		rc.tc.Logf("got expected stream error: %v", f)
	case f != nil && f.Type == ftGoAway && slices.Contains(codes, f.ErrCode()):
		rc.tc.Logf("got connection error instead of stream error: %v", f)
	case f != nil:
		rc.tc.Failf("got %v; want stream error %s", f, codesString(codes))
	case closed:
		rc.tc.Failf("connection closed; want stream error %s", codesString(codes))
	default:
		rc.tc.Failf("no stream error within %v; want RST_STREAM %s on stream %d", reactionTimeout, codesString(codes), stream)
	}
}

// expectNoError checks that no GOAWAY or RST_STREAM with an error
// code arrives within d.
func (rc *rawConn) expectNoError(d time.Duration) {
	f, _ := rc.waitFor(d, func(f *rawFrame) bool {
		return (f.Type == ftGoAway || f.Type == ftRSTStream) && f.ErrCode() != http2.ErrCodeNo
	})
	if f != nil {
		rc.tc.Failf("unexpected error: %v", f)
	}
}

// expectPingAck expects a PING ACK echoing data.
func (rc *rawConn) expectPingAck(data [8]byte) {
	f, _ := rc.waitFor(reactionTimeout, func(f *rawFrame) bool {
		return f.Type == ftPing && f.Flags&flagAck != 0
	})
	if f == nil {
		rc.tc.Failf("no PING ACK received (RFC 9113 §6.7: receivers MUST send a PING ACK)")
	} else if !bytes.Equal(f.Payload, data[:]) {
		rc.tc.Failf("PING ACK payload %x; want %x", f.Payload, data)
	}
}

// Writers. Errors are reported as test infrastructure errors.

func (rc *rawConn) check(err error) {
	if err != nil {
		rc.tc.Logf("raw %s: write: %v", rc.who, err)
	}
}

// block HPACK-encodes fields, given as name, value pairs.
func (rc *rawConn) block(kv ...string) []byte {
	rc.wmu.Lock()
	defer rc.wmu.Unlock()
	rc.encBuf.Reset()
	for i := 0; i+1 < len(kv); i += 2 {
		rc.enc.WriteField(hpack.HeaderField{Name: kv[i], Value: kv[i+1]})
	}
	return bytes.Clone(rc.encBuf.Bytes())
}

func (rc *rawConn) logSend(format string, args ...any) {
	rc.tc.Logf("raw %s: send "+format, append([]any{rc.who}, args...)...)
}

func (rc *rawConn) writeSettings(s ...http2.Setting) {
	rc.logSend("SETTINGS %v", s)
	rc.wmu.Lock()
	defer rc.wmu.Unlock()
	rc.check(rc.fr.WriteSettings(s...))
}

func (rc *rawConn) writeSettingsAck() {
	rc.logSend("SETTINGS ACK")
	rc.wmu.Lock()
	defer rc.wmu.Unlock()
	rc.check(rc.fr.WriteSettingsAck())
}

// writeHeaders writes a HEADERS frame with the given block.
func (rc *rawConn) writeHeaders(stream uint32, endStream, endHeaders bool, block []byte) {
	rc.logSend("HEADERS stream=%d endStream=%v endHeaders=%v len=%d", stream, endStream, endHeaders, len(block))
	rc.wmu.Lock()
	defer rc.wmu.Unlock()
	rc.check(rc.fr.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      stream,
		BlockFragment: block,
		EndStream:     endStream,
		EndHeaders:    endHeaders,
	}))
}

// writeFields encodes kv and writes it as a complete HEADERS frame.
func (rc *rawConn) writeFields(stream uint32, endStream bool, kv ...string) {
	rc.tc.Logf("raw %s: fields %q", rc.who, kv)
	rc.writeHeaders(stream, endStream, true, rc.block(kv...))
}

func (rc *rawConn) writeContinuation(stream uint32, endHeaders bool, block []byte) {
	rc.logSend("CONTINUATION stream=%d endHeaders=%v len=%d", stream, endHeaders, len(block))
	rc.wmu.Lock()
	defer rc.wmu.Unlock()
	rc.check(rc.fr.WriteContinuation(stream, endHeaders, block))
}

func (rc *rawConn) writeData(stream uint32, endStream bool, data []byte) {
	rc.logSend("DATA stream=%d endStream=%v len=%d", stream, endStream, len(data))
	rc.wmu.Lock()
	defer rc.wmu.Unlock()
	rc.check(rc.fr.WriteData(stream, endStream, data))
}

func (rc *rawConn) writeRaw(typ byte, flags byte, stream uint32, payload []byte) {
	rc.logSend("%s (raw) stream=%d flags=0x%x len=%d", frameTypeName(typ), stream, flags, len(payload))
	rc.wmu.Lock()
	defer rc.wmu.Unlock()
	rc.check(rc.fr.WriteRawFrame(http2.FrameType(typ), http2.Flags(flags), stream, payload))
}

func (rc *rawConn) writeWindowUpdate(stream, incr uint32) {
	rc.writeRaw(ftWindowUpdate, 0, stream, binary.BigEndian.AppendUint32(nil, incr))
}

func (rc *rawConn) writeRST(stream uint32, code http2.ErrCode) {
	rc.writeRaw(ftRSTStream, 0, stream, binary.BigEndian.AppendUint32(nil, uint32(code)))
}

func (rc *rawConn) writeGoAway(last uint32, code http2.ErrCode) {
	p := binary.BigEndian.AppendUint32(nil, last)
	p = binary.BigEndian.AppendUint32(p, uint32(code))
	rc.writeRaw(ftGoAway, 0, 0, p)
}

func (rc *rawConn) writePing(ack bool, data [8]byte) {
	var flags byte
	if ack {
		flags = flagAck
	}
	rc.writeRaw(ftPing, flags, 0, data[:])
}

// settingsPayload encodes settings, given as id, value pairs.
func settingsPayload(kv ...uint32) []byte {
	var p []byte
	for i := 0; i+1 < len(kv); i += 2 {
		p = binary.BigEndian.AppendUint16(p, uint16(kv[i]))
		p = binary.BigEndian.AppendUint32(p, kv[i+1])
	}
	return p
}
