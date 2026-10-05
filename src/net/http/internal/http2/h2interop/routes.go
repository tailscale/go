// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// routeHandler returns the Go implementation of the interop route set
// documented in peers/README.md. It's used both by the Go HTTP/2
// server under test and by the HTTP/1 backend behind reverse proxy
// peers (nginx, envoy, etc).
func routeHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(len(helloBody)))
		io.WriteString(w, helloBody)
	})
	mux.HandleFunc("/bytes/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, ok := pathInt(w, r, "n")
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(n))
		w.Write(Pattern(n))
	})
	mux.HandleFunc("/stream/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, ok := pathInt(w, r, "n")
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		p := Pattern(n)
		rc := http.NewResponseController(w)
		for len(p) > 0 {
			c := min(len(p), 1000)
			if _, err := w.Write(p[:c]); err != nil {
				return
			}
			rc.Flush()
			p = p[c:]
		}
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.ContentLength >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(r.ContentLength, 10))
		}
		rc := http.NewResponseController(w)
		rc.EnableFullDuplex()
		w.WriteHeader(200)
		rc.Flush()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				rc.Flush()
			}
			if err != nil {
				return
			}
		}
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		h := sha256.New()
		n, err := io.Copy(h, r.Body)
		if err != nil {
			http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
			return
		}
		ub := UploadBody{
			Len:           n,
			SHA256:        hex.EncodeToString(h.Sum(nil)),
			ContentLength: r.ContentLength,
		}
		if len(r.Trailer) > 0 {
			ub.Trailer = lowerHeader(r.Trailer)
		}
		writeJSON(w, ub)
	})
	mux.HandleFunc("/trailers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Trailer", "X-Trailer-A, X-Trailer-B")
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, trailersBody)
		w.Header().Set("X-Trailer-A", "1")
		w.Header().Set("X-Trailer-B", "two")
	})
	mux.HandleFunc("/status/{code}", func(w http.ResponseWriter, r *http.Request) {
		code, ok := pathInt(w, r, "code")
		if !ok {
			return
		}
		w.WriteHeader(code)
	})
	mux.HandleFunc("/bigheader/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, ok := pathInt(w, r, "n")
		if !ok {
			return
		}
		w.Header().Set("X-Big", headerPattern(n))
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/manyheaders/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, ok := pathInt(w, r, "n")
		if !ok {
			return
		}
		for i := range n {
			w.Header().Set(fmt.Sprintf("X-H-%d", i), fmt.Sprintf("value-%d", i))
		}
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/early-hints", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</style.css>; rel=preload; as=style")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, InfoBody{
			Method:    r.Method,
			Path:      r.URL.RequestURI(),
			Authority: r.Host,
			Proto:     r.Proto,
			Header:    lowerHeader(r.Header),
		})
	})
	mux.HandleFunc("/delay/{ms}", func(w http.ResponseWriter, r *http.Request) {
		ms, ok := pathInt(w, r, "ms")
		if !ok {
			return
		}
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/rst", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(Pattern(1000))
		http.NewResponseController(w).Flush()
		panic(http.ErrAbortHandler)
	})
	return mux
}

func pathInt(w http.ResponseWriter, r *http.Request, name string) (int, bool) {
	n, err := strconv.Atoi(r.PathValue(name))
	if err != nil || n < 0 {
		http.Error(w, "bad "+name, http.StatusBadRequest)
		return 0, false
	}
	return n, true
}

func writeJSON(w http.ResponseWriter, v any) {
	j, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(j)))
	w.Write(j)
}

func lowerHeader(h http.Header) map[string][]string {
	m := make(map[string][]string, len(h))
	for k, vv := range h {
		lk := strings.ToLower(k)
		m[lk] = append(m[lk], vv...)
	}
	return m
}
