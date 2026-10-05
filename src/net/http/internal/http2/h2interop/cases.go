// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// A Case is an interop scenario: a set of requests a client makes to
// a server implementing the route set, and the expected results. Each
// Case is run for every applicable pairing of a Go client or server
// with a peer, in each mode.
type Case struct {
	Name string
	Desc string
	// ClientNeeds and ServerNeeds are space-separated features the
	// implementations must have for the case to apply.
	ClientNeeds string
	ServerNeeds string
	// Modes, if non-empty, restricts the modes ("tls", "h2c").
	Modes string
	// Covers are the IDs of the requirements (see requirements.txt)
	// the case covers, for both the Go client and server.
	Covers string

	Concurrent bool
	Requests   func() []*Req

	// Check, if non-nil, performs extra checks after the results are
	// checked against each Req's Expect.
	Check func(tc *testCtx, x *caseRun)
}

// caseRun is the state of one run of a Case.
type caseRun struct {
	client   ClientImpl
	server   ServerImpl
	mode     string
	spec     *ClientSpec
	out      *ClientOutput
	tap      *Tap
	serverGo bool
	clientGo bool
}

// Expect describes the expected Result of a Req.
type Expect struct {
	Status        int
	BodyLen       int64 // -1 to not check
	BodySHA256    string
	Body          string            // checked if the client reports bodies
	Header        map[string]string // required header values ("*" for any)
	Trailer       map[string]string
	Informational []int
	// Error reports that the request is expected to fail.
	Error bool
	// Check performs additional checks on the Result.
	Check func(tc *testCtx, x *caseRun, r *Result)
}

func expectStatus(code int) *Expect { return &Expect{Status: code, BodyLen: -1} }

func expectPattern(n int) *Expect {
	return &Expect{Status: 200, BodyLen: int64(n), BodySHA256: PatternSHA256(n)}
}

func expectBody(s string) *Expect {
	return &Expect{Status: 200, BodyLen: int64(len(s)), BodySHA256: sha256Hex([]byte(s)), Body: s}
}

// expectJSON expects a 200 response with a JSON body, decoded into a
// new T and passed to check.
func expectJSON[T any](check func(tc *testCtx, x *caseRun, v *T)) *Expect {
	return &Expect{Status: 200, BodyLen: -1, Check: func(tc *testCtx, x *caseRun, r *Result) {
		if r.Body == "" {
			tc.Failf("empty JSON response body (body_len %d)", r.BodyLen)
			return
		}
		v := new(T)
		if err := json.Unmarshal([]byte(r.Body), v); err != nil {
			tc.Failf("bad JSON response %q: %v", r.Body, err)
			return
		}
		check(tc, x, v)
	}}
}

func expectUpload(n int, trailer map[string]string) *Expect {
	return expectJSON(func(tc *testCtx, x *caseRun, u *UploadBody) {
		if u.Len != int64(n) || (n > 0 && u.SHA256 != PatternSHA256(n)) {
			tc.Failf("server received %d bytes (sha256 %s); want %d bytes (sha256 %s)", u.Len, u.SHA256, n, PatternSHA256(n))
		}
		for k, v := range trailer {
			if got := u.Trailer[k]; len(got) != 1 || got[0] != v {
				tc.Failf("server received request trailer %s=%q; want %q", k, got, v)
			}
		}
	})
}

func req(method, path string, e *Expect) *Req {
	return &Req{Method: method, Path: path, Expect: e}
}

func get(path string, e *Expect) *Req { return req("GET", path, e) }

func one(r *Req) func() []*Req { return func() []*Req { return []*Req{r} } }

func repeat(n int, f func(i int) *Req) func() []*Req {
	return func() []*Req {
		rs := make([]*Req, n)
		for i := range rs {
			rs[i] = f(i)
		}
		return rs
	}
}

var cases = []*Case{
	{
		Name:        "hello",
		Covers:      "9113-8.3-d1 9113-8.3.1-d2",
		Desc:        "simple GET",
		ServerNeeds: "route:hello",
		Requests:    one(get("/hello", expectBody(helloBody))),
	},
	{
		Name:        "head",
		Covers:      "9113-8.1.1-1",
		Desc:        "HEAD has content-length but no content",
		ServerNeeds: "route:bytes",
		Requests: one(req("HEAD", "/bytes/1000", &Expect{
			Status: 200, BodyLen: 0, Header: map[string]string{"content-length": "1000"},
		})),
	},
	{
		Name:        "get-empty",
		Desc:        "GET with an empty response body and content-length: 0",
		ServerNeeds: "route:bytes",
		Requests:    one(get("/bytes/0", expectPattern(0))),
	},
	{
		Name:        "get-100k",
		Covers:      "9113-4.2-1",
		Desc:        "response larger than the initial flow control window",
		ServerNeeds: "route:bytes",
		Requests:    one(get("/bytes/100000", expectPattern(100000))),
	},
	{
		Name:        "get-10m",
		Desc:        "large response, exercising receive flow control",
		ServerNeeds: "route:bytes",
		Requests:    one(get("/bytes/10000000", expectPattern(10000000))),
	},
	{
		Name:        "stream-1m",
		Desc:        "response streamed in small DATA frames without content-length",
		ServerNeeds: "route:stream",
		Requests:    one(get("/stream/1000000", expectPattern(1000000))),
	},
	{
		Name:        "upload-empty",
		Desc:        "POST with an empty body",
		ClientNeeds: "body",
		ServerNeeds: "route:upload",
		Requests:    one(req("POST", "/upload", expectUpload(0, nil))),
	},
	{
		Name:        "upload-100k",
		Desc:        "request body larger than the initial flow control window",
		ClientNeeds: "body request-body",
		ServerNeeds: "route:upload",
		Requests: one(&Req{Method: "POST", Path: "/upload", BodyLen: 100000,
			Expect: expectUpload(100000, nil)}),
	},
	{
		Name:        "upload-10m",
		Desc:        "large request body, exercising send flow control",
		ClientNeeds: "body request-body",
		ServerNeeds: "route:upload",
		Requests: one(&Req{Method: "PUT", Path: "/upload", BodyLen: 10000000,
			Expect: expectUpload(10000000, nil)}),
	},
	{
		Name:        "upload-stream",
		Desc:        "request body without content-length",
		ClientNeeds: "body stream-request-body",
		ServerNeeds: "route:upload",
		Requests: one(&Req{Method: "POST", Path: "/upload", BodyLen: 1000000, NoContentLength: true,
			Expect: expectUpload(1000000, nil)}),
	},
	{
		Name:        "echo-1m",
		Desc:        "full-duplex echo of a request body",
		ClientNeeds: "request-body",
		ServerNeeds: "route:echo",
		Requests: one(&Req{Method: "POST", Path: "/echo", BodyLen: 1000000,
			Expect: expectPattern(1000000)}),
	},
	{
		Name:        "concurrent-10",
		Covers:      "9113-9.1-1 9113-9.1-2",
		Desc:        "10 concurrent 100KB responses on one connection",
		ClientNeeds: "concurrent",
		ServerNeeds: "route:bytes",
		Concurrent:  true,
		Requests:    repeat(10, func(int) *Req { return get("/bytes/100000", expectPattern(100000)) }),
		Check:       checkOneConn,
	},
	{
		Name:        "concurrent-200",
		Desc:        "200 concurrent requests, more than common SETTINGS_MAX_CONCURRENT_STREAMS",
		ClientNeeds: "concurrent",
		ServerNeeds: "route:bytes",
		Concurrent:  true,
		Requests:    repeat(200, func(i int) *Req { return get(fmt.Sprintf("/bytes/%d", 1000+i), expectPattern(1000+i)) }),
	},
	{
		Name:        "concurrent-upload-20",
		Desc:        "20 concurrent 100KB uploads sharing the connection send window",
		ClientNeeds: "concurrent body request-body",
		ServerNeeds: "route:upload",
		Concurrent:  true,
		Requests: repeat(20, func(i int) *Req {
			n := 100000 + i
			return &Req{Method: "POST", Path: "/upload", BodyLen: n, Expect: expectUpload(n, nil)}
		}),
	},
	{
		Name:        "trailers",
		Desc:        "response trailers",
		ClientNeeds: "trailers-recv",
		ServerNeeds: "route:trailers",
		Requests: one(get("/trailers", &Expect{Status: 200, BodyLen: int64(len(trailersBody)), Body: trailersBody,
			Trailer: map[string]string{"x-trailer-a": "1", "x-trailer-b": "two"}})),
	},
	{
		Name:        "request-trailers",
		Desc:        "request trailers",
		ClientNeeds: "body request-body trailers-send",
		ServerNeeds: "route:upload request-trailers",
		Requests: one(&Req{Method: "POST", Path: "/upload", BodyLen: 1000,
			Trailer: [][2]string{{"x-req-trailer", "hello"}},
			Expect:  expectUpload(1000, map[string]string{"x-req-trailer": "hello"})}),
	},
	{
		Name:        "status-204",
		ServerNeeds: "route:status",
		Requests:    one(get("/status/204", &Expect{Status: 204, BodyLen: 0})),
	},
	{
		Name:        "status-304",
		Covers:      "9113-8.1.1-1",
		ServerNeeds: "route:status",
		Requests:    one(get("/status/304", &Expect{Status: 304, BodyLen: 0})),
	},
	{
		Name:        "status-404",
		ServerNeeds: "route:status",
		Requests:    one(get("/status/404", expectStatus(404))),
	},
	{
		Name:        "big-response-header",
		Desc:        "20KB response header, requiring CONTINUATION frames",
		ServerNeeds: "route:bigheader",
		Requests: one(get("/bigheader/20000", &Expect{Status: 200, BodyLen: 3,
			Header: map[string]string{"x-big": headerPattern(20000)}})),
	},
	{
		Name:        "many-response-headers",
		Covers:      "9113-8.2-1",
		Desc:        "response with 50 extra header fields",
		ServerNeeds: "route:manyheaders",
		Requests: one(get("/manyheaders/50", &Expect{Status: 200, BodyLen: 3,
			Header: map[string]string{"x-h-0": "value-0", "x-h-25": "value-25", "x-h-49": "value-49"}})),
	},
	{
		Name:        "big-request-header",
		Desc:        "20KB request header, requiring CONTINUATION frames",
		ClientNeeds: "body custom-headers",
		ServerNeeds: "route:info",
		Requests: one(&Req{Method: "GET", Path: "/info",
			Header: [][2]string{{"x-big", headerPattern(20000)}},
			Expect: expectInfo(func(tc *testCtx, x *caseRun, info *InfoBody) {
				checkInfoHeader(tc, info, "x-big", headerPattern(20000))
			})}),
	},
	{
		Name:        "many-request-headers",
		Covers:      "9113-8.2-1",
		Desc:        "request with 50 extra header fields",
		ClientNeeds: "body custom-headers",
		ServerNeeds: "route:info",
		Requests: func() []*Req {
			r := &Req{Method: "GET", Path: "/info"}
			for i := range 50 {
				r.Header = append(r.Header, [2]string{fmt.Sprintf("x-h-%d", i), fmt.Sprintf("value-%d", i)})
			}
			r.Expect = expectInfo(func(tc *testCtx, x *caseRun, info *InfoBody) {
				for i := range 50 {
					checkInfoHeader(tc, info, fmt.Sprintf("x-h-%d", i), fmt.Sprintf("value-%d", i))
				}
			})
			return []*Req{r}
		},
	},
	{
		Name:        "info",
		Covers:      "9113-8.3.1-2 9113-8.3-d1 9113-8.3.1-d2",
		Desc:        "request pseudo-headers, query string, and header fields as seen by the server",
		ClientNeeds: "body custom-headers",
		ServerNeeds: "route:info",
		Requests: one(&Req{Method: "GET", Path: "/info?a=1&b=%20x&c=%E2%9C%93",
			Header: [][2]string{{"x-custom", "value"}, {"x-inner-space", "a  b\tc"}},
			Expect: expectInfo(func(tc *testCtx, x *caseRun, info *InfoBody) {
				if info.Method != "GET" {
					tc.Failf("server saw method %q", info.Method)
				}
				if want := "/info?a=1&b=%20x&c=%E2%9C%93"; info.Path != want {
					tc.Failf("server saw path %q; want %q", info.Path, want)
				}
				if want := x.authority(); info.Authority != want {
					tc.Failf("server saw authority %q; want %q", info.Authority, want)
				}
				checkInfoHeader(tc, info, "x-custom", "value")
				checkInfoHeader(tc, info, "x-inner-space", "a  b\tc")
				// RFC 9113 §8.3: "Pseudo-header fields are not HTTP
				// header fields." §8.3.1: "All HTTP/2 requests
				// implicitly have a protocol version of 2.0."
				for k := range info.Header {
					if strings.HasPrefix(k, ":") {
						tc.Failf("server handler saw pseudo-header %q as a request field", k)
					}
				}
				if x.serverGo && info.Proto != "HTTP/2.0" {
					tc.Failf("Go server handler saw protocol %q; want HTTP/2.0", info.Proto)
				}
			})}),
	},
	{
		Name:        "cookie-crumbs",
		Covers:      "9113-8.2.3-1 9113-8.2.3-2 7541-2.1-1 7541-2.1-2",
		Desc:        "cookies split into crumbs (RFC 9113 §8.2.3) are rejoined by the server",
		ClientNeeds: "body custom-headers",
		ServerNeeds: "route:info",
		Requests: one(&Req{Method: "GET", Path: "/info",
			Header: [][2]string{{"cookie", "a=1"}, {"cookie", "b=2; c=3"}},
			Expect: expectInfo(func(tc *testCtx, x *caseRun, info *InfoBody) {
				got := info.Header["cookie"]
				if x.serverGo {
					// RFC 9113 §8.2.3: multiple cookie fields MUST be
					// concatenated with "; " before being passed to a
					// generic HTTP server application.
					if len(got) != 1 || got[0] != "a=1; b=2; c=3" {
						tc.Failf("Go server handler saw cookie %q; want [\"a=1; b=2; c=3\"]", got)
					}
					return
				}
				joined := strings.Join(got, "; ")
				if joined != "a=1; b=2; c=3" {
					tc.Failf("server saw cookie %q; want crumbs equivalent to \"a=1; b=2; c=3\"", got)
				}
			})}),
	},
	{
		Name:        "early-hints",
		Covers:      "9113-8.1-1",
		Desc:        "103 Early Hints interim response before the final response",
		ClientNeeds: "informational",
		ServerNeeds: "route:early-hints",
		Requests: one(get("/early-hints", &Expect{Status: 200, BodyLen: 3, Body: "ok\n", Informational: []int{103},
			Check: func(tc *testCtx, x *caseRun, r *Result) {
				if len(r.Informational) > 0 && getField(r.Informational[0].Header, "link") == "" {
					tc.Failf("103 response missing link header: %v", fieldsString(r.Informational[0].Header))
				}
			}})),
	},
	{
		Name:        "expect-continue",
		Desc:        "request with expect: 100-continue",
		ClientNeeds: "body request-body expect-continue",
		ServerNeeds: "route:upload",
		Requests: one(&Req{Method: "POST", Path: "/upload", BodyLen: 100000, ExpectContinue: true,
			Expect: expectUpload(100000, nil)}),
	},
	{
		Name:        "cancel-then-reuse",
		Covers:      "9113-5.1-11 9113-5.4.2-1 9113-6.4-2",
		Desc:        "client resets a stream mid-response, then reuses the connection",
		ClientNeeds: "cancel",
		ServerNeeds: "route:bytes route:hello",
		Requests: func() []*Req {
			return []*Req{
				{Method: "GET", Path: "/bytes/10000000", CancelAfter: 100000,
					Expect: &Expect{Status: 200, BodyLen: -1, Check: func(tc *testCtx, x *caseRun, r *Result) {
						if !r.Canceled && r.Error == "" {
							tc.Failf("client did not cancel")
						}
					}}},
				get("/hello", expectBody(helloBody)),
			}
		},
		Check: checkOneConn,
	},
	{
		Name:        "server-reset",
		Desc:        "server resets a stream mid-response; the client reports an error and reuses the connection",
		ServerNeeds: "route:rst route:hello",
		Requests: func() []*Req {
			return []*Req{
				get("/rst", &Expect{Error: true, BodyLen: -1}),
				get("/hello", expectBody(helloBody)),
			}
		},
	},
	{
		Name:        "obs-text-header",
		Desc:        "request header value containing non-ASCII (obs-text) bytes",
		ClientNeeds: "body custom-headers",
		ServerNeeds: "route:info",
		Requests: one(&Req{Method: "GET", Path: "/info",
			Header: [][2]string{{"x-utf8", "café"}},
			Expect: expectInfo(func(tc *testCtx, x *caseRun, info *InfoBody) {
				checkInfoHeader(tc, info, "x-utf8", "café")
			})}),
	},
}

func expectInfo(check func(tc *testCtx, x *caseRun, info *InfoBody)) *Expect {
	return expectJSON(check)
}

func checkInfoHeader(tc *testCtx, info *InfoBody, name, want string) {
	got := info.Header[name]
	if len(got) != 1 || got[0] != want {
		if len(want) > 100 {
			lens := make([]int, len(got))
			for i, g := range got {
				lens[i] = len(g)
			}
			tc.Failf("server saw %d %q fields of lengths %v; want one of length %d", len(got), name, lens, len(want))
			return
		}
		tc.Failf("server saw %s=%q; want %q", name, got, want)
	}
}

// authority returns the :authority the client should have sent.
func (x *caseRun) authority() string {
	if x.spec.Authority != "" {
		return x.spec.Authority
	}
	return strings.TrimPrefix(strings.TrimPrefix(x.spec.URL, "https://"), "http://")
}

// checkOneConn checks that a client that multiplexes used a single
// connection.
func checkOneConn(tc *testCtx, x *caseRun) {
	if _, ok := x.client.(*proxyClient); ok {
		// Proxies map their clients' connections to upstream
		// connections in their own ways.
		return
	}
	n := x.tap.NumConns()
	if n <= 1 {
		return
	}
	used := 0
	for _, v := range x.tap.Validators() {
		if v.Streams() > 0 {
			used++
		}
	}
	if limit := x.tap.ServerMaxConcurrentStreams(); x.spec.Concurrent && limit < uint32(len(x.spec.Requests)) {
		tc.Logf("client opened %d connections (%d carried requests); server allows %d concurrent streams", n, used, limit)
		return
	}
	if used > 1 {
		tc.Warnf("client sent requests on %d connections (and opened %d); want 1", used, n)
	} else {
		tc.Logf("client opened %d connections, but only used %d", n, used)
	}
}

func checkResult(tc *testCtx, x *caseRun, i int, rq *Req, r *Result) {
	e := rq.Expect
	what := fmt.Sprintf("request %d (%s %s)", i, rq.Method, rq.Path)
	if len(rq.Path) > 60 {
		what = fmt.Sprintf("request %d (%s %s...)", i, rq.Method, rq.Path[:60])
	}
	if e.Error {
		if r.Error == "" {
			tc.Failf("%s: succeeded with status %d, %d body bytes; want error", what, r.Status, r.BodyLen)
		} else {
			tc.Logf("%s: got expected error: %s", what, r.Error)
		}
		return
	}
	if r.Error != "" {
		tc.Failf("%s: error: %s", what, r.Error)
		return
	}
	if r.Proto != "" && r.Proto != "h2" {
		tc.Failf("%s: used protocol %q, not h2", what, r.Proto)
	}
	if e.Status != 0 && r.Status != e.Status {
		tc.Failf("%s: status %d; want %d", what, r.Status, e.Status)
		return
	}
	// RFC 9113 §8.3: "Pseudo-header fields are not HTTP header fields."
	for _, kv := range append(r.Header, r.Trailer...) {
		if strings.HasPrefix(kv[0], ":") {
			tc.Failf("%s: client reported pseudo-header %q as a response field", what, kv[0])
		}
	}
	if r.Canceled {
		return
	}
	if e.BodyLen >= 0 && r.BodyLen != e.BodyLen {
		tc.Failf("%s: body length %d; want %d", what, r.BodyLen, e.BodyLen)
	} else if e.BodySHA256 != "" && r.BodySHA256 != "" && r.BodySHA256 != e.BodySHA256 {
		tc.Failf("%s: body sha256 %s; want %s", what, r.BodySHA256, e.BodySHA256)
	} else if e.Body != "" && r.Body != "" && r.Body != e.Body {
		tc.Failf("%s: body %q; want %q", what, r.Body, e.Body)
	}
	for k, v := range e.Header {
		got := r.Get(k)
		if got == "" || (v != "*" && got != v) {
			if len(v) > 100 {
				tc.Failf("%s: header %s has length %d; want length %d", what, k, len(got), len(v))
			} else {
				tc.Failf("%s: header %s=%q; want %q", what, k, got, v)
			}
		}
	}
	for k, v := range e.Trailer {
		if got := r.GetTrailer(k); got != v {
			tc.Failf("%s: trailer %s=%q; want %q (trailers: %v; header: %v)", what, k, got, v, fieldsString(r.Trailer), fieldsString(r.Header))
		}
	}
	if len(e.Informational) > 0 {
		var got []int
		for _, inf := range r.Informational {
			got = append(got, inf.Status)
		}
		if fmt.Sprint(got) != fmt.Sprint(e.Informational) {
			tc.Failf("%s: interim responses %v; want %v", what, got, e.Informational)
		}
	}
	if e.Check != nil {
		e.Check(tc, x, r)
	}
}

// interopTest returns the Test running c between client and server.
func interopTest(name string, c *Case, client ClientImpl, server ServerImpl, mode string) *Test {
	peer := ""
	_, cgo := client.(*goClient)
	_, sgo := server.(*goServer)
	switch {
	case !cgo:
		peer = client.Name()
	case !sgo:
		peer = server.Name()
	}
	return &Test{
		Name:   name,
		Desc:   c.Desc,
		Peer:   peer,
		Images: append(client.Images(), server.Images()...),
		Run: func(ctx context.Context, tc *testCtx) {
			x := &caseRun{client: client, server: server, mode: mode, clientGo: cgo, serverGo: sgo}
			runCase(ctx, tc, c, x)
		},
	}
}

// applicable reports whether c can run between client and server in
// mode, or why not.
func applicable(c *Case, client ClientImpl, server ServerImpl, mode string) (bool, string) {
	if c.Modes != "" && !strings.Contains(c.Modes, mode) {
		return false, "mode " + mode
	}
	cf, sf := client.Features(), server.Features()
	if !cf[mode] {
		return false, client.Name() + " client doesn't support " + mode
	}
	if !sf[mode] {
		return false, server.Name() + " server doesn't support " + mode
	}
	for f := range strings.FieldsSeq(c.ClientNeeds) {
		if !cf[f] {
			return false, client.Name() + " client lacks feature " + f
		}
	}
	for f := range strings.FieldsSeq(c.ServerNeeds) {
		if !sf[f] {
			return false, server.Name() + " server lacks " + f
		}
	}
	return true, ""
}

func runCase(ctx context.Context, tc *testCtx, c *Case, x *caseRun) {
	addr, err := x.server.Start(ctx, tc, x.mode)
	if err == errSkip {
		return
	}
	if err != nil {
		tc.Errorf("starting %s server: %v", x.server.Name(), err)
		return
	}
	tap, err := tc.Tap(addr, x.mode == "tls", x.client.Name()+" client", x.server.Name()+" server")
	if err != nil {
		tc.Errorf("starting tap: %v", err)
		return
	}
	x.tap = tap
	scheme := "https"
	if x.mode == "h2c" {
		scheme = "http"
	}
	x.spec = &ClientSpec{
		URL:        scheme + "://" + tap.Addr(),
		H2C:        x.mode == "h2c",
		Concurrent: c.Concurrent,
		Requests:   c.Requests(),
	}
	out, err := x.client.Do(ctx, tc, x.spec)
	if err == errSkip {
		return
	}
	if err != nil {
		tc.Errorf("running %s client: %v", x.client.Name(), err)
		return
	}
	x.out = out
	if out.Error != "" {
		tc.Failf("%s client failed: %s", x.client.Name(), out.Error)
		return
	}
	if len(out.Results) != len(x.spec.Requests) {
		tc.Errorf("%s client returned %d results for %d requests", x.client.Name(), len(out.Results), len(x.spec.Requests))
		return
	}
	nfail := 0
	for i, rq := range x.spec.Requests {
		r := out.Results[i]
		if r == nil {
			tc.Errorf("missing result %d", i)
			continue
		}
		before := len(tc.failures)
		checkResult(tc, x, i, rq, r)
		if len(tc.failures) > before {
			nfail++
			if nfail > 5 {
				tc.Logf("(not checking remaining results)")
				break
			}
		}
	}
	if c.Check != nil {
		c.Check(tc, x)
	}
}

// interopTests returns all interop tests for the given implementations.
func interopTests(clients []ClientImpl, servers []ServerImpl, includeSkips bool) []*Test {
	var tests []*Test
	for _, client := range clients {
		for _, server := range servers {
			_, cgo := client.(*goClient)
			_, sgo := server.(*goServer)
			var prefix string
			switch {
			case cgo && sgo:
				// Go-to-Go, as a baseline, only with matching profiles.
				if client.(*goClient).profile != server.(*goServer).profile {
					continue
				}
				prefix = "gogo" + client.(*goClient).profile.suffix()
			case sgo:
				prefix = "goserver" + server.(*goServer).profile.suffix() + "/" + client.Name()
			case cgo:
				prefix = "goclient" + client.(*goClient).profile.suffix() + "/" + server.Name()
			default:
				continue
			}
			for _, mode := range []string{"tls", "h2c"} {
				for _, c := range cases {
					name := prefix + "/" + mode + "/" + c.Name
					ok, why := applicable(c, client, server, mode)
					if !ok {
						if includeSkips {
							tests = append(tests, skipTest(name, why))
						}
						continue
					}
					tests = append(tests, interopTest(name, c, client, server, mode))
				}
			}
		}
	}
	return tests
}

func skipTest(name, why string) *Test {
	return &Test{Name: name, Run: func(ctx context.Context, tc *testCtx) { tc.Skipf("%s", why) }}
}
