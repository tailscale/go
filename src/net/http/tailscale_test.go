// Copyright 2023 Tailscale. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package http_test

import (
	"io"
	"log"
	"net/http"
	. "net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func init() {
	SetResponseWriteEnforcer(func(r *http.Request) {
		respEnf.mu.Lock()
		fs := slices.Clone(respEnf.fs)
		respEnf.mu.Unlock()

		for _, f := range fs {
			if f != nil {
				f(r)
			}
		}
	})
}

var respEnf = struct {
	mu sync.Mutex
	fs []func(*Request)
}{}

func addResponseWriteEnforcer(f func(*Request)) (cleanup func()) {
	respEnf.mu.Lock()
	defer respEnf.mu.Unlock()
	n := len(respEnf.fs)
	respEnf.fs = append(respEnf.fs, f)

	return func() {
		respEnf.mu.Lock()
		defer respEnf.mu.Unlock()
		respEnf.fs[n] = nil
	}
}

func TestSetResponseWriteEnforcer(t *testing.T) {
	run(t, testSetResponseWriteEnforcer, testNotParallel)
}
func testSetResponseWriteEnforcer(t *testing.T, mode testMode) {
	var found atomic.Bool
	remove := addResponseWriteEnforcer(func(r *Request) {
		t.Logf("addResponseWriteEnforcer: %s %s", r.Method, r.URL)
		if r.URL.Query().Get("t") == t.Name() {
			found.Store(true)
		}
	})
	defer remove()

	errLog := new(strings.Builder)
	defer func() {
		if t.Failed() && errLog.Len() > 0 {
			t.Logf("error logs:\n%s", errLog)
		}
	}()

	cst := newClientServerTest(t, mode, HandlerFunc(func(w ResponseWriter, r *Request) {
		if !r.URL.Query().Has("noop") {
			io.WriteString(w, t.Name())
		}
	}),
		func(ts *httptest.Server) {
			ts.Config.ReadTimeout = 250 * time.Millisecond
			ts.Config.ErrorLog = log.New(errLog, "", 0)
		},
	)
	ts := cst.ts
	client := ts.Client()

	for _, q := range []string{"", "&noop=1"} {
		found.Store(false)

		u := ts.URL + "?t=" + url.QueryEscape(t.Name()) + q
		t.Log("GET", u)
		res, err := client.Get(u)
		if err != nil {
			t.Fatalf("unexpected error making request: %s", err)
		}
		b, err := httputil.DumpResponse(res, true)
		t.Logf("Response: error(%v)%s", err, b)

		if !found.Load() {
			t.Errorf("request %s did not reach SetResponseWriteEnforcer callback", q)
		}
	}
}
