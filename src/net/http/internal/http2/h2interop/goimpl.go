// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ClientImpl is an HTTP/2 client implementation.
type ClientImpl interface {
	// Name is the implementation's short name, e.g. "curl".
	Name() string
	Features() featureSet
	// Images returns the docker images needed by the client.
	Images() []string
	// Do performs the requests in spec.
	Do(ctx context.Context, tc *testCtx, spec *ClientSpec) (*ClientOutput, error)
}

// ServerImpl is an HTTP/2 server implementation.
type ServerImpl interface {
	Name() string
	Features() featureSet
	Images() []string
	// Start returns the address of a server for the mode ("tls" or
	// "h2c"), starting one if necessary. Servers may be shared
	// between tests.
	Start(ctx context.Context, tc *testCtx, mode string) (addr string, err error)
}

type featureSet map[string]bool

// feats returns a featureSet from a space-separated list.
func feats(lists ...string) featureSet {
	fs := featureSet{}
	for _, list := range lists {
		for f := range strings.FieldsSeq(list) {
			if strings.HasPrefix(f, "-") {
				delete(fs, f[1:])
			} else {
				fs[f] = true
			}
		}
	}
	return fs
}

// allClientFeatures is every client feature a Case may need.
const allClientFeatures = "tls h2c concurrent body body-hash request-body stream-request-body " +
	"trailers-recv trailers-send informational cancel expect-continue custom-headers " +
	"report-header-order"

// allRoutes is every route of the route set (see peers/README.md).
const allRoutes = "tls h2c request-trailers route:hello route:bytes route:stream route:echo route:upload route:trailers " +
	"route:status route:bigheader route:manyheaders route:early-hints route:info route:delay route:rst"

// goProfile is a configuration of the Go implementation.
type goProfile struct {
	Name  string // "" for the default
	HTTP2 http.HTTP2Config
}

func (p *goProfile) suffix() string {
	if p.Name == "" {
		return ""
	}
	return ":" + p.Name
}

var goProfiles = []*goProfile{
	{Name: ""},
	{
		// constrained exercises small limits, which peers'
		// flow control and HPACK encoders must respect.
		Name: "constrained",
		HTTP2: http.HTTP2Config{
			MaxConcurrentStreams:          4,
			MaxDecoderHeaderTableSize:     256,
			MaxReadFrameSize:              16384,
			MaxReceiveBufferPerConnection: 65536,
			MaxReceiveBufferPerStream:     16384,
		},
	},
}

// goServer is the Go net/http HTTP/2 server.
type goServer struct {
	profile *goProfile
}

func (s *goServer) Name() string         { return "Go" }
func (s *goServer) Features() featureSet { return feats(allRoutes) }
func (s *goServer) Images() []string     { return nil }

// Start starts a new Go server for the test, so that errors it counts
// can be attributed to the test.
func (s *goServer) Start(ctx context.Context, tc *testCtx, mode string) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	cfg := s.profile.HTTP2
	cfg.CountError = func(token string) { tc.countError("Go server", token) }
	srv := &http.Server{
		Handler:  routeHandler(),
		HTTP2:    &cfg,
		ErrorLog: log.New(&logWriter{tc: tc, prefix: "Go server log: "}, "", 0),
	}
	srv.Protocols = new(http.Protocols)
	srv.Protocols.SetHTTP1(true)
	if mode == "tls" {
		srv.Protocols.SetHTTP2(true)
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{testCert}}
		go srv.ServeTLS(ln, "", "")
	} else {
		srv.Protocols.SetUnencryptedHTTP2(true)
		go srv.Serve(ln)
	}
	tc.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), nil
}

// logWriter sends log output to the test log.
type logWriter struct {
	tc     *testCtx
	prefix string
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.tc.Logf("%s%s", w.prefix, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// goClient is the Go net/http HTTP/2 client (Transport).
type goClient struct {
	profile *goProfile
}

func (c *goClient) Name() string         { return "Go" }
func (c *goClient) Features() featureSet { return feats(allClientFeatures) }
func (c *goClient) Images() []string     { return nil }

func (c *goClient) Do(ctx context.Context, tc *testCtx, spec *ClientSpec) (*ClientOutput, error) {
	cfg := c.profile.HTTP2
	cfg.CountError = func(token string) { tc.countError("Go client", token) }
	protos := new(http.Protocols)
	if spec.H2C {
		protos.SetUnencryptedHTTP2(true)
	} else {
		protos.SetHTTP2(true)
	}
	tr := &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		Protocols:             protos,
		HTTP2:                 &cfg,
		ExpectContinueTimeout: 5 * time.Second,
		DisableCompression:    true,
	}
	defer tr.CloseIdleConnections()
	return doRequests(ctx, tc, tr, spec), nil
}

// doRequests performs spec's requests using rt.
func doRequests(ctx context.Context, tc *testCtx, rt http.RoundTripper, spec *ClientSpec) *ClientOutput {
	out := &ClientOutput{Results: make([]*Result, len(spec.Requests))}
	if spec.Concurrent {
		var wg sync.WaitGroup
		for i, req := range spec.Requests {
			wg.Go(func() {
				out.Results[i] = doRequest(ctx, tc, rt, spec, req)
			})
		}
		wg.Wait()
	} else {
		for i, req := range spec.Requests {
			out.Results[i] = doRequest(ctx, tc, rt, spec, req)
		}
	}
	return out
}

// onlyReader hides other methods (such as Len) of an io.Reader so
// that net/http can't determine the body length.
type onlyReader struct{ io.Reader }

func doRequest(ctx context.Context, tc *testCtx, rt http.RoundTripper, spec *ClientSpec, req *Req) *Result {
	res := &Result{}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if req.Timeout != 0 {
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	var mu sync.Mutex
	trace := &httptrace.ClientTrace{
		Got1xxResponse: func(code int, header textproto.MIMEHeader) error {
			mu.Lock()
			defer mu.Unlock()
			res.Informational = append(res.Informational, &Informational{Status: code, Header: sortedFields(header)})
			return nil
		},
	}
	ctx = httptrace.WithClientTrace(ctx, trace)
	var body io.Reader
	if req.BodyLen > 0 {
		body = bytes.NewReader(Pattern(req.BodyLen))
		if req.NoContentLength {
			body = onlyReader{body}
		}
	}
	hreq, err := http.NewRequestWithContext(ctx, req.Method, spec.URL+req.Path, body)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if req.BodyLen > 0 && req.NoContentLength {
		hreq.ContentLength = -1
	}
	if spec.Authority != "" {
		hreq.Host = spec.Authority
	}
	for _, kv := range req.Header {
		hreq.Header.Add(kv[0], kv[1])
	}
	if req.ExpectContinue {
		hreq.Header.Set("Expect", "100-continue")
	}
	if len(req.Trailer) > 0 {
		hreq.Trailer = http.Header{}
		for _, kv := range req.Trailer {
			hreq.Trailer.Add(kv[0], kv[1])
		}
	}
	resp, err := rt.RoundTrip(hreq)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	res.Proto = protoName(resp)
	res.Header = sortedFields(textproto.MIMEHeader(resp.Header))
	h := sha256.New()
	var small bytes.Buffer
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			if small.Len() <= 64<<10 {
				small.Write(buf[:n])
			}
			res.BodyLen += int64(n)
		}
		if req.CancelAfter > 0 && res.BodyLen >= int64(req.CancelAfter) {
			res.Canceled = true
			cancel()
			return res
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			res.Error = err.Error()
			break
		}
	}
	res.BodySHA256 = hex.EncodeToString(h.Sum(nil))
	if small.Len() <= 64<<10 && utf8.Valid(small.Bytes()) {
		res.Body = small.String()
	}
	if len(resp.Trailer) > 0 {
		res.Trailer = sortedFields(textproto.MIMEHeader(resp.Trailer))
	}
	return res
}

func protoName(resp *http.Response) string {
	switch resp.ProtoMajor {
	case 2:
		return "h2"
	case 1:
		return fmt.Sprintf("http/1.%d", resp.ProtoMinor)
	}
	return resp.Proto
}

// sortedFields converts h to lowercase fields, sorted by name.
func sortedFields(h textproto.MIMEHeader) [][2]string {
	var f [][2]string
	for _, k := range sortedKeys(h) {
		for _, v := range h[k] {
			f = append(f, [2]string{strings.ToLower(k), v})
		}
	}
	return f
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// h1Transport returns an HTTP/1-only transport, used to drive
// reverse proxy peers.
func h1Transport() *http.Transport {
	protos := new(http.Protocols)
	protos.SetHTTP1(true)
	return &http.Transport{
		TLSClientConfig:    &tls.Config{InsecureSkipVerify: true},
		Protocols:          protos,
		DisableCompression: true,
	}
}

var errSkip = errors.New("skip")

// serveForDebugging runs the Go server with the route set until killed.
func serveForDebugging(mode, addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Handler: routeHandler(), Protocols: new(http.Protocols)}
	srv.Protocols.SetHTTP1(true)
	log.Printf("serving %s on %s", mode, ln.Addr())
	if mode == "tls" {
		srv.Protocols.SetHTTP2(true)
		srv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{testCert}}
		log.Fatal(srv.ServeTLS(ln, "", ""))
	}
	srv.Protocols.SetUnencryptedHTTP2(true)
	log.Fatal(srv.Serve(ln))
}
