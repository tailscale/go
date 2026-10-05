// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// This file has reverse proxy peers: nginx, HAProxy, Envoy, Apache
// httpd, and nghttpx. Each is used in two roles:
//
//   - As a server, the Go client sends HTTP/2 requests to the proxy,
//     which forwards them over HTTP/1.1 to a Go HTTP/1 backend serving
//     the route set. This tests the Go client against the proxy's
//     HTTP/2 server implementation.
//
//   - As a client, an HTTP/1.1 driver sends requests to the proxy,
//     which forwards them over HTTP/2 (TLS or h2c) to the Go server.
//     This tests the Go server against the proxy's HTTP/2 client
//     implementation.

type proxyPeer struct {
	name string
	dir  string
	// serverFeatures are the features when used as an HTTP/2 server.
	serverFeatures featureSet
	// clientFeatures are the features when used as an HTTP/2 client,
	// or nil if the proxy can't forward over HTTP/2.
	clientFeatures featureSet
	// config returns the container arguments to run the proxy
	// listening on front (speaking frontMode: "tls", "h2c", or "h1")
	// and forwarding to back (speaking backMode: "h1", "tls", or
	// "h2c"). It may write configuration files to dir, which is
	// mounted in the container at /work/conf.
	config func(dir, confName, front, frontMode, back, backMode string) ([]string, error)
}

// proxyRouteFeatures are the route set features available through a
// reverse proxy to the Go HTTP/1 backend. Proxies generally don't
// forward 1xx responses, trailers, or stream resets from an HTTP/1
// backend.
const proxyRouteFeatures = "tls h2c route:hello route:bytes route:stream route:echo route:upload " +
	"route:status route:bigheader route:manyheaders route:info route:delay"

// proxyClientFeatures are the client features of an HTTP/1 driver
// going through a reverse proxy.
const proxyClientFeatures = "tls h2c concurrent body body-hash request-body stream-request-body custom-headers"

var proxyPeers = []*proxyPeer{
	{
		name:           "nginx",
		dir:            "nginx",
		serverFeatures: feats(proxyRouteFeatures),
		clientFeatures: feats(proxyClientFeatures),
		config:         nginxConfig,
	},
	{
		name:           "haproxy",
		dir:            "haproxy",
		serverFeatures: feats(proxyRouteFeatures, "route:trailers"),
		clientFeatures: feats(proxyClientFeatures),
		config:         haproxyConfig,
	},
	{
		name:           "envoy",
		dir:            "envoy",
		serverFeatures: feats(proxyRouteFeatures),
		clientFeatures: feats(proxyClientFeatures),
		config:         envoyConfig,
	},
	{
		name:           "apache",
		dir:            "apache",
		serverFeatures: feats(proxyRouteFeatures),
		clientFeatures: feats(proxyClientFeatures),
		config:         apacheConfig,
	},
	{
		name:           "nghttpx",
		dir:            "cli",
		serverFeatures: feats(proxyRouteFeatures),
		clientFeatures: feats(proxyClientFeatures),
		config:         nghttpxConfig,
	},
}

func init() {
	for _, p := range proxyPeers {
		peerServers = append(peerServers, &proxyServer{p})
		if p.clientFeatures != nil {
			peerClients = append(peerClients, &proxyClient{p})
		}
	}
}

// h1Backend is the shared Go HTTP/1 backend behind proxies.
var h1Backend = sync.OnceValues(func() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: routeHandler(), Protocols: new(http.Protocols)}
	srv.Protocols.SetHTTP1(true)
	go srv.Serve(ln)
	return ln.Addr().String(), nil
})

var confSeq atomic.Int64

// startProxy starts a proxy container, returning its id and front address.
func startProxy(ctx context.Context, d *dockerEnv, p *proxyPeer, frontMode, back, backMode string) (id, front string, err error) {
	port, err := freePort()
	if err != nil {
		return "", "", err
	}
	front = fmt.Sprintf("127.0.0.1:%d", port)
	confDir := filepath.Join(d.workDir, "conf")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return "", "", err
	}
	confName := fmt.Sprintf("%s-%d", p.name, confSeq.Add(1))
	args, err := p.config(confDir, confName, front, frontMode, back, backMode)
	if err != nil {
		return "", "", err
	}
	id, err = d.Start(context.Background(), p.dir, args...)
	if err != nil {
		return "", "", err
	}
	if err := d.waitListening(ctx, id, front, 30*time.Second); err != nil {
		d.Stop(id)
		return "", "", err
	}
	return id, front, nil
}

// proxyServer is a proxy used as an HTTP/2 server.
type proxyServer struct{ p *proxyPeer }

func (s *proxyServer) Name() string         { return s.p.name }
func (s *proxyServer) Features() featureSet { return s.p.serverFeatures }
func (s *proxyServer) Images() []string     { return []string{s.p.dir} }

func (s *proxyServer) Start(ctx context.Context, tc *testCtx, mode string) (string, error) {
	d := tc.env.Docker
	_, addr, err := d.Shared("proxy-server:"+s.p.name+"/"+mode, func() (string, string, error) {
		back, err := h1Backend()
		if err != nil {
			return "", "", err
		}
		return startProxy(ctx, d, s.p, mode, back, "h1")
	})
	return addr, err
}

// proxyClient is a proxy used as an HTTP/2 client, driven by an
// HTTP/1 client.
type proxyClient struct{ p *proxyPeer }

func (c *proxyClient) Name() string         { return c.p.name }
func (c *proxyClient) Features() featureSet { return c.p.clientFeatures }
func (c *proxyClient) Images() []string     { return []string{c.p.dir} }

func (c *proxyClient) Do(ctx context.Context, tc *testCtx, spec *ClientSpec) (*ClientOutput, error) {
	backMode := "tls"
	if spec.H2C {
		backMode = "h2c"
	}
	upstream := strings.TrimPrefix(strings.TrimPrefix(spec.URL, "https://"), "http://")
	id, front, err := startProxy(ctx, tc.env.Docker, c.p, "h1", upstream, backMode)
	if err != nil {
		return nil, err
	}
	defer func() {
		tc.Logf("%s logs:\n%s", c.p.name, truncate(tc.env.Docker.Logs(id), 8000))
		tc.env.Docker.Stop(id)
	}()
	// Drive the proxy with HTTP/1, sending the upstream's address
	// as the Host so that it's forwarded as :authority.
	dspec := *spec
	dspec.URL = "http://" + front
	if dspec.Authority == "" {
		dspec.Authority = upstream
	}
	tr := h1Transport()
	tr.MaxConnsPerHost = 0
	defer tr.CloseIdleConnections()
	out := doRequests(ctx, tc, tr, &dspec)
	for _, r := range out.Results {
		// The driver speaks HTTP/1 to the proxy; the protocol
		// that matters is the proxy's upstream protocol.
		r.Proto = ""
	}
	return out, nil
}

func hostPort(addr string) (string, string) {
	h, p, _ := net.SplitHostPort(addr)
	return h, p
}

func writeConf(dir, name, content string) (string, error) {
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		return "", err
	}
	return "/work/conf/" + name, nil
}

func nginxConfig(dir, confName, front, frontMode, back, backMode string) ([]string, error) {
	listen := "listen " + front + ";"
	switch frontMode {
	case "tls":
		listen = "listen " + front + " ssl;\n\t\thttp2 on;"
	case "h2c":
		listen = "listen " + front + ";\n\t\thttp2 on;"
	}
	upstream := "http://" + back
	upstreamOpts := "proxy_http_version 1.1;\n\t\t\tproxy_set_header Connection \"\";"
	switch backMode {
	case "tls":
		upstream = "https://" + back
		upstreamOpts = "proxy_http_version 2;\n\t\t\tproxy_ssl_verify off;"
	case "h2c":
		upstreamOpts = "proxy_http_version 2;"
	}
	conf := fmt.Sprintf(`worker_processes 1;
error_log stderr notice;
pid /tmp/nginx.pid;
events {}
http {
	access_log off;
	client_body_temp_path /tmp/client_body;
	proxy_temp_path /tmp/proxy;
	client_max_body_size 0;
	large_client_header_buffers 16 128k;
	http2_max_concurrent_streams 128;
	server {
		%s
		ssl_certificate /work/cert.pem;
		ssl_certificate_key /work/key.pem;
		location / {
			proxy_pass %s;
			%s
			proxy_set_header Host $http_host;
			proxy_buffering off;
			proxy_request_buffering off;
			proxy_buffer_size 128k;
			proxy_buffers 8 128k;
		}
	}
}
`, listen, upstream, upstreamOpts)
	path, err := writeConf(dir, confName+".conf", conf)
	if err != nil {
		return nil, err
	}
	return []string{"--entrypoint", "nginx", "--", "-c", path, "-g", "daemon off;"}, nil
}

func haproxyConfig(dir, confName, front, frontMode, back, backMode string) ([]string, error) {
	bind := "bind " + front
	switch frontMode {
	case "tls":
		bind += " ssl crt /work/conf/" + confName + ".pem alpn h2"
	case "h2c":
		bind += " proto h2"
	}
	server := "server s1 " + back
	switch backMode {
	case "tls":
		server += " ssl verify none alpn h2"
	case "h2c":
		server += " proto h2"
	}
	// HAProxy wants the certificate and key in one file.
	pem := append(append([]byte(nil), mustRead("cert.pem", dir)...), '\n')
	pem = append(pem, mustRead("key.pem", dir)...)
	if err := os.WriteFile(filepath.Join(dir, confName+".pem"), pem, 0o644); err != nil {
		return nil, err
	}
	conf := fmt.Sprintf(`global
	log stderr format raw local0 notice
	tune.bufsize 262144
	tune.h2.max-concurrent-streams 128
defaults
	mode http
	log global
	timeout connect 5s
	timeout client 60s
	timeout server 60s
frontend fe
	%s
	default_backend be
backend be
	%s
`, bind, server)
	path, err := writeConf(dir, confName+".cfg", conf)
	if err != nil {
		return nil, err
	}
	return []string{"--entrypoint", "haproxy", "--", "-f", path, "-db"}, nil
}

// mustRead reads a file from the work directory (the parent of dir).
func mustRead(name, dir string) []byte {
	b, err := os.ReadFile(filepath.Join(filepath.Dir(dir), name))
	if err != nil {
		panic(err)
	}
	return b
}

func envoyConfig(dir, confName, front, frontMode, back, backMode string) ([]string, error) {
	fh, fp := hostPort(front)
	bh, bp := hostPort(back)
	codec := "AUTO"
	downstreamTLS := ""
	if frontMode == "tls" {
		downstreamTLS = `
      transport_socket:
        name: envoy.transport_sockets.tls
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.DownstreamTlsContext
          common_tls_context:
            alpn_protocols: ["h2"]
            tls_certificates:
            - certificate_chain: {filename: /work/cert.pem}
              private_key: {filename: /work/key.pem}`
	}
	if frontMode == "h2c" {
		codec = "HTTP2"
	}
	upstreamProto := `
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config:
          http_protocol_options: {enable_trailers: true}`
	upstreamTLS := ""
	if backMode == "tls" || backMode == "h2c" {
		upstreamProto = `
    typed_extension_protocol_options:
      envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
        "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
        explicit_http_config:
          http2_protocol_options: {}`
	}
	if backMode == "tls" {
		upstreamTLS = `
    transport_socket:
      name: envoy.transport_sockets.tls
      typed_config:
        "@type": type.googleapis.com/envoy.extensions.transport_sockets.tls.v3.UpstreamTlsContext
        common_tls_context:
          alpn_protocols: ["h2"]`
	}
	conf := fmt.Sprintf(`static_resources:
  listeners:
  - name: front
    address:
      socket_address: {address: %s, port_value: %s}
    filter_chains:
    - filters:
      - name: envoy.filters.network.http_connection_manager
        typed_config:
          "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
          stat_prefix: front
          codec_type: %s
          stream_idle_timeout: 60s
          http_protocol_options: {enable_trailers: true}
          route_config:
            virtual_hosts:
            - name: all
              domains: ["*"]
              routes:
              - match: {prefix: "/"}
                route: {cluster: back, timeout: 60s}
          http_filters:
          - name: envoy.filters.http.router
            typed_config:
              "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router%s
  clusters:
  - name: back
    type: STATIC
    connect_timeout: 5s%s%s
    load_assignment:
      cluster_name: back
      endpoints:
      - lb_endpoints:
        - endpoint:
            address:
              socket_address: {address: %s, port_value: %s}
`, fh, fp, codec, downstreamTLS, upstreamProto, upstreamTLS, bh, bp)
	path, err := writeConf(dir, confName+".yaml", conf)
	if err != nil {
		return nil, err
	}
	return []string{"--entrypoint", "envoy", "--", "-c", path, "--log-level", "warn", "--concurrency", "1",
		"--base-id", fmt.Sprint(confSeq.Load())}, nil
}

func apacheConfig(dir, confName, front, frontMode, back, backMode string) ([]string, error) {
	protocols := "Protocols http/1.1"
	tls := ""
	switch frontMode {
	case "tls":
		protocols = "Protocols h2 http/1.1"
		tls = "SSLEngine on\nSSLCertificateFile /work/cert.pem\nSSLCertificateKeyFile /work/key.pem"
	case "h2c":
		protocols = "Protocols h2c http/1.1\nH2Direct on"
	}
	proxyPass := "ProxyPass / http://" + back + "/ responsefieldsize=131072"
	switch backMode {
	case "tls":
		proxyPass = "SSLProxyEngine on\nSSLProxyVerify none\nSSLProxyCheckPeerName off\nSSLProxyCheckPeerCN off\nSSLProxyCheckPeerExpire off\nProxyPass / h2://" + back + "/"
	case "h2c":
		proxyPass = "ProxyPass / h2c://" + back + "/"
	}
	conf := fmt.Sprintf(`ServerRoot /usr/local/apache2
Listen %s
LoadModule mpm_event_module modules/mod_mpm_event.so
LoadModule authz_core_module modules/mod_authz_core.so
LoadModule unixd_module modules/mod_unixd.so
LoadModule log_config_module modules/mod_log_config.so
LoadModule socache_shmcb_module modules/mod_socache_shmcb.so
LoadModule ssl_module modules/mod_ssl.so
LoadModule http2_module modules/mod_http2.so
LoadModule proxy_module modules/mod_proxy.so
LoadModule proxy_http_module modules/mod_proxy_http.so
LoadModule proxy_http2_module modules/mod_proxy_http2.so
User daemon
Group daemon
ServerName localhost
ErrorLog /proc/self/fd/2
LogLevel warn
PidFile /tmp/%s.pid
Mutex posixsem
Timeout 60
LimitRequestBody 0
LimitRequestFieldSize 131072
H2MaxSessionStreams 128
ProxyPreserveHost On
%s
%s
%s
`, front, confName, protocols, tls, proxyPass)
	path, err := writeConf(dir, confName+".conf", conf)
	if err != nil {
		return nil, err
	}
	return []string{"--entrypoint", "httpd", "--", "-f", path, "-DFOREGROUND"}, nil
}

func nghttpxConfig(dir, confName, front, frontMode, back, backMode string) ([]string, error) {
	fh, fp := hostPort(front)
	bh, bp := hostPort(back)
	frontend := fmt.Sprintf("%s,%s", fh, fp)
	args := []string{"--entrypoint", "nghttpx", "--"}
	if frontMode != "tls" {
		frontend += ";no-tls"
	}
	backend := fmt.Sprintf("%s,%s", bh, bp)
	switch backMode {
	case "tls":
		backend += ";;proto=h2;tls"
		args = append(args, "--insecure")
	case "h2c":
		backend += ";;proto=h2"
	}
	args = append(args, "--frontend="+frontend, "--backend="+backend, "--workers=1",
		"--log-level=WARN", "--errorlog-file=/dev/stderr", "--accesslog-file=/dev/null",
		"--frontend-http2-max-concurrent-streams=128", "--no-ocsp")
	if frontMode == "tls" {
		args = append(args, "/work/key.pem", "/work/cert.pem")
	}
	return args, nil
}
