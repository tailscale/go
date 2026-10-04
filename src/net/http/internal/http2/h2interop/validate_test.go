// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"encoding/binary"
	"strings"
	"testing"
)

func rawFrameBytes(typ, flags byte, stream uint32, payload []byte) []byte {
	b := []byte{byte(len(payload) >> 16), byte(len(payload) >> 8), byte(len(payload)), typ, flags}
	b = binary.BigEndian.AppendUint32(b, stream)
	return append(b, payload...)
}

func TestValidateGoAwayLastStreamID(t *testing.T) {
	tests := []struct {
		sender int
		last   uint32
		bad    bool
	}{
		{sideClient, 0, false},
		{sideClient, 2, false},
		{sideClient, 3, true}, // a client's own stream
		{sideServer, 0, false},
		{sideServer, 1, false},
		{sideServer, 1<<31 - 1, false},
		{sideServer, 2, true}, // a server's own stream
	}
	for _, tt := range tests {
		v := newConnValidator(0, "client", "server")
		v.Feed(sideClient, []byte(clientPreface))
		v.Feed(sideClient, rawFrameBytes(ftSettings, 0, 0, nil))
		v.Feed(sideServer, rawFrameBytes(ftSettings, 0, 0, nil))
		p := binary.BigEndian.AppendUint32(nil, tt.last)
		p = binary.BigEndian.AppendUint32(p, 0)
		v.Feed(tt.sender, rawFrameBytes(ftGoAway, 0, 0, p))
		bad := false
		for _, f := range v.Findings() {
			if f.Sev == SevViolation && strings.Contains(f.Msg, "last-stream-ID") {
				bad = true
			}
		}
		if bad != tt.bad {
			t.Errorf("sender %d, last-stream-ID %d: violation = %v; want %v", tt.sender, tt.last, bad, tt.bad)
		}
	}
}
