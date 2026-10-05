// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/http2/hpack"
)

// This file implements a passive HTTP/2 connection validator. It is
// fed the raw bytes of both directions of a connection (by the tap,
// see tap.go) and checks each frame against the rules of RFC 9113 and
// RFC 7541, attributing each finding to the endpoint that sent the
// offending frame.
//
// The validator is deliberately independent of the frame parser in
// net/http/internal/http2 so that it doesn't share its bugs. Only
// HPACK decoding is shared.
//
// Because the validator sits between the two endpoints, it can't know
// exactly when an endpoint received a frame from its peer. The tap
// feeds each chunk of bytes to the validator before forwarding it, so
// the validator always learns about a frame no later than its
// recipient does. Rules are written so that this can only make the
// validator more permissive, never produce false positives: limits
// that a recipient lowers are only enforced once the sender has
// acknowledged them, and frames that might have been sent before
// their sender saw a racing frame from its peer are not flagged.
//
// Some rules depend on what an endpoint has received, such as "after
// receiving a RST_STREAM on a stream, the receiver MUST NOT send
// additional frames for that stream". The validator enforces these
// only once the endpoint has provably processed the frame in
// question: when it acknowledges a SETTINGS or PING frame, it must
// have processed every frame its peer sent before that one.

// Severity is the severity of a Finding.
type Severity int

const (
	// SevInfo is a notable protocol event, such as an endpoint
	// sending GOAWAY or RST_STREAM with an error code.
	SevInfo Severity = iota
	// SevWarning is a violation of a SHOULD-level requirement or
	// a likely-but-not-certain problem.
	SevWarning
	// SevViolation is a violation of a MUST-level requirement.
	SevViolation
)

func (s Severity) String() string {
	switch s {
	case SevInfo:
		return "info"
	case SevWarning:
		return "warning"
	case SevViolation:
		return "VIOLATION"
	}
	return "?"
}

// Finding is something the validator noticed.
type Finding struct {
	Sev    Severity
	Sender string // the endpoint that sent the frame, e.g. "Go server"
	SentBy int    // sideClient or sideServer
	Rule   string // e.g. "RFC 9113 §6.9.1"
	Stream uint32
	Msg    string
}

func (f Finding) String() string {
	s := fmt.Sprintf("%s: %s sent", f.Sev, f.Sender)
	if f.Stream != 0 {
		s += fmt.Sprintf(" (stream %d)", f.Stream)
	}
	s += ": " + f.Msg
	if f.Rule != "" {
		s += " [" + ruleCitation(f.Rule) + "]"
	}
	return s
}

const (
	sideClient = 0
	sideServer = 1
)

// Frame types (RFC 9113 §6, RFC 9218 §7.1, RFC 7838, RFC 8336).
const (
	ftData           = 0x0
	ftHeaders        = 0x1
	ftPriority       = 0x2
	ftRSTStream      = 0x3
	ftSettings       = 0x4
	ftPushPromise    = 0x5
	ftPing           = 0x6
	ftGoAway         = 0x7
	ftWindowUpdate   = 0x8
	ftContinuation   = 0x9
	ftAltSvc         = 0xa
	ftOrigin         = 0xc
	ftPriorityUpdate = 0x10
)

var frameTypeNames = map[byte]string{
	ftData: "DATA", ftHeaders: "HEADERS", ftPriority: "PRIORITY",
	ftRSTStream: "RST_STREAM", ftSettings: "SETTINGS", ftPushPromise: "PUSH_PROMISE",
	ftPing: "PING", ftGoAway: "GOAWAY", ftWindowUpdate: "WINDOW_UPDATE",
	ftContinuation: "CONTINUATION", ftAltSvc: "ALTSVC", ftOrigin: "ORIGIN",
	ftPriorityUpdate: "PRIORITY_UPDATE",
}

func frameTypeName(t byte) string {
	if s, ok := frameTypeNames[t]; ok {
		return s
	}
	return fmt.Sprintf("UNKNOWN_0x%x", t)
}

// Flags.
const (
	flagEndStream  = 0x1
	flagAck        = 0x1
	flagEndHeaders = 0x4
	flagPadded     = 0x8
	flagPriority   = 0x20
)

// Settings identifiers (RFC 9113 §6.5.2, RFC 8441 §3, RFC 9218 §2.1).
const (
	setHeaderTableSize       = 0x1
	setEnablePush            = 0x2
	setMaxConcurrentStreams  = 0x3
	setInitialWindowSize     = 0x4
	setMaxFrameSize          = 0x5
	setMaxHeaderListSize     = 0x6
	setEnableConnectProtocol = 0x8
	setNoRFC7540Priorities   = 0x9
)

var settingNames = map[uint16]string{
	setHeaderTableSize: "HEADER_TABLE_SIZE", setEnablePush: "ENABLE_PUSH",
	setMaxConcurrentStreams: "MAX_CONCURRENT_STREAMS", setInitialWindowSize: "INITIAL_WINDOW_SIZE",
	setMaxFrameSize: "MAX_FRAME_SIZE", setMaxHeaderListSize: "MAX_HEADER_LIST_SIZE",
	setEnableConnectProtocol: "ENABLE_CONNECT_PROTOCOL", setNoRFC7540Priorities: "NO_RFC7540_PRIORITIES",
}

func settingName(id uint16) string {
	if s, ok := settingNames[id]; ok {
		return s
	}
	return fmt.Sprintf("0x%x", id)
}

var settingDefaults = map[uint16]uint32{
	setHeaderTableSize:       4096,
	setEnablePush:            1,
	setMaxConcurrentStreams:  1<<32 - 1,
	setInitialWindowSize:     65535,
	setMaxFrameSize:          16384,
	setMaxHeaderListSize:     1<<32 - 1,
	setEnableConnectProtocol: 0,
}

// Error codes (RFC 9113 §7).
var errCodeNames = []string{
	"NO_ERROR", "PROTOCOL_ERROR", "INTERNAL_ERROR", "FLOW_CONTROL_ERROR",
	"SETTINGS_TIMEOUT", "STREAM_CLOSED", "FRAME_SIZE_ERROR", "REFUSED_STREAM",
	"CANCEL", "COMPRESSION_ERROR", "CONNECT_ERROR", "ENHANCE_YOUR_CALM",
	"INADEQUATE_SECURITY", "HTTP_1_1_REQUIRED",
}

func errCodeName(c uint32) string {
	if int(c) < len(errCodeNames) {
		return errCodeNames[c]
	}
	return fmt.Sprintf("0x%x", c)
}

const clientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

const maxWindow = 1<<31 - 1

// settingsState tracks the SETTINGS sent by one endpoint, which
// constrain what its peer may send.
type settingsState struct {
	acked   map[uint16]uint32
	pending []map[uint16]uint32 // sent but not yet acknowledged, oldest first
}

func newSettingsState() *settingsState {
	return &settingsState{acked: map[uint16]uint32{}}
}

func (s *settingsState) ackedVal(id uint16) uint32 {
	if v, ok := s.acked[id]; ok {
		return v
	}
	return settingDefaults[id]
}

// permissiveMax returns the largest value of setting id that's in
// effect or pending acknowledgment.
func (s *settingsState) permissiveMax(id uint16) uint32 {
	v := s.ackedVal(id)
	for _, p := range s.pending {
		if pv, ok := p[id]; ok {
			v = max(v, pv)
		}
	}
	return v
}

// latest returns the most recently sent value of setting id.
func (s *settingsState) latest(id uint16) uint32 {
	for i := len(s.pending) - 1; i >= 0; i-- {
		if v, ok := s.pending[i][id]; ok {
			return v
		}
	}
	return s.ackedVal(id)
}

// endpoint tracks state about what one endpoint has sent.
type endpoint struct {
	side    int
	name    string
	buf     []byte
	broken  bool // framing lost; stop parsing this direction
	preface bool // client preface (client) or first SETTINGS (both) seen
	nframes int

	settings *settingsState // SETTINGS sent by this endpoint

	// effInitWindow is the permissive SETTINGS_INITIAL_WINDOW_SIZE
	// that this endpoint has granted to its peer. Increases take
	// effect when sent, decreases when acknowledged.
	effInitWindow int64

	// sendWindow is the connection-level flow control window
	// available to this endpoint for sending DATA.
	sendWindow int64

	// strictInitWindow is the SETTINGS_INITIAL_WINDOW_SIZE most
	// recently sent by this endpoint. Unlike effInitWindow, changes
	// take effect immediately. It's used to compute the strictest
	// possible view of the peer's flow control windows, to decide
	// whether a FLOW_CONTROL_ERROR sent by this endpoint is justified.
	strictInitWindow int64

	// flowFault reports whether this endpoint has sent anything that
	// might justify a connection-level FLOW_CONTROL_ERROR.
	flowFault bool

	hb *headerBlock // in-progress header block sent by this endpoint

	dec        *hpack.Decoder // decodes field blocks sent by this endpoint
	decFields  []hpack.HeaderField
	encMaxSize uint32 // dynamic table size the encoder last signaled

	maxOpened   uint32          // highest stream ID opened by this endpoint
	touched     map[uint32]bool // stream IDs this endpoint has sent any frame on
	lastGoAway  int64           // last-stream-ID in the most recent GOAWAY sent, or -1
	goAwayIdx   int             // index of the first GOAWAY this endpoint sent, or 0
	goAwayCode  uint32          // error code of the first GOAWAY with an error this endpoint sent
	pings       map[[8]byte]int // outstanding PINGs sent by this endpoint -> frame index
	connectProt bool            // sent SETTINGS_ENABLE_CONNECT_PROTOCOL=1

	// processed is the index (counting from 1 in the peer's sequence
	// of frames) of the latest peer frame this endpoint has provably
	// processed; see the package comment.
	processed int
	// settingsIdx are the indexes of SETTINGS frames sent by this
	// endpoint and not yet acknowledged, oldest first.
	settingsIdx []int
	nSettings   int // non-ACK SETTINGS frames sent
	// noRFC7540 is the SETTINGS_NO_RFC7540_PRIORITIES value sent, or -1.
	noRFC7540 int64
	// htsChanges are the SETTINGS_HEADER_TABLE_SIZE limits this
	// endpoint (as encoder) has acknowledged since its last field block.
	htsChanges []uint32
	// respondedMax is the highest peer-initiated stream ID on which
	// this endpoint has sent HEADERS or DATA.
	respondedMax uint32
}

type headerBlock struct {
	typ       byte // ftHeaders or ftPushPromise
	stream    uint32
	promised  uint32
	endStream bool
	frag      []byte
}

// vstream is the validator's view of a stream.
type vstream struct {
	id     uint32
	opener int
	d      [2]streamDir // indexed by sending side
	method string
	closed bool // both sides ended, or reset
	tunnel bool // a CONNECT request that got a 2xx response
}

// streamDir is the state of one direction of a stream.
type streamDir struct {
	headers       bool  // sent the request, or final response, header section
	ended         bool  // sent END_STREAM
	reset         bool  // sent RST_STREAM
	endIdx        int   // index of the frame that carried END_STREAM, or 0
	rstIdx        int   // index of the RST_STREAM frame, or 0
	reducedOver   bool  // exceeded the window only after the receiver reduced SETTINGS_INITIAL_WINDOW_SIZE
	window        int64 // send window available for DATA
	strictWindow  int64 // window under the strictest interpretation; see endpoint.strictInitWindow
	flowFault     bool  // sent anything that might justify a stream FLOW_CONTROL_ERROR
	contentLength int64 // declared content-length, or -1
	dataLen       int64 // DATA payload bytes sent (excluding padding)
	status        int   // for responses: final status
	noContent     bool  // message is defined to have no content
}

// connValidator validates one HTTP/2 connection.
type connValidator struct {
	mu       sync.Mutex
	id       int
	ep       [2]*endpoint
	streams  map[uint32]*vstream
	findings []Finding
	trace    []string
	isH2     bool // client sent the HTTP/2 preface
	http1    bool // client sent an HTTP/1 request instead
	done     bool

	frameCounts [2]map[string]int
	active      [2]int // number of open streams, by opener
	maxActive   [2]int
}

const maxTraceLines = 3000

func newConnValidator(id int, clientName, serverName string) *connValidator {
	v := &connValidator{
		id:      id,
		streams: map[uint32]*vstream{},
	}
	for side, name := range []string{clientName, serverName} {
		e := &endpoint{
			side:             side,
			name:             name,
			settings:         newSettingsState(),
			effInitWindow:    65535,
			sendWindow:       65535,
			strictInitWindow: 65535,
			encMaxSize:       4096,
			lastGoAway:       -1,
			pings:            map[[8]byte]int{},
			noRFC7540:        -1,
			touched:          map[uint32]bool{},
		}
		e.dec = hpack.NewDecoder(4096, func(f hpack.HeaderField) {
			e.decFields = append(e.decFields, f)
		})
		e.dec.SetMaxStringLength(16 << 20)
		v.ep[side] = e
		v.frameCounts[side] = map[string]int{}
	}
	return v
}

func (v *connValidator) peer(side int) *endpoint { return v.ep[1-side] }

func (v *connValidator) report(sev Severity, sentBy int, stream uint32, rule, format string, args ...any) {
	f := Finding{
		Sev:    sev,
		Sender: v.ep[sentBy].name,
		SentBy: sentBy,
		Rule:   rule,
		Stream: stream,
		Msg:    fmt.Sprintf(format, args...),
	}
	v.findings = append(v.findings, f)
	v.tracef("  ^^^ %v", f)
}

func (v *connValidator) tracef(format string, args ...any) {
	if len(v.trace) < maxTraceLines {
		v.trace = append(v.trace, fmt.Sprintf(format, args...))
	} else if len(v.trace) == maxTraceLines {
		v.trace = append(v.trace, "... (trace truncated)")
	}
}

// Feed processes bytes sent by side.
func (v *connValidator) Feed(side int, p []byte) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e := v.ep[side]
	if v.ep[0].broken || v.ep[1].broken || v.done {
		// Once either direction is lost, state shared between the
		// directions (streams, settings) can't be trusted.
		return
	}
	e.buf = append(e.buf, p...)
	if side == sideClient && !e.preface {
		n := min(len(e.buf), len(clientPreface))
		if string(e.buf[:n]) != clientPreface[:n] {
			e.broken = true
			v.ep[sideServer].broken = true
			if bytes.Contains(e.buf, []byte(" HTTP/1.")) {
				v.http1 = true
				v.tracef("C: HTTP/1.x request; not validating")
				return
			}
			v.report(SevViolation, side, 0, "9113-3.4-1 9113-3.2-2 9113-3.4-d1", "invalid connection preface %q", e.buf[:n])
			return
		}
		if n < len(clientPreface) {
			return
		}
		e.buf = e.buf[len(clientPreface):]
		e.preface = true
		v.isH2 = true
		v.tracef("C: connection preface")
		// Process anything the server sent while we were waiting.
		if s := v.ep[sideServer]; len(s.buf) > 0 {
			v.parseFrames(s)
		}
	}
	if !v.isH2 {
		// Wait for the client preface before parsing server frames.
		return
	}
	v.parseFrames(e)
}

func (v *connValidator) parseFrames(e *endpoint) {
	for len(e.buf) >= 9 && !v.ep[0].broken && !v.ep[1].broken {
		length := int(e.buf[0])<<16 | int(e.buf[1])<<8 | int(e.buf[2])
		if len(e.buf) < 9+length {
			return
		}
		typ := e.buf[3]
		flags := e.buf[4]
		stream := binary.BigEndian.Uint32(e.buf[5:9]) & (1<<31 - 1)
		payload := e.buf[9 : 9+length]
		if e.buf[5]&0x80 != 0 {
			v.report(SevViolation, e.side, stream, "9113-4.1-4", "%s frame with the reserved stream identifier bit set", frameTypeName(typ))
		}
		v.frame(e, typ, flags, stream, payload)
		e.buf = e.buf[9+length:]
	}
	if len(e.buf) == 0 {
		e.buf = nil
	}
}

func sideLetter(side int) string {
	if side == sideClient {
		return "C"
	}
	return "S"
}

func flagsString(typ, flags byte) string {
	var parts []string
	switch typ {
	case ftData:
		if flags&flagEndStream != 0 {
			parts = append(parts, "END_STREAM")
		}
		if flags&flagPadded != 0 {
			parts = append(parts, "PADDED")
		}
	case ftHeaders:
		if flags&flagEndStream != 0 {
			parts = append(parts, "END_STREAM")
		}
		if flags&flagEndHeaders != 0 {
			parts = append(parts, "END_HEADERS")
		}
		if flags&flagPadded != 0 {
			parts = append(parts, "PADDED")
		}
		if flags&flagPriority != 0 {
			parts = append(parts, "PRIORITY")
		}
	case ftSettings, ftPing:
		if flags&flagAck != 0 {
			parts = append(parts, "ACK")
		}
	case ftContinuation:
		if flags&flagEndHeaders != 0 {
			parts = append(parts, "END_HEADERS")
		}
	case ftPushPromise:
		if flags&flagEndHeaders != 0 {
			parts = append(parts, "END_HEADERS")
		}
		if flags&flagPadded != 0 {
			parts = append(parts, "PADDED")
		}
	default:
		if flags != 0 {
			parts = append(parts, fmt.Sprintf("0x%x", flags))
		}
	}
	return strings.Join(parts, "|")
}

func (v *connValidator) frame(e *endpoint, typ, flags byte, stream uint32, payload []byte) {
	side := e.side
	p := v.peer(side)
	e.nframes++
	v.frameCounts[side][frameTypeName(typ)]++
	if stream != 0 {
		e.touched[stream] = true
	}
	v.tracef("%s: %s stream=%d len=%d flags=%s", sideLetter(side), frameTypeName(typ), stream, len(payload), flagsString(typ, flags))

	if allowed, ok := definedFlags[typ]; ok && flags&^allowed != 0 {
		v.report(SevViolation, side, stream, "9113-4.1-3", "%s frame with undefined flags 0x%x set", frameTypeName(typ), flags&^allowed)
	}
	v.checkClosedStream(e, typ, stream)

	if e.nframes == 1 && typ != ftSettings {
		v.report(SevViolation, side, stream, "9113-3.4-1 9113-3.4-2 9113-3.2-2 9113-3.3-2 9113-6.5-1", "first frame was %s, not SETTINGS", frameTypeName(typ))
	}

	// §4.2: frame size limits are imposed by the receiver's
	// SETTINGS_MAX_FRAME_SIZE.
	if maxSize := p.settings.permissiveMax(setMaxFrameSize); uint32(len(payload)) > maxSize {
		v.report(SevViolation, side, stream, "9113-4.1-1 9113-6.5.3-1", "%s frame of %d bytes exceeds peer's SETTINGS_MAX_FRAME_SIZE of %d",
			frameTypeName(typ), len(payload), maxSize)
	}

	// §6.10: a header block must be contiguous.
	if e.hb != nil && (typ != ftContinuation || stream != e.hb.stream) {
		v.report(SevViolation, side, stream, "9113-4.3-1 9113-6.2-3 9113-6.10-1 9113-8.1-2", "%s frame on stream %d interrupted a field block on stream %d",
			frameTypeName(typ), stream, e.hb.stream)
		e.hb = nil
	}

	switch typ {
	case ftData:
		v.dataFrame(e, flags, stream, payload)
	case ftHeaders:
		v.headersFrame(e, flags, stream, payload)
	case ftPriority:
		if stream == 0 {
			v.report(SevViolation, side, 0, "RFC 9113 §6.3", "PRIORITY frame on stream 0")
		}
		if len(payload) != 5 {
			v.report(SevViolation, side, stream, "RFC 9113 §6.3", "PRIORITY frame with length %d, want 5", len(payload))
		} else if dep := binary.BigEndian.Uint32(payload) & (1<<31 - 1); dep == stream {
			v.report(SevWarning, side, stream, "RFC 7540 §5.3.1", "stream depends on itself")
		}
	case ftRSTStream:
		v.rstFrame(e, stream, payload)
	case ftSettings:
		v.settingsFrame(e, flags, stream, payload)
	case ftPushPromise:
		v.pushPromiseFrame(e, flags, stream, payload)
	case ftPing:
		if stream != 0 {
			v.report(SevViolation, side, stream, "9113-6.7-1", "PING frame on stream %d", stream)
		}
		if len(payload) != 8 {
			v.report(SevViolation, side, stream, "9113-6.7-1", "PING frame with length %d, want 8", len(payload))
			return
		}
		var data [8]byte
		copy(data[:], payload)
		if flags&flagAck != 0 {
			idx, ok := p.pings[data]
			if !ok {
				v.report(SevWarning, side, 0, "RFC 9113 §6.7", "PING ACK %x doesn't match any outstanding PING", data)
			}
			delete(p.pings, data)
			e.processed = max(e.processed, idx)
		} else {
			e.pings[data] = e.nframes
		}
	case ftGoAway:
		if stream != 0 {
			v.report(SevViolation, side, stream, "RFC 9113 §6.8", "GOAWAY frame on stream %d", stream)
		}
		if len(payload) < 8 {
			v.report(SevViolation, side, stream, "RFC 9113 §6.8", "GOAWAY frame with length %d, want at least 8", len(payload))
			return
		}
		last := int64(binary.BigEndian.Uint32(payload) & (1<<31 - 1))
		code := binary.BigEndian.Uint32(payload[4:])
		// The last-stream-ID identifies "the last peer-initiated
		// stream" (RFC 9113 §6.8), so it must be 0 or have the
		// parity of streams initiated by the receiver.
		if peerOdd := p.side == sideClient; last != 0 && (last%2 == 1) != peerOdd {
			v.report(SevViolation, side, 0, "9113-6.8-d1", "GOAWAY last-stream-ID %d is not a stream the peer could initiate", last)
		}
		if e.lastGoAway >= 0 && last > e.lastGoAway {
			v.report(SevViolation, side, 0, "9113-6.8-6", "GOAWAY last-stream-ID increased from %d to %d", e.lastGoAway, last)
		}
		e.lastGoAway = last
		v.report(SevInfo, side, 0, "", "GOAWAY last-stream-ID=%d code=%s debug=%q", last, errCodeName(code), payload[8:])
		if e.goAwayIdx == 0 {
			e.goAwayIdx = e.nframes
		}
		if code != 0 {
			if e.goAwayCode != 0 && e.goAwayCode != code {
				v.report(SevWarning, side, 0, "9113-5.4-2", "GOAWAY with error code %s after one with %s", errCodeName(code), errCodeName(e.goAwayCode))
			}
			if e.goAwayCode == 0 {
				e.goAwayCode = code
			}
		}
		if last < int64(e.respondedMax) {
			v.report(SevViolation, side, 0, "9113-8.7-3 9113-8.7-2", "GOAWAY last-stream-ID %d is below stream %d, on which the sender already sent a response", last, e.respondedMax)
		}
		if code == errFlowControl && !p.flowFault && !v.anyStreamFlowFault(p.side) {
			v.report(SevViolation, side, 0, "9113-6.9.3-1 9113-6.9.1-1", "GOAWAY FLOW_CONTROL_ERROR, but the peer never exceeded a flow control window or overflowed one")
		}
	case ftWindowUpdate:
		v.windowUpdateFrame(e, stream, payload)
	case ftContinuation:
		if e.hb == nil {
			v.report(SevViolation, side, stream, "9113-6.10-5", "CONTINUATION frame without preceding HEADERS or PUSH_PROMISE")
			return
		}
		if stream == 0 {
			v.report(SevViolation, side, 0, "9113-6.10-3", "CONTINUATION frame on stream 0")
		}
		e.hb.frag = append(e.hb.frag, payload...)
		if flags&flagEndHeaders != 0 {
			hb := e.hb
			e.hb = nil
			v.fieldBlock(e, hb)
		}
	case ftPriorityUpdate:
		if side == sideServer {
			v.report(SevViolation, side, stream, "9218-7.1-10", "server sent PRIORITY_UPDATE")
		}
		if stream != 0 {
			v.report(SevViolation, side, stream, "9218-7.1-1", "PRIORITY_UPDATE frame on stream %d", stream)
		}
		if len(payload) < 4 {
			v.report(SevViolation, side, stream, "RFC 9218 §7.1", "PRIORITY_UPDATE frame with length %d", len(payload))
		} else {
			v.tracef("   prioritized stream %d: %q", binary.BigEndian.Uint32(payload)&(1<<31-1), payload[4:])
		}
	default:
		// Unknown and extension frame types are permitted (§5.5).
	}
}

func (v *connValidator) getStream(id uint32) *vstream { return v.streams[id] }

// definedFlags are the flags defined for each frame type.
var definedFlags = map[byte]byte{
	ftData:         flagEndStream | flagPadded,
	ftHeaders:      flagEndStream | flagEndHeaders | flagPadded | flagPriority,
	ftPriority:     0,
	ftRSTStream:    0,
	ftSettings:     flagAck,
	ftPushPromise:  flagEndHeaders | flagPadded,
	ftPing:         flagAck,
	ftGoAway:       0,
	ftWindowUpdate: 0,
	ftContinuation: flagEndHeaders,
}

// checkClosedStream checks that e doesn't send frames (other than
// PRIORITY) on a stream that is closed from its point of view:
// because it reset the stream, or because it has provably processed
// the peer's RST_STREAM or both END_STREAMs (see the package comment).
func (v *connValidator) checkClosedStream(e *endpoint, typ byte, stream uint32) {
	switch typ {
	case ftData, ftHeaders, ftRSTStream, ftWindowUpdate:
	default:
		return
	}
	st := v.streams[stream]
	if st == nil {
		return
	}
	d, pd := &st.d[e.side], &st.d[1-e.side]
	name := frameTypeName(typ)
	switch {
	case pd.rstIdx > 0 && pd.rstIdx <= e.processed:
		if typ == ftRSTStream {
			v.report(SevViolation, e.side, stream, "9113-5.4.2-4", "RST_STREAM in response to the peer's RST_STREAM, after processing it")
		} else {
			v.report(SevViolation, e.side, stream, "9113-6.4-1 9113-5.1-9", "%s frame after processing the peer's RST_STREAM", name)
		}
	case d.reset:
		if typ == ftRSTStream {
			v.report(SevWarning, e.side, stream, "9113-5.4.2-2 9113-5.4-1", "more than one RST_STREAM for the stream")
		} else if typ == ftWindowUpdate {
			// DATA and HEADERS are checked elsewhere.
			v.report(SevViolation, e.side, stream, "9113-5.1-9", "WINDOW_UPDATE frame after sending RST_STREAM")
		}
	case d.ended && pd.endIdx > 0 && pd.endIdx <= e.processed:
		v.report(SevViolation, e.side, stream, "9113-5.1-9", "%s frame on a closed stream (after sending END_STREAM and processing the peer's)", name)
	}
}

const (
	errFlowControl   = 0x3
	errRefusedStream = 0x7
)

func (v *connValidator) anyStreamFlowFault(side int) bool {
	for _, st := range v.streams {
		if st.d[side].flowFault {
			return true
		}
	}
	return false
}

// isIdle reports whether the stream id is in the "idle" state: it
// hasn't been opened, and its ID is above any opened by its initiator.
func (v *connValidator) isIdle(id uint32) bool {
	if v.streams[id] != nil {
		return false
	}
	opener := sideClient
	if id%2 == 0 {
		opener = sideServer
	}
	return id > v.ep[opener].maxOpened
}

// activeStreams returns the number of streams opened by side that
// count toward the peer's SETTINGS_MAX_CONCURRENT_STREAMS.
func (v *connValidator) activeStreams(side int) int {
	return v.active[side]
}

func (v *connValidator) maybeClose(st *vstream) {
	if st.closed {
		return
	}
	if (st.d[0].ended && st.d[1].ended) || st.d[0].reset || st.d[1].reset {
		st.closed = true
		v.active[st.opener]--
	}
}

// stripPadding returns the payload with padding removed, or ok=false
// if the padding is invalid.
func (v *connValidator) stripPadding(e *endpoint, typ byte, flags byte, stream uint32, payload []byte) ([]byte, bool) {
	if flags&flagPadded == 0 {
		return payload, true
	}
	if len(payload) < 1 {
		v.report(SevViolation, e.side, stream, "RFC 9113 §6.1", "padded %s frame too short for pad length", frameTypeName(typ))
		return nil, false
	}
	pad := int(payload[0])
	if pad >= len(payload) {
		v.report(SevViolation, e.side, stream, "RFC 9113 §6.1", "%s frame pad length %d >= payload length %d", frameTypeName(typ), pad, len(payload))
		return nil, false
	}
	for _, b := range payload[len(payload)-pad:] {
		if b != 0 {
			rule := "9113-6.1-2"
			if typ == ftHeaders {
				rule = "9113-6.2-1"
			}
			v.report(SevViolation, e.side, stream, rule, "%s frame with non-zero padding", frameTypeName(typ))
			break
		}
	}
	return payload[1 : len(payload)-pad], true
}

func (v *connValidator) dataFrame(e *endpoint, flags byte, stream uint32, payload []byte) {
	side := e.side
	if stream == 0 {
		v.report(SevViolation, side, 0, "9113-6.1-4", "DATA frame on stream 0")
		return
	}
	// Flow control counts the entire payload, including padding (§6.9.1).
	flen := int64(len(payload))
	e.sendWindow -= flen
	if e.sendWindow < 0 {
		e.flowFault = true
		v.report(SevViolation, side, stream, "9113-6.9.1-1 9113-5.2.1-2 9113-6.1-d1 9113-6.9.2-d1", "DATA exceeded connection flow control window by %d bytes", -e.sendWindow)
		e.sendWindow = 0
	}
	data, ok := v.stripPadding(e, ftData, flags, stream, payload)
	if !ok {
		return
	}
	st := v.getStream(stream)
	if st == nil {
		if v.isIdle(stream) {
			v.report(SevViolation, side, stream, "9113-5.1-14", "DATA frame on idle stream")
		}
		return
	}
	d := &st.d[side]
	switch {
	case d.reset:
		v.report(SevViolation, side, stream, "9113-5.1-9", "DATA frame after sending RST_STREAM")
		return
	case d.ended:
		v.report(SevViolation, side, stream, "9113-5.1-9", "DATA frame after sending END_STREAM")
		return
	case !d.headers:
		v.report(SevViolation, side, stream, "RFC 9113 §8.1", "DATA frame before the header section")
	}
	d.strictWindow -= flen
	if d.strictWindow < 0 {
		d.flowFault = true
	}
	d.window -= flen
	if d.window < 0 {
		v.report(SevViolation, side, stream, "9113-6.9.1-1 9113-5.2.1-2 9113-6.9.2-2 9113-6.5.3-1", "DATA exceeded stream flow control window by %d bytes", -d.window)
		d.window = 0
	}
	d.dataLen += int64(len(data))
	if st.opener != side {
		e.respondedMax = max(e.respondedMax, stream)
	}
	if d.noContent && len(data) > 0 && d.dataLen == int64(len(data)) {
		what := "a HEAD request"
		if d.status != 0 {
			what = fmt.Sprintf("a %d response", d.status)
		}
		v.report(SevViolation, side, stream, "9113-8.1.1-d2", "DATA payload in response to %s, which has no content", what)
	}
	if flags&flagEndStream != 0 {
		v.endStream(e, st)
	}
}

func (v *connValidator) endStream(e *endpoint, st *vstream) {
	d := &st.d[e.side]
	d.ended = true
	d.endIdx = e.nframes
	if d.contentLength >= 0 && !d.noContent && d.dataLen != d.contentLength {
		v.report(SevViolation, e.side, st.id, "RFC 9113 §8.1.1", "content-length %d doesn't match DATA payload length %d", d.contentLength, d.dataLen)
	}
	v.maybeClose(st)
}

func (v *connValidator) headersFrame(e *endpoint, flags byte, stream uint32, payload []byte) {
	side := e.side
	if stream == 0 {
		v.report(SevViolation, side, 0, "9113-6.2-5", "HEADERS frame on stream 0")
		return
	}
	frag, ok := v.stripPadding(e, ftHeaders, flags, stream, payload)
	if !ok {
		return
	}
	if flags&flagPriority != 0 {
		if len(frag) < 5 {
			v.report(SevViolation, side, stream, "RFC 9113 §6.2", "HEADERS frame too short for priority fields")
			return
		}
		if dep := binary.BigEndian.Uint32(frag) & (1<<31 - 1); dep == stream {
			v.report(SevWarning, side, stream, "RFC 7540 §5.3.1", "stream depends on itself")
		}
		frag = frag[5:]
	}
	hb := &headerBlock{
		typ:       ftHeaders,
		stream:    stream,
		endStream: flags&flagEndStream != 0,
		frag:      append([]byte(nil), frag...),
	}
	if flags&flagEndHeaders != 0 {
		v.fieldBlock(e, hb)
	} else {
		e.hb = hb
	}
}

func (v *connValidator) pushPromiseFrame(e *endpoint, flags byte, stream uint32, payload []byte) {
	side := e.side
	if side == sideClient {
		v.report(SevViolation, side, stream, "9113-8.4.1-5", "client sent PUSH_PROMISE")
		return
	}
	if v.peer(side).settings.permissiveMax(setEnablePush) == 0 {
		v.report(SevViolation, side, stream, "9113-6.6-8 9113-6.5.2-1", "PUSH_PROMISE sent after client disabled push")
	}
	frag, ok := v.stripPadding(e, ftPushPromise, flags, stream, payload)
	if !ok {
		return
	}
	if len(frag) < 4 {
		v.report(SevViolation, side, stream, "RFC 9113 §6.6", "PUSH_PROMISE frame too short")
		return
	}
	hb := &headerBlock{
		typ:      ftPushPromise,
		stream:   stream,
		promised: binary.BigEndian.Uint32(frag) & (1<<31 - 1),
		frag:     append([]byte(nil), frag[4:]...),
	}
	if flags&flagEndHeaders != 0 {
		v.fieldBlock(e, hb)
	} else {
		e.hb = hb
	}
}

func (v *connValidator) rstFrame(e *endpoint, stream uint32, payload []byte) {
	side := e.side
	if stream == 0 {
		v.report(SevViolation, side, 0, "9113-6.4-3", "RST_STREAM frame on stream 0")
		return
	}
	if len(payload) != 4 {
		v.report(SevViolation, side, stream, "RFC 9113 §6.4", "RST_STREAM frame with length %d, want 4", len(payload))
		return
	}
	code := binary.BigEndian.Uint32(payload)
	v.report(SevInfo, side, stream, "", "RST_STREAM code=%s", errCodeName(code))
	st := v.getStream(stream)
	if st == nil {
		// RFC 7540 §5.3.1 required a stream error in response to a
		// PRIORITY frame making an idle stream depend on itself, so
		// allow resets of idle streams that the peer has referenced.
		if v.isIdle(stream) && !v.peer(side).touched[stream] {
			v.report(SevViolation, side, stream, "9113-6.4-5", "RST_STREAM frame on idle stream")
		}
		return
	}
	if code == errFlowControl && !st.d[1-side].flowFault && !v.peer(side).flowFault && !st.d[1-side].reset {
		if st.d[1-side].reducedOver {
			// RFC 9113 §6.9.3 permits this, but clients don't
			// retry such requests.
			v.report(SevWarning, side, stream, "9113-6.9.3-3", "RST_STREAM FLOW_CONTROL_ERROR for data the peer sent within the window in effect before it processed a reduction of SETTINGS_INITIAL_WINDOW_SIZE (permitted, but the request fails)")
		} else {
			v.report(SevViolation, side, stream, "9113-6.9.3-1 9113-6.9.1-1", "RST_STREAM FLOW_CONTROL_ERROR, but the peer never exceeded the stream's flow control window, even if every SETTINGS_INITIAL_WINDOW_SIZE took effect when sent")
		}
	}
	if code == errRefusedStream && (st.d[side].headers || st.d[side].dataLen > 0) && st.opener != side {
		v.report(SevViolation, side, stream, "9113-8.7-3 9113-8.7-2", "RST_STREAM REFUSED_STREAM on a stream the sender already responded on")
	}
	if !st.d[side].reset {
		st.d[side].rstIdx = e.nframes
	}
	st.d[side].reset = true
	v.maybeClose(st)
}

func (v *connValidator) windowUpdateFrame(e *endpoint, stream uint32, payload []byte) {
	side := e.side
	if len(payload) != 4 {
		v.report(SevViolation, side, stream, "RFC 9113 §6.9", "WINDOW_UPDATE frame with length %d, want 4", len(payload))
		return
	}
	incr := int64(binary.BigEndian.Uint32(payload) & (1<<31 - 1))
	if incr == 0 {
		v.report(SevViolation, side, stream, "RFC 9113 §6.9", "WINDOW_UPDATE with an increment of 0")
		if stream == 0 {
			e.flowFault = true
		}
	}
	p := v.peer(side)
	if stream == 0 {
		p.sendWindow += incr
		if p.sendWindow > maxWindow {
			e.flowFault = true
			v.report(SevViolation, side, 0, "9113-6.9.1-3", "WINDOW_UPDATE caused the connection window to exceed 2^31-1 (%d)", p.sendWindow)
		}
		return
	}
	st := v.getStream(stream)
	if st == nil {
		if v.isIdle(stream) {
			v.report(SevViolation, side, stream, "RFC 9113 §5.1", "WINDOW_UPDATE frame on idle stream")
		}
		return
	}
	d := &st.d[p.side]
	d.window += incr
	d.strictWindow += incr
	if incr == 0 || d.strictWindow > maxWindow {
		st.d[side].flowFault = true
	}
	if d.window > maxWindow && !st.closed {
		v.report(SevViolation, side, stream, "9113-6.9.1-3", "WINDOW_UPDATE caused the stream window to exceed 2^31-1 (%d)", d.window)
	}
}

func (v *connValidator) settingsFrame(e *endpoint, flags byte, stream uint32, payload []byte) {
	side := e.side
	p := v.peer(side)
	if stream != 0 {
		v.report(SevViolation, side, stream, "9113-6.5-5", "SETTINGS frame on stream %d", stream)
		return
	}
	if flags&flagAck != 0 {
		if len(payload) != 0 {
			v.report(SevViolation, side, 0, "9113-6.5-3", "SETTINGS ACK with a %d byte payload", len(payload))
		}
		// This endpoint acknowledges the oldest pending SETTINGS
		// sent by its peer.
		ps := p.settings
		if len(ps.pending) == 0 {
			v.report(SevViolation, side, 0, "RFC 9113 §6.5.3", "SETTINGS ACK without an outstanding SETTINGS frame")
			return
		}
		applied := ps.pending[0]
		ps.pending = ps.pending[1:]
		e.processed = max(e.processed, p.settingsIdx[0])
		p.settingsIdx = p.settingsIdx[1:]
		for id, val := range applied {
			ps.acked[id] = val
		}
		if hts, ok := applied[setHeaderTableSize]; ok {
			e.htsChanges = append(e.htsChanges, hts)
		}
		if _, ok := applied[setInitialWindowSize]; ok {
			v.adjustInitWindow(p)
		}
		return
	}
	if len(payload)%6 != 0 {
		v.report(SevViolation, side, 0, "RFC 9113 §6.5", "SETTINGS frame with length %d, not a multiple of 6", len(payload))
		return
	}
	m := map[uint16]uint32{}
	var desc []string
	for b := payload; len(b) >= 6; b = b[6:] {
		id := binary.BigEndian.Uint16(b)
		val := binary.BigEndian.Uint32(b[2:])
		desc = append(desc, fmt.Sprintf("%s=%d", settingName(id), val))
		switch id {
		case setEnablePush:
			if val > 1 {
				v.report(SevViolation, side, 0, "RFC 9113 §6.5.2", "SETTINGS_ENABLE_PUSH=%d", val)
			} else if val == 1 && side == sideServer {
				v.report(SevViolation, side, 0, "9113-6.5.2-4 9113-6.5.2-5", "server sent SETTINGS_ENABLE_PUSH=1")
			}
		case setInitialWindowSize:
			if val > maxWindow {
				v.report(SevViolation, side, 0, "RFC 9113 §6.5.2", "SETTINGS_INITIAL_WINDOW_SIZE=%d exceeds 2^31-1", val)
				e.flowFault = true
			}
		case setMaxFrameSize:
			if val < 16384 || val > 1<<24-1 {
				v.report(SevViolation, side, 0, "9113-6.5.2-10", "SETTINGS_MAX_FRAME_SIZE=%d out of range", val)
			}
		case setEnableConnectProtocol:
			if val > 1 {
				v.report(SevViolation, side, 0, "8441-3-1", "SETTINGS_ENABLE_CONNECT_PROTOCOL=%d", val)
			}
			if val == 0 && e.connectProt {
				v.report(SevViolation, side, 0, "8441-3-3", "SETTINGS_ENABLE_CONNECT_PROTOCOL changed from 1 to 0")
			}
			if val == 1 {
				e.connectProt = true
			}
		case setNoRFC7540Priorities:
			if val > 1 {
				v.report(SevViolation, side, 0, "9218-2.1-1", "SETTINGS_NO_RFC7540_PRIORITIES=%d", val)
			}
			switch {
			case e.noRFC7540 < 0 && e.nSettings > 0:
				v.report(SevViolation, side, 0, "9218-2.1-3", "SETTINGS_NO_RFC7540_PRIORITIES first sent in a later SETTINGS frame")
			case e.noRFC7540 >= 0 && int64(val) != e.noRFC7540:
				v.report(SevViolation, side, 0, "9218-2.1-4", "SETTINGS_NO_RFC7540_PRIORITIES changed from %d to %d", e.noRFC7540, val)
			}
			e.noRFC7540 = int64(val)
		}
		m[id] = val
	}
	v.tracef("   %s", strings.Join(desc, " "))
	e.settings.pending = append(e.settings.pending, m)
	e.settingsIdx = append(e.settingsIdx, e.nframes)
	e.nSettings++
	if w, ok := m[setInitialWindowSize]; ok {
		v.adjustInitWindow(e)
		delta := int64(w) - e.strictInitWindow
		e.strictInitWindow = int64(w)
		for _, st := range v.streams {
			st.d[p.side].strictWindow += delta
			if st.d[p.side].strictWindow < 0 {
				st.d[p.side].reducedOver = true
			}
			if st.d[p.side].strictWindow > maxWindow {
				e.flowFault = true // the receiver of these settings may complain
			}
		}
	}
}

// adjustInitWindow recomputes the permissive initial window that e
// has granted its peer, applying the delta to all streams.
func (v *connValidator) adjustInitWindow(e *endpoint) {
	// Increases apply as soon as they're sent; decreases only once
	// acknowledged. The permissive value is therefore the largest of
	// the acknowledged value and any pending values.
	newWin := int64(e.settings.permissiveMax(setInitialWindowSize))
	if newWin == e.effInitWindow {
		return
	}
	delta := newWin - e.effInitWindow
	e.effInitWindow = newWin
	sender := 1 - e.side
	for _, st := range v.streams {
		st.d[sender].window += delta
	}
}

// fieldBlock processes a complete field block.
func (v *connValidator) fieldBlock(e *endpoint, hb *headerBlock) {
	side := e.side
	p := v.peer(side)
	fields, ok := v.decodeFields(e, hb)
	if !ok {
		return
	}
	v.tracef("   %s", fieldsTrace(fields))

	if hb.typ == ftPushPromise {
		st := v.getStream(hb.stream)
		if st == nil || st.opener != sideClient {
			v.report(SevViolation, side, hb.stream, "RFC 9113 §8.4", "PUSH_PROMISE on a stream not opened by the client")
		}
		if hb.promised%2 != 0 || hb.promised <= e.maxOpened {
			v.report(SevViolation, side, hb.stream, "RFC 9113 §5.1.1", "PUSH_PROMISE promised invalid stream ID %d", hb.promised)
		} else {
			e.maxOpened = hb.promised
			ps := v.newStream(hb.promised, sideServer)
			ps.d[sideClient].ended = true
			ps.method = v.checkRequest(e, hb.promised, fields, true)
		}
		return
	}

	st := v.getStream(hb.stream)
	if st == nil {
		// A new stream.
		if !v.isIdle(hb.stream) {
			v.report(SevViolation, side, hb.stream, "9113-5.1.1-2", "HEADERS on a closed stream")
			return
		}
		wantOdd := side == sideClient
		if (hb.stream%2 == 1) != wantOdd {
			v.report(SevViolation, side, hb.stream, "9113-5.1.1-1", "HEADERS opened a stream with the peer's stream ID parity")
			return
		}
		if side == sideServer {
			v.report(SevViolation, side, hb.stream, "RFC 9113 §8.4", "server opened stream %d with HEADERS", hb.stream)
			return
		}
		e.maxOpened = hb.stream
		st = v.newStream(hb.stream, side)
		if limit := p.settings.permissiveMax(setMaxConcurrentStreams); uint32(v.activeStreams(side)) > limit {
			v.report(SevViolation, side, hb.stream, "9113-5.1.2-1 9113-6.5.3-1", "opened stream exceeding peer's SETTINGS_MAX_CONCURRENT_STREAMS of %d", limit)
		}
		v.maxActive[side] = max(v.maxActive[side], v.activeStreams(side))
		d := &st.d[side]
		d.headers = true
		st.method = v.checkRequest(e, hb.stream, fields, false)
		if p.goAwayIdx > 0 && p.goAwayIdx <= e.processed {
			v.report(SevViolation, side, hb.stream, "9113-6.8-1", "opened stream after processing the peer's GOAWAY")
		} else if p.lastGoAway >= 0 && int64(hb.stream) > p.lastGoAway {
			v.report(SevInfo, side, hb.stream, "RFC 9113 §6.8", "opened stream after the peer sent GOAWAY (may be a benign race)")
		}
		v.setContentLength(e, st, fields)
		if hb.endStream {
			v.endStream(e, st)
		}
		return
	}

	d := &st.d[side]
	switch {
	case d.reset:
		v.report(SevViolation, side, hb.stream, "9113-5.1-9", "HEADERS after sending RST_STREAM")
		return
	case d.ended:
		v.report(SevViolation, side, hb.stream, "9113-5.1-9", "HEADERS after sending END_STREAM")
		return
	}

	if side == sideServer && !d.headers {
		// Response header section, interim or final.
		status := v.checkResponse(e, hb.stream, fields)
		if status >= 100 && status < 200 {
			if hb.endStream {
				v.report(SevViolation, side, hb.stream, "RFC 9113 §8.1", "interim %d response with END_STREAM", status)
			}
			if fieldsHas(fields, "content-length") {
				v.report(SevViolation, side, hb.stream, "RFC 9110 §8.6", "content-length in a %d response", status)
			}
			return
		}
		d.headers = true
		d.status = status
		e.respondedMax = max(e.respondedMax, st.id)
		if st.method == "CONNECT" && status >= 200 && status < 300 {
			st.tunnel = true
		}
		if st.method == "HEAD" || status == 204 || status == 304 {
			d.noContent = true
		}
		if status == 204 && fieldsHas(fields, "content-length") {
			v.report(SevViolation, side, hb.stream, "RFC 9110 §8.6", "content-length in a 204 response")
		}
		v.setContentLength(e, st, fields)
		if hb.endStream {
			v.endStream(e, st)
		}
		return
	}

	// Trailers.
	if st.tunnel {
		v.report(SevViolation, side, hb.stream, "9113-8.5-2", "HEADERS frame on an established CONNECT tunnel")
	}
	if !hb.endStream {
		v.report(SevViolation, side, hb.stream, "RFC 9113 §8.1", "trailer section HEADERS without END_STREAM")
	}
	v.checkFields(e, hb.stream, fields)
	for _, f := range fields {
		if strings.HasPrefix(f.Name, ":") {
			v.report(SevViolation, side, hb.stream, "9113-8.1-3 9113-8.3-3", "pseudo-header %q in trailers", f.Name)
		}
	}
	if hb.endStream {
		v.endStream(e, st)
	}
}

func (v *connValidator) newStream(id uint32, opener int) *vstream {
	st := &vstream{id: id, opener: opener}
	for side := range 2 {
		st.d[side].window = v.peer(side).effInitWindow
		st.d[side].strictWindow = v.peer(side).strictInitWindow
		st.d[side].contentLength = -1
	}
	v.streams[id] = st
	v.active[opener]++
	return st
}

func (v *connValidator) setContentLength(e *endpoint, st *vstream, fields []hpack.HeaderField) {
	var cl []string
	for _, f := range fields {
		if f.Name == "content-length" {
			cl = append(cl, f.Value)
		}
	}
	if len(cl) == 0 {
		return
	}
	n, err := strconv.ParseInt(cl[0], 10, 64)
	if err != nil || n < 0 || strings.TrimLeft(cl[0], "0123456789") != "" {
		v.report(SevViolation, e.side, st.id, "RFC 9110 §8.6", "invalid content-length %q", cl[0])
		return
	}
	for _, c := range cl[1:] {
		if c != cl[0] {
			v.report(SevViolation, e.side, st.id, "RFC 9110 §8.6", "conflicting content-length values %q", cl)
			return
		}
	}
	st.d[e.side].contentLength = n
}

// decodeFields HPACK-decodes a field block, checking RFC 7541 rules.
func (v *connValidator) decodeFields(e *endpoint, hb *headerBlock) ([]hpack.HeaderField, bool) {
	side := e.side
	p := v.peer(side)
	block := hb.frag

	// RFC 7541 §4.2: dynamic table size updates must appear at the
	// start of a block. Once the encoder has acknowledged a smaller
	// SETTINGS_HEADER_TABLE_SIZE than its current table size, its
	// next block must start with an update reducing the table.
	rest := block
	sizeBefore := e.encMaxSize
	var updates []uint32
	for range 2 {
		if len(rest) == 0 || rest[0]&0xe0 != 0x20 {
			break
		}
		val, n, ok := hpackReadInt(rest, 5)
		if !ok {
			break
		}
		rest = rest[n:]
		e.encMaxSize = uint32(val)
		updates = append(updates, uint32(val))
		v.tracef("   (dynamic table size update to %d)", val)
	}
	if len(e.htsChanges) > 1 {
		// RFC 7541 §4.2: if the limit changed more than once since
		// the last field block, the smallest must be signaled if
		// it's below the encoder's current size.
		low := slices.Min(e.htsChanges)
		if low < sizeBefore && !slices.ContainsFunc(updates, func(u uint32) bool { return u <= low }) {
			v.report(SevViolation, side, hb.stream, "7541-4.2-3", "field block doesn't signal the smallest SETTINGS_HEADER_TABLE_SIZE (%d) acknowledged since the last field block (updates: %v)", low, updates)
		}
	}
	e.htsChanges = nil
	if limit := p.settings.ackedVal(setHeaderTableSize); e.encMaxSize > limit {
		// The encoder's table size MUST stay within the limit, so
		// it must signal a reduction. That only matters in practice
		// if the encoder uses the dynamic table.
		sev, what := SevWarning, "doesn't use the dynamic table"
		if hpackUsesDynamicTable(rest) {
			sev, what = SevViolation, "uses the dynamic table"
		}
		v.report(sev, side, hb.stream, "9113-4.3.1-1 7541-4.2-1 7541-4.2-2", "field block %s at size %d after acknowledging SETTINGS_HEADER_TABLE_SIZE=%d, without a dynamic table size update",
			what, e.encMaxSize, limit)
		// Avoid repeating this finding for every block.
		e.encMaxSize = limit
	}

	e.dec.SetAllowedMaxDynamicTableSize(p.settings.permissiveMax(setHeaderTableSize))
	e.decFields = e.decFields[:0]
	if _, err := e.dec.Write(block); err != nil {
		v.report(SevViolation, side, hb.stream, "7541-6.3-1", "HPACK decoding error: %v", err)
		e.broken = true // compression state is now unknown
		return nil, false
	}
	if err := e.dec.Close(); err != nil {
		v.report(SevViolation, side, hb.stream, "7541-6.3-1", "HPACK decoding error: %v", err)
		e.broken = true
		return nil, false
	}
	fields := append([]hpack.HeaderField(nil), e.decFields...)
	var size uint32
	for _, f := range fields {
		size += f.Size()
	}
	if limit := p.settings.permissiveMax(setMaxHeaderListSize); size > limit {
		v.report(SevWarning, side, hb.stream, "RFC 9113 §6.5.2", "field section of size %d exceeds peer's advisory SETTINGS_MAX_HEADER_LIST_SIZE of %d", size, limit)
	}
	return fields, true
}

// hpackUsesDynamicTable reports whether an HPACK field block inserts
// into or references the dynamic table (RFC 7541 §6). It returns true
// if the block can't be parsed.
func hpackUsesDynamicTable(p []byte) bool {
	const staticLen = 61
	skipString := func() bool {
		if len(p) == 0 {
			return false
		}
		n, sz, ok := hpackReadInt(p, 7)
		if !ok || uint64(len(p)-sz) < n {
			return false
		}
		p = p[sz+int(n):]
		return true
	}
	for len(p) > 0 {
		b := p[0]
		var prefix uint
		switch {
		case b&0x80 != 0: // indexed field
			idx, sz, ok := hpackReadInt(p, 7)
			if !ok || idx > staticLen {
				return true
			}
			p = p[sz:]
			continue
		case b&0xc0 == 0x40: // literal with incremental indexing
			return true
		case b&0xe0 == 0x20: // dynamic table size update
			_, sz, ok := hpackReadInt(p, 5)
			if !ok {
				return true
			}
			p = p[sz:]
			continue
		default: // literal without indexing or never indexed
			prefix = 4
		}
		idx, sz, ok := hpackReadInt(p, prefix)
		if !ok || idx > staticLen {
			return true
		}
		p = p[sz:]
		if idx == 0 && !skipString() {
			return true
		}
		if !skipString() {
			return true
		}
	}
	return false
}

// hpackReadInt reads an RFC 7541 §5.1 integer with an n-bit prefix.
func hpackReadInt(p []byte, n uint) (val uint64, size int, ok bool) {
	mask := byte(1<<n - 1)
	val = uint64(p[0] & mask)
	if val < uint64(mask) {
		return val, 1, true
	}
	var m uint
	for i := 1; i < len(p) && i < 10; i++ {
		b := p[i]
		val += uint64(b&0x7f) << m
		if b&0x80 == 0 {
			return val, i + 1, true
		}
		m += 7
	}
	return 0, 0, false
}

func fieldsTrace(fields []hpack.HeaderField) string {
	var sb strings.Builder
	for i, f := range fields {
		if i > 0 {
			sb.WriteString(" ")
		}
		val := f.Value
		if len(val) > 60 {
			val = fmt.Sprintf("%s...(%d bytes)", val[:40], len(val))
		}
		fmt.Fprintf(&sb, "%s=%q", f.Name, val)
	}
	return sb.String()
}

func fieldsHas(fields []hpack.HeaderField, name string) bool {
	for _, f := range fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

// checkFields checks the generic field validity rules of RFC 9113
// §8.2, which apply to all field sections.
func (v *connValidator) checkFields(e *endpoint, stream uint32, fields []hpack.HeaderField) {
	side := e.side
	seenRegular := false
	for _, f := range fields {
		name := f.Name
		if name == "" {
			v.report(SevViolation, side, stream, "9113-8.2.1-3", "empty field name")
			continue
		}
		pseudo := name[0] == ':'
		if pseudo {
			if seenRegular {
				v.report(SevViolation, side, stream, "9113-8.3-5", "pseudo-header %q after regular field", name)
			}
			name = name[1:]
		} else {
			seenRegular = true
		}
		for i := 0; i < len(name); i++ {
			c := name[i]
			if c <= 0x20 || (c >= 'A' && c <= 'Z') || c >= 0x7f || c == ':' {
				v.report(SevViolation, side, stream, "9113-8.2.1-3 9113-8.2.1-4 9113-8.2-1", "invalid character %q in field name %q", c, f.Name)
				break
			}
		}
		if strings.ContainsAny(f.Value, "\x00\r\n") {
			v.report(SevViolation, side, stream, "9113-8.2.1-5", "NUL, CR, or LF in value of field %q", f.Name)
		}
		if val := f.Value; val != "" && (isWS(val[0]) || isWS(val[len(val)-1])) {
			v.report(SevViolation, side, stream, "9113-8.2.1-6", "leading or trailing whitespace in value of field %q: %q", f.Name, val)
		}
		switch f.Name {
		case "connection", "proxy-connection", "keep-alive", "transfer-encoding", "upgrade":
			v.report(SevViolation, side, stream, "9113-8.2.2-1", "connection-specific field %q", f.Name)
		case "te":
			if f.Value != "trailers" {
				v.report(SevViolation, side, stream, "9113-8.2.2-3", "TE field with value %q", f.Value)
			}
		}
	}
}

func isWS(c byte) bool { return c == ' ' || c == '\t' }

// checkRequest checks a request header section and returns its method.
func (v *connValidator) checkRequest(e *endpoint, stream uint32, fields []hpack.HeaderField, pushed bool) string {
	side := e.side
	v.checkFields(e, stream, fields)
	pseudo := map[string]string{}
	var host []string
	for _, f := range fields {
		if f.Name == "host" {
			host = append(host, f.Value)
		}
		if !strings.HasPrefix(f.Name, ":") {
			continue
		}
		switch f.Name {
		case ":method", ":scheme", ":authority", ":path", ":protocol":
		default:
			v.report(SevViolation, side, stream, "9113-8.3-1 9113-8.3-2", "unknown or response pseudo-header %q in request", f.Name)
			continue
		}
		if _, dup := pseudo[f.Name]; dup {
			v.report(SevViolation, side, stream, "9113-8.3-7 9113-8.3.1-12", "duplicate pseudo-header %q", f.Name)
		}
		pseudo[f.Name] = f.Value
	}
	method, hasMethod := pseudo[":method"]
	if !hasMethod || method == "" {
		v.report(SevViolation, side, stream, "9113-8.3.1-12", "request missing :method")
	}
	_, hasProtocol := pseudo[":protocol"]
	if method == "CONNECT" && !hasProtocol {
		if _, ok := pseudo[":authority"]; !ok {
			v.report(SevViolation, side, stream, "RFC 9113 §8.5", "CONNECT request missing :authority")
		}
		if _, ok := pseudo[":scheme"]; ok {
			v.report(SevViolation, side, stream, "9113-8.5-1", "CONNECT request with :scheme")
		}
		if _, ok := pseudo[":path"]; ok {
			v.report(SevViolation, side, stream, "9113-8.5-1", "CONNECT request with :path")
		}
	} else {
		if hasProtocol {
			if method != "CONNECT" {
				v.report(SevViolation, side, stream, "RFC 8441 §4", ":protocol pseudo-header with method %q", method)
			}
			if side == sideClient && !v.peer(side).connectProt {
				v.report(SevViolation, side, stream, "9113-5.5-5", ":protocol sent without peer's SETTINGS_ENABLE_CONNECT_PROTOCOL")
			}
		}
		scheme, hasScheme := pseudo[":scheme"]
		path, hasPath := pseudo[":path"]
		if !hasScheme {
			v.report(SevViolation, side, stream, "9113-8.3.1-12 8441-4-2", "request missing :scheme")
		}
		if !hasPath {
			v.report(SevViolation, side, stream, "9113-8.3.1-12 8441-4-2", "request missing :path")
		} else if (scheme == "http" || scheme == "https") && path != "*" && !strings.HasPrefix(path, "/") {
			v.report(SevViolation, side, stream, "9113-8.3.1-10", "invalid :path %q", path)
		} else if path == "*" && method != "OPTIONS" {
			v.report(SevViolation, side, stream, "RFC 9113 §8.3.1", ":path \"*\" with method %q", method)
		}
	}
	if _, ok := pseudo[":authority"]; !ok && method != "CONNECT" && !pushed {
		// Intermediaries forwarding requests without authority
		// information omit it legitimately.
		v.report(SevInfo, side, stream, "9113-8.3.1-2", "request without :authority (host: %q)", host)
	}
	if auth, ok := pseudo[":authority"]; ok {
		if strings.Contains(auth, "@") {
			v.report(SevViolation, side, stream, "9113-8.3.1-9", ":authority %q includes userinfo", auth)
		}
		for _, h := range host {
			if h != auth {
				v.report(SevViolation, side, stream, "9113-8.3.1-3", "host %q differs from :authority %q", h, auth)
			}
		}
	}
	if pushed && (method != "GET" && method != "HEAD") {
		v.report(SevViolation, side, stream, "RFC 9113 §8.4", "pushed request with unsafe method %q", method)
	}
	return method
}

// checkResponse checks a response header section and returns its status.
func (v *connValidator) checkResponse(e *endpoint, stream uint32, fields []hpack.HeaderField) int {
	side := e.side
	v.checkFields(e, stream, fields)
	var statuses []string
	for _, f := range fields {
		if !strings.HasPrefix(f.Name, ":") {
			continue
		}
		if f.Name != ":status" {
			v.report(SevViolation, side, stream, "9113-8.3-1 9113-8.3-2", "pseudo-header %q in response", f.Name)
			continue
		}
		statuses = append(statuses, f.Value)
	}
	switch len(statuses) {
	case 0:
		v.report(SevViolation, side, stream, "9113-8.3.2-1", "response missing :status")
		return 0
	case 1:
	default:
		v.report(SevViolation, side, stream, "9113-8.3-7", "multiple :status pseudo-headers")
	}
	s := statuses[0]
	code, err := strconv.Atoi(s)
	if err != nil || len(s) != 3 || code < 100 {
		v.report(SevViolation, side, stream, "RFC 9110 §15", "invalid :status %q", s)
		return 0
	}
	if code == 101 {
		v.report(SevViolation, side, stream, "RFC 9113 §8.6", "101 (Switching Protocols) response")
	}
	return code
}

// finish is called when the connection has closed.
func (v *connValidator) finish() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.done {
		return
	}
	v.done = true
	for side, e := range v.ep {
		if e.broken {
			continue
		}
		if len(e.buf) > 0 && v.isH2 {
			v.report(SevWarning, side, 0, "", "connection closed with %d bytes of an incomplete frame", len(e.buf))
		}
		if e.hb != nil {
			v.report(SevWarning, side, e.hb.stream, "", "connection closed during a field block")
		}
	}
}

// Summary returns a one-line description of the connection.
func (v *connValidator) Summary() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var parts []string
	for side, e := range v.ep {
		var counts []string
		for _, name := range sortedKeys(v.frameCounts[side]) {
			counts = append(counts, fmt.Sprintf("%s:%d", name, v.frameCounts[side][name]))
		}
		parts = append(parts, fmt.Sprintf("%s sent %d frames (%s), max %d concurrent streams opened",
			e.name, e.nframes, strings.Join(counts, " "), v.maxActive[side]))
	}
	return fmt.Sprintf("conn %d: %s", v.id, strings.Join(parts, "; "))
}

// Trace returns the frame trace.
func (v *connValidator) Trace() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "--- conn %d (C=%s, S=%s) ---\n", v.id, v.ep[0].name, v.ep[1].name)
	for _, l := range v.trace {
		buf.WriteString(l)
		buf.WriteByte('\n')
	}
	return buf.String()
}

// Findings returns the findings so far.
func (v *connValidator) Findings() []Finding {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]Finding(nil), v.findings...)
}

// Streams returns the number of streams opened on the connection.
func (v *connValidator) Streams() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.streams)
}
