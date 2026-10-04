// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"time"
)

// peerClients and peerServers are the non-Go implementations tested
// against Go's server and client, respectively.
var (
	peerClients = []ClientImpl{
		&jsonClient{name: "python-h2", dir: "python", features: feats(allClientFeatures, "-report-header-order")},
		&jsonClient{name: "node", dir: "node", features: feats(allClientFeatures, "-report-header-order")},
		&jsonClient{name: "rust-h2", dir: "rust", features: feats(allClientFeatures)},
		// Java's HttpClient only supports h2c via HTTP/1.1 Upgrade,
		// which Go doesn't implement, and doesn't expose trailers or
		// interim responses.
		&jsonClient{name: "java", dir: "java", features: feats(allClientFeatures,
			"-h2c -trailers-recv -trailers-send -informational -report-header-order")},
	}
	peerServers = jsonServers("python-h2", "python", feats(allRoutes)).
			and(jsonServers("node", "node", feats(allRoutes))).
			and(jsonServers("rust-h2", "rust", feats(allRoutes))).
			and(jsonServers("jetty", "java", feats(allRoutes)))
)

// serverProfiles are HTTP/2 settings that configurable peer servers
// are run with, to exercise the Go client's handling of the peer's
// limits.
var serverProfiles = []struct {
	name     string
	settings map[string]uint32
}{
	{name: ""},
	{name: "constrained", settings: map[string]uint32{
		"SETTINGS_MAX_CONCURRENT_STREAMS": 2,
		"SETTINGS_INITIAL_WINDOW_SIZE":    1024,
		"SETTINGS_HEADER_TABLE_SIZE":      0,
		"SETTINGS_MAX_FRAME_SIZE":         16384,
	}},
	{name: "large", settings: map[string]uint32{
		"SETTINGS_MAX_FRAME_SIZE":      1<<24 - 1,
		"SETTINGS_INITIAL_WINDOW_SIZE": 1<<31 - 1,
		"SETTINGS_HEADER_TABLE_SIZE":   65536,
	}},
}

type serverList []ServerImpl

func (l serverList) and(m serverList) serverList { return append(l, m...) }

// jsonServers returns a jsonServer for each of serverProfiles.
func jsonServers(name, dir string, features featureSet) serverList {
	var l serverList
	for _, p := range serverProfiles {
		n := name
		if p.name != "" {
			n += ":" + p.name
		}
		l = append(l, &jsonServer{name: n, dir: dir, features: features, settings: p.settings})
	}
	return l
}

// jsonClient is a peer client implementing the JSON client protocol
// described in peers/README.md.
type jsonClient struct {
	name     string
	dir      string
	features featureSet
}

func (c *jsonClient) Name() string         { return c.name }
func (c *jsonClient) Features() featureSet { return c.features }
func (c *jsonClient) Images() []string     { return []string{c.dir} }

func (c *jsonClient) Do(ctx context.Context, tc *testCtx, spec *ClientSpec) (*ClientOutput, error) {
	id, err := tc.env.Docker.ToolContainer(ctx, c.dir)
	if err != nil {
		return nil, err
	}
	in, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	stdout, stderr, err := tc.env.Docker.Exec(ctx, id, in, "client")
	if len(stderr) > 0 {
		tc.Logf("%s client stderr:\n%s", c.name, truncate(string(stderr), 8000))
	}
	if err != nil && len(stdout) == 0 {
		return nil, fmt.Errorf("%v", err)
	}
	out := new(ClientOutput)
	if err := json.Unmarshal(stdout, out); err != nil {
		return nil, fmt.Errorf("bad client output: %v\n%s", err, truncate(string(stdout), 2000))
	}
	return out, nil
}

// jsonServer is a peer server implementing the route set described
// in peers/README.md, configured by the JSON server protocol.
type jsonServer struct {
	name     string
	dir      string
	features featureSet
	settings map[string]uint32
}

func (s *jsonServer) Name() string         { return s.name }
func (s *jsonServer) Features() featureSet { return s.features }
func (s *jsonServer) Images() []string     { return []string{s.dir} }

func (s *jsonServer) Start(ctx context.Context, tc *testCtx, mode string) (string, error) {
	d := tc.env.Docker
	_, addr, err := d.Shared("server:"+s.name+"/"+mode, func() (string, string, error) {
		port, err := freePort()
		if err != nil {
			return "", "", err
		}
		spec := ServerSpec{
			Addr:     fmt.Sprintf("127.0.0.1:%d", port),
			TLS:      mode == "tls",
			Cert:     "/work/cert.pem",
			Key:      "/work/key.pem",
			Settings: maps.Clone(s.settings),
		}
		j, _ := json.Marshal(spec)
		id, err := d.Start(context.Background(), s.dir, "server", string(j))
		if err != nil {
			return "", "", err
		}
		if err := d.waitListening(ctx, id, spec.Addr, 60*time.Second); err != nil {
			return "", "", err
		}
		return id, spec.Addr, nil
	})
	return addr, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n... (%d bytes truncated)", len(s)-n)
}
