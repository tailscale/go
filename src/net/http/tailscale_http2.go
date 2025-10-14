// Copyright 2026 Tailscale. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !nethttpomithttp2

package http

import (
	"net/http/internal/http2"
)

func init() {
	registerHTTP2ResponseWriteEnforcer = func(f func(*Request)) {
		http2.ResponseWriteEnforcer = func(sr *http2.ServerRequest) {
			f(requestFromHTTP2ServerRequest(sr))
		}
	}
}

// requestFromHTTP2ServerRequest converts an http2.ServerRequest to a Request,
// mirroring the conversion done by http2Handler.ServeHTTP in http2.go.
func requestFromHTTP2ServerRequest(sr *http2.ServerRequest) *Request {
	return &Request{
		ctx:           sr.Context,
		Proto:         "HTTP/2.0",
		ProtoMajor:    2,
		ProtoMinor:    0,
		Method:        sr.Method,
		URL:           sr.URL,
		Header:        Header(sr.Header),
		RequestURI:    sr.RequestURI,
		Trailer:       Header(sr.Trailer),
		Body:          sr.Body,
		Host:          sr.Host,
		ContentLength: sr.ContentLength,
		RemoteAddr:    sr.RemoteAddr,
		TLS:           sr.TLS,
		MultipartForm: sr.MultipartForm,
	}
}
