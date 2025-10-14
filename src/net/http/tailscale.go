// Copyright 2023 Tailscale. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http

var roundTripEnforcer func(*Request) error

var responseWriteEnforcer func(*Request)

// SetRoundTripEnforcer sets a program-global resolver enforcer that can cause
// RoundTrip calls to fail based on the request and its context.
//
// f must be non-nil.
//
// SetRoundTripEnforcer can only be called once, and must not be called
// concurrent with any RoundTrip call; it's expected to be registered during
// init.
func SetRoundTripEnforcer(f func(*Request) error) {
	if f == nil {
		panic("nil func")
	}
	if roundTripEnforcer != nil {
		panic("already called")
	}
	roundTripEnforcer = f
}

// SetResponseWriteEnforcer sets a program-global response writing enforcer
// which can panic to interrupt the sending of a response.
//
// f must be non-nil.
//
// SetResponseWriteEnforcer can only be called once, and must not be called
// concurrent with any ResponseWriter.WriteHeader call; it's expected to be
// registered during init.
//
// f only accepts a *Request - and not the ResponseWriter - because the
// HTTP/2 responseWriterState is free to write its own status line and has no
// reference to the responseWriter.
func SetResponseWriteEnforcer(f func(*Request)) {
	if f == nil {
		panic("nil func")
	}
	if responseWriteEnforcer != nil {
		panic("already called")
	}
	responseWriteEnforcer = f
	if registerHTTP2ResponseWriteEnforcer != nil {
		registerHTTP2ResponseWriteEnforcer(f)
	}
}

// registerHTTP2ResponseWriteEnforcer installs the enforcer into the
// net/http/internal/http2 package.
// It is set during init by tailscale_http2.go, unless the nethttpomithttp2
// build tag is set.
var registerHTTP2ResponseWriteEnforcer func(f func(*Request))
