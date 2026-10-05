// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// This file defines the JSON protocol spoken between the runner and
// library-based peer clients and servers (see peers/README.md),
// along with the deterministic body pattern that all peers implement.

// ClientSpec is the JSON document given on stdin to a peer client.
// The client performs Requests against URL and writes a ClientOutput
// to stdout.
type ClientSpec struct {
	// URL is the base URL ("https://127.0.0.1:1234" or
	// "http://127.0.0.1:1234"). Request paths are appended to it.
	URL string `json:"url"`

	// H2C reports whether the client should speak HTTP/2 with
	// prior knowledge over cleartext TCP. It is only set when URL
	// is an "http" URL.
	H2C bool `json:"h2c"`

	// Authority, if non-empty, is the :authority to send instead of
	// the host:port from URL.
	Authority string `json:"authority,omitempty"`

	// Concurrent reports whether all requests should be started at
	// once on a single connection. Otherwise they're done
	// sequentially, reusing one connection.
	Concurrent bool `json:"concurrent"`

	Requests []*Req `json:"requests"`
}

// Req is a single request to be performed by a client.
type Req struct {
	Method string `json:"method"`
	Path   string `json:"path"`

	// Header are extra request header fields to send, in order.
	// Names are lowercase.
	Header [][2]string `json:"header,omitempty"`

	// BodyLen is the length of the request body to send, which
	// is the body pattern (see Pattern). Zero means no body.
	BodyLen int `json:"body_len,omitempty"`

	// NoContentLength, if set, asks the client to not declare a
	// content-length for the request body, streaming it instead.
	NoContentLength bool `json:"no_content_length,omitempty"`

	// Trailer are request trailer fields to send after the body.
	Trailer [][2]string `json:"trailer,omitempty"`

	// ExpectContinue asks the client to send "expect: 100-continue"
	// and wait for the interim response before sending the body.
	ExpectContinue bool `json:"expect_continue,omitempty"`

	// CancelAfter, if positive, asks the client to cancel (reset)
	// the stream after receiving at least this many response body
	// bytes.
	CancelAfter int `json:"cancel_after,omitempty"`

	// Timeout, if non-zero, is a deadline for the whole request,
	// including reading the response body. It's only implemented
	// by the Go client.
	Timeout time.Duration `json:"-"`

	// Expect is what the runner expects. It is not sent to peers.
	Expect *Expect `json:"-"`
}

// ClientOutput is the JSON document a peer client writes to stdout.
type ClientOutput struct {
	Results []*Result `json:"results"`
	// Error is a fatal error that prevented running any requests.
	Error string `json:"error,omitempty"`
}

// Result is the outcome of one Req, as reported by a client.
type Result struct {
	Status int `json:"status"`

	// Proto is the protocol used, if the client knows it ("h2", "http/1.1").
	Proto string `json:"proto,omitempty"`

	// Header and Trailer are the response fields, names lowercased,
	// in the order received when the client can tell.
	Header  [][2]string `json:"header"`
	Trailer [][2]string `json:"trailer,omitempty"`

	// Informational are any 1xx responses received before the final response.
	Informational []*Informational `json:"informational,omitempty"`

	BodyLen    int64  `json:"body_len"`
	BodySHA256 string `json:"body_sha256,omitempty"`
	// Body is the response body, if it is valid UTF-8 and at most
	// 64KiB.
	Body string `json:"body,omitempty"`

	// Error is non-empty if the request failed.
	Error string `json:"error,omitempty"`

	// Canceled reports whether the client canceled the request
	// per Req.CancelAfter.
	Canceled bool `json:"canceled,omitempty"`
}

// Informational is a 1xx interim response.
type Informational struct {
	Status int         `json:"status"`
	Header [][2]string `json:"header"`
}

// Get returns the first value of the named response header, or "".
func (r *Result) Get(name string) string { return getField(r.Header, name) }

// GetTrailer returns the first value of the named response trailer, or "".
func (r *Result) GetTrailer(name string) string { return getField(r.Trailer, name) }

func getField(fields [][2]string, name string) string {
	for _, kv := range fields {
		if strings.EqualFold(kv[0], name) {
			return kv[1]
		}
	}
	return ""
}

// ServerSpec is the JSON document given on stdin to a peer server.
// The server listens on Addr until killed, serving the route set
// documented in peers/README.md.
type ServerSpec struct {
	Addr string `json:"addr"` // "127.0.0.1:port"
	TLS  bool   `json:"tls"`  // whether to serve TLS with ALPN "h2"; else h2c prior knowledge
	Cert string `json:"cert"` // PEM file path
	Key  string `json:"key"`  // PEM file path

	// Settings are HTTP/2 settings the server should advertise,
	// keyed by RFC name (e.g. "SETTINGS_MAX_CONCURRENT_STREAMS").
	// Servers which can't set a given setting ignore it.
	Settings map[string]uint32 `json:"settings,omitempty"`
}

// patternAlphabet is repeated to produce deterministic bodies.
// Its length (63) is deliberately not a divisor of any frame or buffer
// size, so reordered or duplicated chunks change the body hash.
const patternAlphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ\n"

// Pattern returns the n-byte body pattern.
func Pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = patternAlphabet[i%len(patternAlphabet)]
	}
	return b
}

// PatternSHA256 returns the hex SHA-256 of Pattern(n).
func PatternSHA256(n int) string { return sha256Hex(Pattern(n)) }

// headerPattern returns an n-byte header-safe value (no newline).
func headerPattern(n int) string {
	const a = "abcdefghijklmnopqrstuvwxyz0123456789"
	var sb strings.Builder
	for i := range n {
		sb.WriteByte(a[i%len(a)])
	}
	return sb.String()
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// helloBody is the body served at /hello.
const helloBody = "Hello, HTTP/2 interop!\n"

// trailersBody is the body served at /trailers.
const trailersBody = "trailers follow\n"

// InfoBody is the JSON body served at /info.
type InfoBody struct {
	Method    string              `json:"method"`
	Path      string              `json:"path"` // including query
	Authority string              `json:"authority"`
	Proto     string              `json:"proto,omitempty"`
	Header    map[string][]string `json:"header"` // lowercase names; excludes pseudo-headers
}

// UploadBody is the JSON body served at /upload.
type UploadBody struct {
	Len     int64               `json:"len"`
	SHA256  string              `json:"sha256"`
	Trailer map[string][]string `json:"trailer,omitempty"`
	// ContentLength is the request's declared content-length, or -1.
	ContentLength int64 `json:"content_length"`
}

func mustJSON(v any) string {
	j, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(j)
}

func fieldsString(f [][2]string) string {
	var sb strings.Builder
	for i, kv := range f {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "%s: %q", kv[0], kv[1])
	}
	return sb.String()
}
