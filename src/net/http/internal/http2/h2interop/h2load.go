// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"context"
	"regexp"
	"strconv"
	"time"
)

// h2load (from nghttp2) is a load generator. These tests check that
// the Go server serves every request correctly under concurrent load,
// with the tap validating the frames.

type h2loadScenario struct {
	name    string
	args    []string // h2load arguments, before the URL
	path    string
	bodyLen int // request body length (POST), or 0
}

var h2loadScenarios = []h2loadScenario{
	{name: "small-gets", args: []string{"-n", "20000", "-c", "8", "-m", "100"}, path: "/bytes/1000"},
	{name: "large-gets", args: []string{"-n", "200", "-c", "4", "-m", "10"}, path: "/bytes/1000000"},
	{name: "uploads", args: []string{"-n", "2000", "-c", "4", "-m", "20"}, path: "/upload", bodyLen: 100000},
	{name: "small-window", args: []string{"-n", "2000", "-c", "4", "-m", "50", "-w", "12", "-W", "16"}, path: "/bytes/100000"},
}

func init() {
	for _, p := range goProfiles {
		for _, mode := range []string{"tls", "h2c"} {
			for _, sc := range h2loadScenarios {
				extraTests = append(extraTests, &Test{
					Name:      "goserver" + p.suffix() + "/h2load/" + mode + "/" + sc.name,
					Desc:      "h2load " + sc.name,
					Peer:      "h2load",
					Images:    []string{"cli"},
					Exclusive: true,
					Timeout:   3 * time.Minute,
					Run: func(ctx context.Context, tc *testCtx) {
						runH2load(ctx, tc, p, mode, sc)
					},
				})
			}
		}
	}
}

var (
	h2loadRequestsRE = regexp.MustCompile(`requests: (\d+) total, (\d+) started, (\d+) done, (\d+) succeeded, (\d+) failed, (\d+) errored, (\d+) timeout`)
	h2loadStatusRE   = regexp.MustCompile(`status codes: (\d+) 2xx, (\d+) 3xx, (\d+) 4xx, (\d+) 5xx`)
)

func runH2load(ctx context.Context, tc *testCtx, p *goProfile, mode string, sc h2loadScenario) {
	srv := &goServer{profile: p}
	addr, err := srv.Start(ctx, tc, mode)
	if err != nil {
		tc.Errorf("%v", err)
		return
	}
	tap, err := tc.Tap(addr, mode == "tls", "h2load client", "Go server")
	if err != nil {
		tc.Errorf("%v", err)
		return
	}
	scheme := "https"
	if mode == "h2c" {
		scheme = "http"
	}
	args := append([]string{"h2load"}, sc.args...)
	if sc.bodyLen > 0 {
		f, err := tc.env.Docker.BodyFile(sc.bodyLen)
		if err != nil {
			tc.Errorf("%v", err)
			return
		}
		args = append(args, "-d", f)
	}
	args = append(args, scheme+"://"+tap.Addr()+sc.path)
	stdout, _, exit, err := runCollect(ctx, tc, "cli", nil, args)
	if err != nil {
		tc.Errorf("running h2load: %v", err)
		return
	}
	tc.Logf("h2load (exit status %d):\n%s", exit, stdout)
	m := h2loadRequestsRE.FindStringSubmatch(stdout)
	if m == nil {
		tc.Failf("no h2load summary in output")
		return
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	total, succeeded := n(m[1]), n(m[4])
	if succeeded != total {
		tc.Failf("h2load: %s", m[0])
	}
	if s := h2loadStatusRE.FindStringSubmatch(stdout); s == nil || n(s[1]) != total {
		tc.Failf("h2load: not all responses were 2xx: %v", s)
	}
}
