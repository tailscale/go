// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"syscall"
	"time"
)

// A Tap is a TCP proxy that sits between an HTTP/2 client and server
// and feeds both directions of each connection to a connValidator.
//
// In TLS mode, the tap terminates TLS from the client and makes its
// own TLS connection to the server. It offers the server the same
// ALPN protocols and TLS versions that the client offered, and
// completes the client's handshake with the protocol the server
// selected, so the endpoints negotiate as they would directly.
type Tap struct {
	ln         net.Listener
	upstream   string
	useTLS     bool
	clientName string
	serverName string

	mu     sync.Mutex
	conns  []*connValidator
	open   map[net.Conn]bool
	closed bool
	nconns int
	wg     sync.WaitGroup
}

func startTap(upstream string, useTLS bool, clientName, serverName string) (*Tap, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t := &Tap{
		ln:         ln,
		upstream:   upstream,
		useTLS:     useTLS,
		clientName: clientName,
		serverName: serverName,
		open:       map[net.Conn]bool{},
	}
	t.wg.Add(1)
	go t.serve()
	return t, nil
}

// Addr returns the tap's listening address.
func (t *Tap) Addr() string { return t.ln.Addr().String() }

func (t *Tap) serve() {
	defer t.wg.Done()
	for {
		c, err := t.ln.Accept()
		if err != nil {
			return
		}
		if !t.track(c) {
			c.Close()
			return
		}
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			t.handle(c)
		}()
	}
}

func (t *Tap) track(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.open[c] = true
	return true
}

func (t *Tap) untrack(c net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.open, c)
}

func (t *Tap) handle(down net.Conn) {
	defer t.untrack(down)
	defer down.Close()
	var up net.Conn
	var err error
	isH2 := true
	if t.useTLS {
		var upTLS *tls.Conn
		cfg := &tls.Config{
			Certificates: []tls.Certificate{testCert},
			GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
				ucfg := &tls.Config{
					InsecureSkipVerify: true,
					NextProtos:         hello.SupportedProtos,
					ServerName:         hello.ServerName,
				}
				for _, v := range hello.SupportedVersions {
					ucfg.MaxVersion = max(ucfg.MaxVersion, v)
				}
				d := &net.Dialer{Timeout: 10 * time.Second}
				c, err := tls.DialWithDialer(d, "tcp", t.upstream, ucfg)
				if err != nil {
					return nil, fmt.Errorf("tap: dialing upstream: %w", err)
				}
				upTLS = c
				cs := c.ConnectionState()
				dcfg := &tls.Config{
					Certificates: []tls.Certificate{testCert},
					MinVersion:   cs.Version,
					MaxVersion:   cs.Version,
				}
				if cs.NegotiatedProtocol != "" {
					dcfg.NextProtos = []string{cs.NegotiatedProtocol}
				}
				return dcfg, nil
			},
		}
		tc := tls.Server(down, cfg)
		tc.SetDeadline(time.Now().Add(10 * time.Second))
		if err := tc.Handshake(); err != nil {
			if upTLS != nil {
				upTLS.Close()
			}
			return
		}
		tc.SetDeadline(time.Time{})
		down = tc
		up = upTLS
		isH2 = tc.ConnectionState().NegotiatedProtocol == "h2"
	} else {
		up, err = net.DialTimeout("tcp", t.upstream, 10*time.Second)
		if err != nil {
			return
		}
	}
	if !t.track(up) {
		up.Close()
		return
	}
	defer t.untrack(up)
	defer up.Close()

	t.mu.Lock()
	t.nconns++
	var v *connValidator
	if isH2 {
		v = newConnValidator(len(t.conns), t.clientName, t.serverName)
		t.conns = append(t.conns, v)
	}
	t.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		pipe(v, sideClient, down, up)
	}()
	go func() {
		defer wg.Done()
		pipe(v, sideServer, up, down)
	}()
	wg.Wait()
	if v != nil {
		v.finish()
	}
}

// pipe copies from src to dst, feeding the validator first.
func pipe(v *connValidator, side int, src, dst net.Conn) {
	buf := make([]byte, 64<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if v != nil {
				v.Feed(side, buf[:n])
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				src.Close()
				return
			}
		}
		if err != nil {
			if err == io.EOF {
				closeWrite(dst)
				return
			}
			// Propagate a TCP reset as a reset.
			if errors.Is(err, syscall.ECONNRESET) {
				if tc, ok := dst.(*net.TCPConn); ok {
					tc.SetLinger(0)
				}
			}
			dst.Close()
			return
		}
	}
}

func closeWrite(c net.Conn) {
	switch c := c.(type) {
	case *net.TCPConn:
		c.CloseWrite()
	case *tls.Conn:
		c.CloseWrite()
	default:
		c.Close()
	}
}

// Close shuts down the tap and all proxied connections.
func (t *Tap) Close() {
	t.mu.Lock()
	t.closed = true
	for c := range t.open {
		c.Close()
	}
	t.mu.Unlock()
	t.ln.Close()
	t.wg.Wait()
}

// Validators returns the validators of the HTTP/2 connections seen so far.
func (t *Tap) Validators() []*connValidator {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*connValidator(nil), t.conns...)
}

// NumConns returns the total number of connections, including HTTP/1.
func (t *Tap) NumConns() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nconns
}

// ServerMaxConcurrentStreams returns the smallest
// SETTINGS_MAX_CONCURRENT_STREAMS sent by the server on any
// connection.
func (t *Tap) ServerMaxConcurrentStreams() uint32 {
	limit := uint32(1<<32 - 1)
	for _, v := range t.Validators() {
		v.mu.Lock()
		limit = min(limit, v.ep[sideServer].settings.latest(setMaxConcurrentStreams))
		v.mu.Unlock()
	}
	return limit
}
