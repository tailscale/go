// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/internal/http2"
	"slices"
	"strings"
	"sync"
	"time"
)

// This file has TLS-level conformance tests of the Go client and
// server: ALPN negotiation (RFC 9113 §3.2, §3.3), TLS versions and
// SNI (§9.2), and TLS 1.2 cipher suites (§9.2.2, Appendix A). The
// scripted peer is a crypto/tls endpoint configured to offer or
// accept only specific protocols, versions, cipher suites, and
// curves; after the handshake it speaks HTTP/2 with a rawConn, or
// HTTP/1.1. The tests are named conform/tls/client/<name> and
// conform/tls/server/<name>.

// tlsConfCase is a TLS-level conformance test of the Go client or
// server.
type tlsConfCase struct {
	role string // "client" or "server": the Go implementation tested
	name string
	// desc states the requirement being tested.
	desc string
	// covers are the IDs of the requirements (see requirements.txt)
	// the case covers.
	covers string
	run    func(ctx context.Context, tc *testCtx)
}

func init() {
	for _, c := range tlsConfCases {
		extraTests = append(extraTests, &Test{
			Name: "conform/tls/" + c.role + "/" + c.name,
			Desc: c.desc,
			Run: func(ctx context.Context, tc *testCtx) {
				tc.peerFindingsIgnored = true
				tc.Logf("requirement: %s", c.desc)
				c.run(ctx, tc)
			},
		})
	}
}

var tlsConfCases = []*tlsConfCase{
	// Go client tests.
	{
		role:   "client",
		name:   "no-h2c-alpn",
		covers: "9113-3.2-1",
		desc: `RFC 9113 §3.2: "The "h2c" protocol identifier MUST NOT be sent by a client". ` +
			`The Go client's ClientHello for an "https" URL must not offer ALPN "h2c", even with unencrypted HTTP/2 enabled.`,
		run: func(ctx context.Context, tc *testCtx) {
			scfg := &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
			for _, p := range goClientProtocols {
				tc.Logf("--- Go client protocols %s", p.name)
				res, conns := runTLSClient(ctx, tc, scfg, goTLSTransport(tc, nil, p), "127.0.0.1")
				for _, sc := range conns {
					if sc.hello == nil {
						continue
					}
					if slices.Contains(sc.hello.SupportedProtos, "h2c") {
						tc.Failf("Go client (protocols %s) offered ALPN %q over TLS", p.name, sc.hello.SupportedProtos)
					}
				}
				if p.h2 && (res.Error != "" || res.Proto != "h2") {
					tc.Failf("Go client (protocols %s): got proto=%q err=%q; want an h2 response", p.name, res.Proto, res.Error)
				}
			}
		},
	},
	{
		role:   "client",
		name:   "alpn-required",
		covers: "9113-3.3-1",
		desc: `RFC 9113 §3.3: "HTTP/2 connections over TLS MUST use protocol negotiation in TLS [TLS-ALPN]." ` +
			`When the server negotiates no ALPN protocol, or "http/1.1", the Go client must not send the HTTP/2 connection preface.`,
		run: func(ctx context.Context, tc *testCtx) {
			for _, protos := range [][]string{nil, {"http/1.1"}} {
				scfg := &tls.Config{NextProtos: protos}
				for _, p := range goClientProtocols {
					tc.Logf("--- server ALPN %q, Go client protocols %s", protos, p.name)
					res, conns := runTLSClient(ctx, tc, scfg, goTLSTransport(tc, nil, p), "127.0.0.1")
					for _, sc := range conns {
						if sc.spoke == "h2" {
							tc.Failf("Go client (protocols %s) sent the HTTP/2 preface over TLS after the server negotiated ALPN %q",
								p.name, sc.state.NegotiatedProtocol)
						}
					}
					if p.h1 && (res.Error != "" || res.Proto != "http/1.1") {
						tc.Failf("Go client (protocols %s): got proto=%q err=%q; want an HTTP/1.1 response", p.name, res.Proto, res.Error)
					}
				}
			}
		},
	},
	{
		role:   "client",
		name:   "tls-version",
		covers: "9113-9.2-1",
		desc: `RFC 9113 §9.2: "Implementations of HTTP/2 MUST use TLS version 1.2 [TLS12] or higher for HTTP/2 over TLS." ` +
			`Against a TLS 1.0 or 1.1 server that selects ALPN "h2", the Go client must fail the handshake or not use HTTP/2 ` +
			`(INADEQUATE_SECURITY is permitted by §9.2.1), also when its tls.Config permits TLS 1.0 for HTTP/1.1.`,
		run: func(ctx context.Context, tc *testCtx) {
			for _, v := range []uint16{tls.VersionTLS10, tls.VersionTLS11} {
				scfg := &tls.Config{MinVersion: v, MaxVersion: v, NextProtos: []string{"h2", "http/1.1"}}
				for _, ccfg := range []struct {
					name string
					cfg  *tls.Config
				}{
					{"default tls.Config", &tls.Config{InsecureSkipVerify: true}},
					{"tls.Config MinVersion TLS 1.0", &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS10}},
				} {
					tc.Logf("--- server %s, Go client %s", tls.VersionName(v), ccfg.name)
					res, conns := runTLSClient(ctx, tc, scfg, goTLSTransport(tc, ccfg.cfg, protoH1H2), "127.0.0.1")
					tc.Logf("Go client result: proto=%q err=%q", res.Proto, res.Error)
					for _, sc := range conns {
						if sc.hsErr == nil && sc.spoke == "h2" && sc.gotRequest {
							tc.Failf("Go client (%s) sent an HTTP/2 request over %s", ccfg.name, tls.VersionName(sc.state.Version))
						}
					}
				}
			}
		},
	},
	{
		role:   "client",
		name:   "sni",
		covers: "9113-9.2-3 9113-9.2-4",
		desc: `RFC 9113 §9.2: "The TLS implementation MUST support the Server Name Indication (SNI) [TLS-EXT] extension to TLS. ` +
			`If the server is identified by a domain name [DNS-TERMS], clients MUST send the server_name TLS extension unless ` +
			`an alternative mechanism to indicate the target host is used." The Go client fetching https://localhost:port/ must send server_name "localhost".`,
		run: func(ctx context.Context, tc *testCtx) {
			if addrs, err := net.DefaultResolver.LookupHost(ctx, "localhost"); err != nil || !slices.Contains(addrs, "127.0.0.1") {
				tc.Errorf("localhost does not resolve to 127.0.0.1: %v %v", addrs, err)
				return
			}
			scfg := &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
			res, conns := runTLSClient(ctx, tc, scfg, goTLSTransport(tc, nil, protoH1H2), "localhost")
			if len(conns) == 0 || conns[0].hello == nil {
				tc.Failf("Go client did not connect (err=%q)", res.Error)
				return
			}
			for _, sc := range conns {
				if sc.hello != nil && sc.hello.ServerName != "localhost" {
					tc.Failf("Go client sent server_name %q; want %q", sc.hello.ServerName, "localhost")
				}
			}
			if res.Error != "" || res.Proto != "h2" {
				tc.Failf("got proto=%q err=%q; want an h2 response", res.Proto, res.Error)
			}
		},
	},
	{
		role:   "client",
		name:   "prohibited-cipher-suites",
		covers: "9113-9.2.2-1",
		desc: `RFC 9113 §9.2.2: "A deployment of HTTP/2 over TLS 1.2 SHOULD NOT use any of the prohibited cipher suites listed in Appendix A." ` +
			`Against a TLS 1.2 server restricted to one prohibited suite that selects ALPN "h2", the Go client should fail the handshake, ` +
			`not use HTTP/2, or send a connection error of type INADEQUATE_SECURITY.`,
		run: func(ctx context.Context, tc *testCtx) {
			for _, cs := range prohibitedSuites {
				tc.Logf("--- server restricted to %s", tls.CipherSuiteName(cs))
				scfg := &tls.Config{
					MinVersion:   tls.VersionTLS12,
					MaxVersion:   tls.VersionTLS12,
					CipherSuites: []uint16{cs},
					NextProtos:   []string{"h2", "http/1.1"},
				}
				res, conns := runTLSClient(ctx, tc, scfg, goTLSTransport(tc, nil, protoH1H2), "127.0.0.1")
				tc.Logf("Go client result: proto=%q err=%q", res.Proto, res.Error)
				for _, sc := range conns {
					switch {
					case sc.hsErr != nil:
						tc.Logf("handshake failed (acceptable): %v", sc.hsErr)
					case sc.spoke != "h2":
						tc.Logf("Go client did not use HTTP/2 (acceptable)")
					case sc.inadequate():
						tc.Logf("Go client sent INADEQUATE_SECURITY (acceptable)")
					case sc.gotRequest:
						tc.Warnf("Go client used HTTP/2 over TLS 1.2 with prohibited cipher suite %s", tls.CipherSuiteName(sc.state.CipherSuite))
					}
				}
			}
		},
	},
	{
		role:   "client",
		name:   "permitted-cipher-suites",
		covers: "9113-9.2.2-3",
		desc: `RFC 9113 §9.2.2: "Implementations MUST NOT generate this error [INADEQUATE_SECURITY] in reaction to the negotiation ` +
			`of a cipher suite that is not prohibited." Over TLS 1.2 restricted to each of several permitted suites, ` +
			`the Go client's HTTP/2 request must succeed.`,
		run: func(ctx context.Context, tc *testCtx) {
			for _, cs := range permittedSuites {
				tc.Logf("--- server restricted to %s", tls.CipherSuiteName(cs))
				scfg := &tls.Config{
					MinVersion:   tls.VersionTLS12,
					MaxVersion:   tls.VersionTLS12,
					CipherSuites: []uint16{cs},
					NextProtos:   []string{"h2", "http/1.1"},
				}
				checkClientH2OK(ctx, tc, scfg, cs, 0)
			}
		},
	},
	{
		role:   "client",
		name:   "mti-cipher-suite",
		covers: "9113-9.2.2-4",
		desc: `RFC 9113 §9.2.2: "deployments of HTTP/2 that use TLS 1.2 MUST support TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 ` +
			`[TLS-ECDHE] with the P-256 elliptic curve [RFC8422]." The Go client's HTTP/2 request must succeed over TLS 1.2 ` +
			`restricted to that suite and curve.`,
		run: func(ctx context.Context, tc *testCtx) {
			scfg := &tls.Config{
				MinVersion:       tls.VersionTLS12,
				MaxVersion:       tls.VersionTLS12,
				CipherSuites:     []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
				CurvePreferences: []tls.CurveID{tls.CurveP256},
				NextProtos:       []string{"h2", "http/1.1"},
			}
			checkClientH2OK(ctx, tc, scfg, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, tls.CurveP256)
		},
	},

	// Go server tests.
	{
		role:   "server",
		name:   "no-h2c-alpn",
		covers: "9113-3.2-1",
		desc: `RFC 9113 §3.2: "The "h2c" protocol identifier MUST NOT be ... selected by a server". ` +
			`A TLS client offering ALPN "h2c" (alone or with "h2" or "http/1.1") must not have "h2c" selected, even with unencrypted HTTP/2 enabled on the server.`,
		run: func(ctx context.Context, tc *testCtx) {
			for _, p := range goServerProtocols {
				addr, err := startGoTLSServer(ctx, tc, p, nil)
				if err != nil {
					tc.Errorf("%v", err)
					return
				}
				for _, offer := range [][]string{{"h2c"}, {"h2c", "h2"}, {"h2c", "http/1.1"}} {
					tc.Logf("--- Go server protocols %s, client ALPN %q", p.name, offer)
					conn, err := dialTLSPeer(tc, addr, &tls.Config{NextProtos: offer})
					if err != nil {
						tc.Logf("handshake failed (acceptable): %v", err)
						continue
					}
					if np := conn.ConnectionState().NegotiatedProtocol; np == "h2c" {
						tc.Failf("Go server (protocols %s) selected ALPN %q from client offer %q", p.name, np, offer)
					}
					conn.Close()
				}
			}
		},
	},
	{
		role:   "server",
		name:   "alpn-required",
		covers: "9113-3.3-1",
		desc: `RFC 9113 §3.3: "HTTP/2 connections over TLS MUST use protocol negotiation in TLS [TLS-ALPN]." ` +
			`A TLS client that negotiates no ALPN protocol, or "http/1.1", and then sends the HTTP/2 connection preface ` +
			`and a request must not get an HTTP/2 response.`,
		run: func(ctx context.Context, tc *testCtx) {
			for _, p := range goServerProtocols {
				addr, err := startGoTLSServer(ctx, tc, p, nil)
				if err != nil {
					tc.Errorf("%v", err)
					return
				}
				for _, offer := range [][]string{nil, {"http/1.1"}} {
					tc.Logf("--- Go server protocols %s, client ALPN %q", p.name, offer)
					conn, err := dialTLSPeer(tc, addr, &tls.Config{NextProtos: offer})
					if err != nil {
						tc.Logf("handshake failed: %v", err)
						continue
					}
					if np := conn.ConnectionState().NegotiatedProtocol; np == "h2" {
						tc.Errorf("server negotiated %q from client offer %q", np, offer)
						return
					}
					rc := newRawConn(tc, "client", conn)
					if _, err := io.WriteString(conn, clientPreface); err != nil {
						tc.Logf("writing preface: %v", err)
					}
					rc.writeSettings()
					rc.writeFields(1, true, ":method", "GET", ":scheme", "https", ":authority", addr, ":path", "/hello")
					got := readSome(conn, reactionTimeout)
					switch {
					case looksLikeH2Frame(got):
						tc.Failf("Go server (protocols %s) responded with an HTTP/2 frame (% x) without negotiating ALPN h2 (client offered %q)",
							p.name, got[:9], offer)
					case len(got) == 0:
						tc.Logf("server sent nothing (acceptable)")
					default:
						line, _, _ := strings.Cut(string(got), "\n")
						tc.Logf("server responded with %q (acceptable)", truncate(line, 100))
					}
					conn.Close()
				}
			}
		},
	},
	{
		role:   "server",
		name:   "tls-version",
		covers: "9113-9.2-1",
		desc: `RFC 9113 §9.2: "Implementations of HTTP/2 MUST use TLS version 1.2 [TLS12] or higher for HTTP/2 over TLS." ` +
			`A TLS 1.0 or 1.1 client offering ALPN "h2" must fail the handshake or not be served HTTP/2 (INADEQUATE_SECURITY ` +
			`is permitted by §9.2.1), also when the server's tls.Config permits TLS 1.0 for HTTP/1.1.`,
		run: func(ctx context.Context, tc *testCtx) {
			for _, scfg := range []struct {
				name string
				cfg  *tls.Config
			}{
				{"default tls.Config", nil},
				{"tls.Config MinVersion TLS 1.0", &tls.Config{MinVersion: tls.VersionTLS10}},
			} {
				addr, err := startGoTLSServer(ctx, tc, protoH1H2, scfg.cfg)
				if err != nil {
					tc.Errorf("%v", err)
					return
				}
				for _, v := range []uint16{tls.VersionTLS10, tls.VersionTLS11} {
					tc.Logf("--- Go server %s, client %s", scfg.name, tls.VersionName(v))
					conn, err := dialTLSPeer(tc, addr, &tls.Config{MinVersion: v, MaxVersion: v, NextProtos: []string{"h2"}})
					if err != nil {
						tc.Logf("handshake failed (acceptable): %v", err)
						continue
					}
					st := conn.ConnectionState()
					if st.NegotiatedProtocol != "h2" {
						tc.Logf("server did not select h2 (acceptable)")
						conn.Close()
						continue
					}
					o := h2Get(tc, conn, addr)
					switch {
					case o.resp != nil && o.resp.Fields != nil:
						tc.Failf("Go server (%s) served an HTTP/2 request over %s (:status %s)",
							scfg.name, tls.VersionName(st.Version), o.resp.Status())
					case o.inadequate():
						tc.Logf("server sent INADEQUATE_SECURITY (acceptable)")
					default:
						tc.Logf("server did not serve the request (acceptable)")
					}
					conn.Close()
				}
			}
		},
	},
	{
		role:   "server",
		name:   "prohibited-cipher-suites",
		covers: "9113-9.2.2-1",
		desc: `RFC 9113 §9.2.2: "A deployment of HTTP/2 over TLS 1.2 SHOULD NOT use any of the prohibited cipher suites listed in Appendix A." ` +
			`A TLS 1.2 client offering only one prohibited suite and ALPN "h2" should fail the handshake, not have h2 selected, ` +
			`or get a connection error of type INADEQUATE_SECURITY.`,
		run: func(ctx context.Context, tc *testCtx) {
			addr, err := (&goServer{profile: goProfiles[0]}).Start(ctx, tc, "tls")
			if err != nil {
				tc.Errorf("%v", err)
				return
			}
			for _, cs := range prohibitedSuites {
				tc.Logf("--- client restricted to %s", tls.CipherSuiteName(cs))
				conn, err := dialTLSPeer(tc, addr, &tls.Config{
					MinVersion:   tls.VersionTLS12,
					MaxVersion:   tls.VersionTLS12,
					CipherSuites: []uint16{cs},
					NextProtos:   []string{"h2"},
				})
				if err != nil {
					tc.Logf("handshake failed (acceptable): %v", err)
					continue
				}
				if conn.ConnectionState().NegotiatedProtocol != "h2" {
					tc.Logf("server did not select h2 (acceptable)")
					conn.Close()
					continue
				}
				o := h2Get(tc, conn, addr)
				switch {
				case o.inadequate():
					tc.Logf("server sent INADEQUATE_SECURITY (acceptable)")
				case o.resp != nil && o.resp.Fields != nil:
					tc.Warnf("Go server served an HTTP/2 request over TLS 1.2 with prohibited cipher suite %s",
						tls.CipherSuiteName(conn.ConnectionState().CipherSuite))
				default:
					tc.Logf("server did not serve the request (acceptable)")
				}
				conn.Close()
			}
		},
	},
	{
		role:   "server",
		name:   "permitted-cipher-suites",
		covers: "9113-9.2.2-3",
		desc: `RFC 9113 §9.2.2: "Implementations MUST NOT generate this error [INADEQUATE_SECURITY] in reaction to the negotiation ` +
			`of a cipher suite that is not prohibited." Over TLS 1.2 restricted to each of several permitted suites, ` +
			`an HTTP/2 request to the Go server must succeed.`,
		run: func(ctx context.Context, tc *testCtx) {
			addr, err := (&goServer{profile: goProfiles[0]}).Start(ctx, tc, "tls")
			if err != nil {
				tc.Errorf("%v", err)
				return
			}
			for _, cs := range permittedSuites {
				tc.Logf("--- client restricted to %s", tls.CipherSuiteName(cs))
				checkServerH2OK(tc, addr, &tls.Config{
					MinVersion:   tls.VersionTLS12,
					MaxVersion:   tls.VersionTLS12,
					CipherSuites: []uint16{cs},
					NextProtos:   []string{"h2"},
				}, cs, 0)
			}
		},
	},
	{
		role:   "server",
		name:   "mti-cipher-suite",
		covers: "9113-9.2.2-4",
		desc: `RFC 9113 §9.2.2: "deployments of HTTP/2 that use TLS 1.2 MUST support TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256 ` +
			`[TLS-ECDHE] with the P-256 elliptic curve [RFC8422]." An HTTP/2 request to the Go server must succeed over ` +
			`TLS 1.2 restricted to that suite and curve.`,
		run: func(ctx context.Context, tc *testCtx) {
			addr, err := (&goServer{profile: goProfiles[0]}).Start(ctx, tc, "tls")
			if err != nil {
				tc.Errorf("%v", err)
				return
			}
			checkServerH2OK(tc, addr, &tls.Config{
				MinVersion:       tls.VersionTLS12,
				MaxVersion:       tls.VersionTLS12,
				CipherSuites:     []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
				CurvePreferences: []tls.CurveID{tls.CurveP256},
				NextProtos:       []string{"h2"},
			}, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, tls.CurveP256)
		},
	},
}

// prohibitedSuites are TLS 1.2 cipher suites listed in RFC 9113
// Appendix A that crypto/tls implements. Some are among crypto/tls's
// defaults (the ECDHE CBC suites); the others are not, and are
// expected to fail the handshake.
var prohibitedSuites = []uint16{
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
	tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256,
	tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
	tls.TLS_RSA_WITH_AES_128_CBC_SHA,
}

// permittedSuites are TLS 1.2 cipher suites not listed in RFC 9113
// Appendix A, for the test certificate's RSA key.
var permittedSuites = []uint16{
	tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
	tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
	tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
}

// tlsProtocols is a set of protocols enabled on the Go client or
// server.
type tlsProtocols struct {
	name        string
	h1, h2, h2c bool
}

func (p *tlsProtocols) protocols() *http.Protocols {
	protos := new(http.Protocols)
	protos.SetHTTP1(p.h1)
	protos.SetHTTP2(p.h2)
	protos.SetUnencryptedHTTP2(p.h2c)
	return protos
}

var (
	protoH1H2    = &tlsProtocols{name: "HTTP1+HTTP2", h1: true, h2: true}
	protoH2      = &tlsProtocols{name: "HTTP2", h2: true}
	protoH1H2H2C = &tlsProtocols{name: "HTTP1+HTTP2+UnencryptedHTTP2", h1: true, h2: true, h2c: true}
	protoH2H2C   = &tlsProtocols{name: "HTTP2+UnencryptedHTTP2", h2: true, h2c: true}
)

var (
	goClientProtocols = []*tlsProtocols{protoH1H2, protoH2, protoH1H2H2C, protoH2H2C}
	goServerProtocols = []*tlsProtocols{protoH1H2, protoH2, protoH1H2H2C}
)

// goTLSTransport returns a Go Transport with the default profile,
// using tlsCfg (or a default that skips verification) and protocols p.
func goTLSTransport(tc *testCtx, tlsCfg *tls.Config, p *tlsProtocols) *http.Transport {
	if tlsCfg == nil {
		tlsCfg = &tls.Config{InsecureSkipVerify: true}
	}
	cfg := goProfiles[0].HTTP2
	cfg.CountError = func(token string) { tc.countError("Go client", token) }
	return &http.Transport{
		TLSClientConfig:    tlsCfg,
		Protocols:          p.protocols(),
		HTTP2:              &cfg,
		DisableCompression: true,
	}
}

// startGoTLSServer starts a Go server serving the route set over TLS
// with protocols p and tlsCfg (to which the test certificate is
// added), as goServer.Start does with its defaults.
func startGoTLSServer(ctx context.Context, tc *testCtx, p *tlsProtocols, tlsCfg *tls.Config) (string, error) {
	if p == protoH1H2 && tlsCfg == nil {
		return (&goServer{profile: goProfiles[0]}).Start(ctx, tc, "tls")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	}
	tlsCfg = tlsCfg.Clone()
	tlsCfg.Certificates = []tls.Certificate{testCert}
	cfg := goProfiles[0].HTTP2
	cfg.CountError = func(token string) { tc.countError("Go server", token) }
	srv := &http.Server{
		Handler:   routeHandler(),
		HTTP2:     &cfg,
		Protocols: p.protocols(),
		TLSConfig: tlsCfg,
		ErrorLog:  log.New(&logWriter{tc: tc, prefix: "Go server log: "}, "", 0),
	}
	go srv.ServeTLS(ln, "", "")
	tc.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), nil
}

// tlsServerConn records what happened on one connection from the Go
// client to the scripted TLS server.
type tlsServerConn struct {
	hello *tls.ClientHelloInfo
	state tls.ConnectionState
	hsErr error
	// spoke is what the client spoke after the handshake: "h2" (it
	// sent the HTTP/2 connection preface), "http/1.1", or "".
	spoke      string
	rc         *rawConn
	gotRequest bool // the client sent an HTTP/2 request
	goAway     *rawFrame
}

// inadequate reports whether the client sent GOAWAY with
// INADEQUATE_SECURITY.
func (sc *tlsServerConn) inadequate() bool {
	if sc.goAway != nil && sc.goAway.ErrCode() == http2.ErrCodeInadequateSecurity {
		return true
	}
	if sc.rc == nil {
		return false
	}
	return len(sc.rc.seen(func(f *rawFrame) bool {
		return f.Type == ftGoAway && f.ErrCode() == http2.ErrCodeInadequateSecurity
	})) > 0
}

// runTLSClient makes a GET /hello request with tr to a scripted TLS
// server configured by scfg (to which the test certificate is
// added), using host as the URL's host. The server answers HTTP/2
// and HTTP/1.1 requests with a 200 response. It returns the client's
// result and the connections the server accepted.
func runTLSClient(ctx context.Context, tc *testCtx, scfg *tls.Config, tr *http.Transport, host string) (*Result, []*tlsServerConn) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tc.Errorf("%v", err)
		return &Result{Error: err.Error()}, nil
	}
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns []*tlsServerConn
		open  []net.Conn
	)
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			sc := &tlsServerConn{}
			mu.Lock()
			conns = append(conns, sc)
			open = append(open, c)
			mu.Unlock()
			wg.Go(func() { serveTLSClientConn(tc, c, scfg, sc) })
		}
	})
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	spec := &ClientSpec{URL: "https://" + net.JoinHostPort(host, port)}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	res := doRequest(rctx, tc, tr, spec, &Req{Method: "GET", Path: "/hello"})
	cancel()
	tr.CloseIdleConnections()
	ln.Close()
	mu.Lock()
	for _, c := range open {
		c.Close()
	}
	mu.Unlock()
	wg.Wait()
	tc.Logf("Go client: status=%d proto=%q err=%q", res.Status, res.Proto, res.Error)
	return res, conns
}

// serveTLSClientConn is the scripted TLS server's side of one
// connection from the Go client.
func serveTLSClientConn(tc *testCtx, c net.Conn, scfg *tls.Config, sc *tlsServerConn) {
	cfg := scfg.Clone()
	cfg.Certificates = []tls.Certificate{testCert}
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		sc.hello = hello
		var vers, suites []string
		for _, v := range hello.SupportedVersions {
			vers = append(vers, tls.VersionName(v))
		}
		for _, cs := range hello.CipherSuites {
			suites = append(suites, tls.CipherSuiteName(cs))
		}
		tc.Logf("ClientHello: server_name=%q alpn=%q versions=%v cipher_suites=%v", hello.ServerName, hello.SupportedProtos, vers, suites)
		return nil, nil
	}
	tconn := tls.Server(c, cfg)
	tconn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tconn.Handshake(); err != nil {
		sc.hsErr = err
		tc.Logf("scripted server: TLS handshake failed: %v", err)
		return
	}
	sc.state = tconn.ConnectionState()
	tc.Logf("scripted server: negotiated %s, %s, curve %v, ALPN %q", tls.VersionName(sc.state.Version),
		tls.CipherSuiteName(sc.state.CipherSuite), sc.state.CurveID, sc.state.NegotiatedProtocol)
	prefix := make([]byte, len(clientPreface))
	n, err := io.ReadFull(tconn, prefix)
	if n == 0 {
		tc.Logf("scripted server: client sent nothing: %v", err)
		return
	}
	tconn.SetDeadline(time.Time{})
	if string(prefix[:n]) == clientPreface {
		sc.spoke = "h2"
		tc.Logf("scripted server: client sent the HTTP/2 connection preface")
		rc := newRawConn(tc, "server", tconn)
		sc.rc = rc
		rc.startReading()
		rc.writeSettings()
		f, _ := rc.waitFor(reactionTimeout, func(f *rawFrame) bool {
			return f.Type == ftGoAway || f.Type == ftSettings && f.Flags&flagAck == 0
		})
		if f == nil || f.Type == ftGoAway {
			sc.goAway = f
			return
		}
		rc.writeSettingsAck()
		f, _ = rc.waitFor(reactionTimeout, func(f *rawFrame) bool {
			return f.Type == ftGoAway || f.Fields != nil && f.Stream == 1
		})
		if f == nil || f.Type == ftGoAway {
			sc.goAway = f
			return
		}
		sc.gotRequest = true
		rc.writeFields(1, false, ":status", "200", "content-type", "text/plain")
		rc.writeData(1, true, []byte("ok"))
		return
	}
	req, err := http.ReadRequest(bufio.NewReader(io.MultiReader(bytes.NewReader(prefix[:n]), tconn)))
	if err != nil {
		tc.Logf("scripted server: reading HTTP/1 request: %v (got %q)", err, prefix[:n])
		return
	}
	sc.spoke = "http/1.1"
	tc.Logf("scripted server: got HTTP/1 request %s %s %s", req.Method, req.RequestURI, req.Proto)
	io.WriteString(tconn, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok")
}

// checkClientH2OK checks that the Go client completes an HTTP/2
// request to the scripted TLS server configured by scfg, using
// cipher suite cs and, if non-zero, curve.
func checkClientH2OK(ctx context.Context, tc *testCtx, scfg *tls.Config, cs uint16, curve tls.CurveID) {
	res, conns := runTLSClient(ctx, tc, scfg, goTLSTransport(tc, nil, protoH1H2), "127.0.0.1")
	for _, sc := range conns {
		switch {
		case sc.hsErr != nil:
			tc.Failf("TLS handshake with %s failed: %v", tls.CipherSuiteName(cs), sc.hsErr)
			continue
		case sc.state.CipherSuite != cs || curve != 0 && sc.state.CurveID != curve:
			tc.Errorf("negotiated %s curve %v; want %s curve %v", tls.CipherSuiteName(sc.state.CipherSuite), sc.state.CurveID,
				tls.CipherSuiteName(cs), curve)
		case sc.spoke != "h2":
			tc.Failf("Go client did not use HTTP/2 with %s (spoke %q)", tls.CipherSuiteName(cs), sc.spoke)
		}
		if sc.inadequate() {
			tc.Failf("Go client sent INADEQUATE_SECURITY for permitted cipher suite %s", tls.CipherSuiteName(cs))
		}
	}
	if res.Error != "" || res.Status != 200 || res.Proto != "h2" {
		tc.Failf("with %s: got status=%d proto=%q err=%q; want an h2 200 response", tls.CipherSuiteName(cs), res.Status, res.Proto, res.Error)
	}
}

// dialTLSPeer connects to the Go server at addr as a scripted TLS
// client configured by cfg, and completes the handshake.
func dialTLSPeer(tc *testCtx, addr string, cfg *tls.Config) (*tls.Conn, error) {
	cfg = cfg.Clone()
	cfg.InsecureSkipVerify = true
	c, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		return nil, err
	}
	tc.Cleanup(func() { c.Close() })
	conn := tls.Client(c, cfg)
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := conn.Handshake(); err != nil {
		c.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	st := conn.ConnectionState()
	tc.Logf("scripted client: negotiated %s, %s, curve %v, ALPN %q", tls.VersionName(st.Version),
		tls.CipherSuiteName(st.CipherSuite), st.CurveID, st.NegotiatedProtocol)
	return conn, nil
}

// h2Outcome is the result of an HTTP/2 request by the scripted client.
type h2Outcome struct {
	rc       *rawConn
	settings bool      // the server sent SETTINGS
	resp     *rawFrame // response HEADERS, RST_STREAM, or GOAWAY
	closed   bool
}

// inadequate reports whether the server sent GOAWAY with
// INADEQUATE_SECURITY.
func (o *h2Outcome) inadequate() bool {
	if o.resp != nil && o.resp.Type == ftGoAway && o.resp.ErrCode() == http2.ErrCodeInadequateSecurity {
		return true
	}
	return len(o.rc.seen(func(f *rawFrame) bool {
		return f.Type == ftGoAway && f.ErrCode() == http2.ErrCodeInadequateSecurity
	})) > 0
}

// h2Get sends the HTTP/2 connection preface and GET /hello on conn,
// and waits for the server's response, stream error, or connection
// error.
func h2Get(tc *testCtx, conn *tls.Conn, authority string) *h2Outcome {
	rc := newRawConn(tc, "client", conn)
	o := &h2Outcome{rc: rc}
	if _, err := io.WriteString(conn, clientPreface); err != nil {
		tc.Logf("writing preface: %v", err)
	}
	rc.writeSettings()
	rc.startReading()
	f, closed := rc.waitFor(reactionTimeout, func(f *rawFrame) bool {
		return f.Type == ftGoAway || f.Type == ftSettings && f.Flags&flagAck == 0
	})
	if f == nil || f.Type == ftGoAway {
		o.resp, o.closed = f, closed
		return o
	}
	o.settings = true
	rc.writeSettingsAck()
	rc.writeFields(1, true, ":method", "GET", ":scheme", "https", ":authority", authority, ":path", "/hello")
	o.resp, o.closed = rc.waitFor(reactionTimeout, func(f *rawFrame) bool {
		switch {
		case f.Type == ftGoAway:
			return f.ErrCode() != http2.ErrCodeNo
		case f.Stream != 1:
			return false
		case f.Type == ftRSTStream:
			return true
		}
		return f.Fields != nil && f.Status() != "" && !strings.HasPrefix(f.Status(), "1")
	})
	return o
}

// checkServerH2OK checks that an HTTP/2 request to the Go server at
// addr succeeds with a scripted TLS client configured by cfg, using
// cipher suite cs and, if non-zero, curve.
func checkServerH2OK(tc *testCtx, addr string, cfg *tls.Config, cs uint16, curve tls.CurveID) {
	conn, err := dialTLSPeer(tc, addr, cfg)
	if err != nil {
		tc.Failf("TLS handshake with %s failed: %v", tls.CipherSuiteName(cs), err)
		return
	}
	defer conn.Close()
	st := conn.ConnectionState()
	switch {
	case st.CipherSuite != cs || curve != 0 && st.CurveID != curve:
		tc.Errorf("negotiated %s curve %v; want %s curve %v", tls.CipherSuiteName(st.CipherSuite), st.CurveID,
			tls.CipherSuiteName(cs), curve)
		return
	case st.NegotiatedProtocol != "h2":
		tc.Failf("Go server selected ALPN %q with %s; want h2", st.NegotiatedProtocol, tls.CipherSuiteName(cs))
		return
	}
	o := h2Get(tc, conn, addr)
	if o.inadequate() {
		tc.Failf("Go server sent INADEQUATE_SECURITY for permitted cipher suite %s", tls.CipherSuiteName(cs))
		return
	}
	switch {
	case o.resp == nil:
		tc.Failf("with %s: no response (closed=%v)", tls.CipherSuiteName(cs), o.closed)
	case o.resp.Fields == nil:
		tc.Failf("with %s: got %v; want a 200 response", tls.CipherSuiteName(cs), o.resp)
	case o.resp.Status() != "200":
		tc.Failf("with %s: got :status %s; want 200", tls.CipherSuiteName(cs), o.resp.Status())
	}
}

// readSome reads from c until it has at least a frame header's worth
// of bytes, the connection is closed, or timeout elapses.
func readSome(c net.Conn, timeout time.Duration) []byte {
	c.SetReadDeadline(time.Now().Add(timeout))
	defer c.SetReadDeadline(time.Time{})
	var got []byte
	buf := make([]byte, 4096)
	for len(got) < 64 {
		n, err := c.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break // EOF, timeout, or reset
		}
	}
	return got
}

// looksLikeH2Frame reports whether b begins with an HTTP/2 frame
// header that could start a server connection preface: a SETTINGS
// (or GOAWAY) frame on stream 0.
func looksLikeH2Frame(b []byte) bool {
	if len(b) < 9 || bytes.HasPrefix(b, []byte("HTTP/")) {
		return false
	}
	length := int(b[0])<<16 | int(b[1])<<8 | int(b[2])
	stream := binary.BigEndian.Uint32(b[5:9]) & (1<<31 - 1)
	switch b[3] {
	case ftSettings:
		return stream == 0 && length%6 == 0
	case ftGoAway:
		return stream == 0 && length >= 8
	}
	return false
}
