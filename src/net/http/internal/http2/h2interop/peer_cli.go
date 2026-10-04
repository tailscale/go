// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// This file has peers driven by command-line tools in the "cli"
// image: curl and nghttp as clients, nghttpd as a server.

func init() {
	peerClients = append(peerClients, &curlClient{}, &nghttpClient{})
	for _, p := range serverProfiles {
		peerServers = append(peerServers, &nghttpdServer{profile: p.name, settings: p.settings})
	}
}

// BodyFile returns the path in containers of a file containing
// Pattern(n), creating it if needed.
func (d *dockerEnv) BodyFile(n int) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	name := filepath.Join(d.workDir, "bodies", strconv.Itoa(n))
	if _, err := os.Stat(name); err == nil {
		return "/work/bodies/" + strconv.Itoa(n), nil
	}
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return "", err
	}
	tmp := name + ".tmp"
	if err := os.WriteFile(tmp, Pattern(n), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, name); err != nil {
		return "", err
	}
	return "/work/bodies/" + strconv.Itoa(n), nil
}

// collectScript runs its arguments as a command in a fresh directory,
// then prints each file the command created with its size, SHA-256,
// and (if small) base64 contents. If $STDIN_FILE is set, the command's
// stdin is redirected from it.
const collectScript = `d=$(mktemp -d); cd $d
if [ -n "$STDIN_FILE" ]; then "$@" < "$STDIN_FILE"; else "$@"; fi
echo; echo "@@@EXIT $?"
for f in *; do
  [ -f "$f" ] || continue
  n=$(wc -c < "$f")
  echo "@@@FILE $f $n $(sha256sum "$f" | cut -c1-64)"
  if [ $n -le 65536 ]; then base64 -w0 "$f"; fi
  echo
done
cd /; rm -rf $d`

type collectedFile struct {
	size   int64
	sha256 string
	data   []byte // nil if large
}

// runCollect runs args in the tool container for dir using
// collectScript, returning the command's stdout, the files it created,
// and its exit status.
func runCollect(ctx context.Context, tc *testCtx, dir string, env []string, args []string) (stdout string, files map[string]*collectedFile, exit int, err error) {
	id, err := tc.env.Docker.ToolContainer(ctx, dir)
	if err != nil {
		return "", nil, 0, err
	}
	tc.Logf("running: %s", shellQuote(args))
	out, stderr, err := tc.env.Docker.ExecEnv(ctx, id, env, nil, append([]string{"sh", "-c", collectScript, "sh"}, args...)...)
	if len(stderr) > 0 {
		tc.Logf("stderr:\n%s", truncate(string(stderr), 8000))
	}
	if err != nil && !bytes.Contains(out, []byte("@@@EXIT")) {
		return "", nil, 0, fmt.Errorf("%v", err)
	}
	files = map[string]*collectedFile{}
	i := bytes.LastIndex(out, []byte("\n@@@EXIT "))
	if i < 0 {
		return "", nil, 0, fmt.Errorf("bad collect output")
	}
	stdout = string(out[:i])
	sc := bufio.NewScanner(bytes.NewReader(out[i+1:]))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "@@@EXIT "); ok {
			exit, _ = strconv.Atoi(v)
			continue
		}
		if v, ok := strings.CutPrefix(line, "@@@FILE "); ok {
			f := strings.Fields(v)
			if len(f) != 3 || !sc.Scan() {
				return "", nil, 0, fmt.Errorf("bad collect output %q", line)
			}
			cf := &collectedFile{sha256: f[2]}
			cf.size, _ = strconv.ParseInt(f[1], 10, 64)
			if cf.size <= 65536 {
				cf.data, _ = base64.StdEncoding.DecodeString(sc.Text())
			}
			files[f[0]] = cf
		}
	}
	return stdout, files, exit, nil
}

func shellQuote(args []string) string {
	var sb strings.Builder
	for i, a := range args {
		if i > 0 {
			sb.WriteByte(' ')
		}
		if a != "" && strings.Trim(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@,") == "" {
			sb.WriteString(a)
		} else {
			sb.WriteString("'" + strings.ReplaceAll(a, "'", `'\''`) + "'")
		}
		if sb.Len() > 2000 {
			sb.WriteString(" ...")
			break
		}
	}
	return sb.String()
}

func (f *collectedFile) fill(r *Result) {
	r.BodyLen = f.size
	r.BodySHA256 = f.sha256
	if f.data != nil && utf8.Valid(f.data) {
		r.Body = string(f.data)
	}
}

// curlClient is the curl command-line tool (libcurl, using nghttp2).
type curlClient struct{}

func (c *curlClient) Name() string     { return "curl" }
func (c *curlClient) Images() []string { return []string{"cli"} }
func (c *curlClient) Features() featureSet {
	// curl can't send request trailers or reset a stream.
	return feats(allClientFeatures, "-trailers-send -cancel")
}

func (c *curlClient) Do(ctx context.Context, tc *testCtx, spec *ClientSpec) (*ClientOutput, error) {
	args := []string{"curl", "-sS"}
	if spec.Concurrent {
		args = append(args, "--parallel", "--parallel-max", "300")
	}
	var env []string
	for i, r := range spec.Requests {
		if i > 0 {
			args = append(args, "--next")
		}
		args = append(args, "-k", "--path-as-is", "--max-time", "50",
			"-o", fmt.Sprintf("%d.body", i), "-D", fmt.Sprintf("%d.hdr", i),
			"-w", fmt.Sprintf("\n@@@BEGIN %d\n%%{json}\n@@@END\n", i))
		if spec.H2C {
			args = append(args, "--http2-prior-knowledge")
		} else {
			args = append(args, "--http2")
		}
		switch {
		case r.Method == "HEAD":
			args = append(args, "--head")
		case r.Method != "GET" || r.BodyLen > 0:
			args = append(args, "-X", r.Method)
		}
		if spec.Authority != "" {
			args = append(args, "-H", "Host: "+spec.Authority)
		}
		for _, kv := range r.Header {
			args = append(args, "-H", kv[0]+": "+kv[1])
		}
		if r.ExpectContinue {
			args = append(args, "-H", "Expect: 100-continue")
		} else {
			args = append(args, "-H", "Expect:")
		}
		if r.BodyLen > 0 {
			f, err := tc.env.Docker.BodyFile(r.BodyLen)
			if err != nil {
				return nil, err
			}
			if r.NoContentLength {
				if len(env) > 0 {
					return nil, fmt.Errorf("curl: only one request without content-length per invocation")
				}
				env = append(env, "STDIN_FILE="+f)
				args = append(args, "-T", "/dev/stdin")
			} else {
				args = append(args, "--data-binary", "@"+f, "-H", "Content-Type:")
			}
		} else if r.Method == "POST" || r.Method == "PUT" {
			args = append(args, "--data-binary", "", "-H", "Content-Type:")
		}
		args = append(args, spec.URL+r.Path)
	}
	stdout, files, exit, err := runCollect(ctx, tc, "cli", env, args)
	if err != nil {
		return nil, err
	}
	tc.Logf("curl exit status %d", exit)
	out := &ClientOutput{Results: make([]*Result, len(spec.Requests))}
	for i := range out.Results {
		out.Results[i] = &Result{Error: "curl reported no result"}
	}
	for chunk := range strings.SplitSeq(stdout, "@@@BEGIN ") {
		idxStr, rest, ok := strings.Cut(chunk, "\n")
		if !ok {
			continue
		}
		i, err := strconv.Atoi(idxStr)
		if err != nil || i < 0 || i >= len(out.Results) {
			continue
		}
		js, _, _ := strings.Cut(rest, "\n@@@END")
		var w struct {
			ResponseCode int     `json:"response_code"`
			HTTPVersion  string  `json:"http_version"`
			ExitCode     int     `json:"exitcode"`
			ErrorMsg     *string `json:"errormsg"`
			SizeDownload int64   `json:"size_download"`
		}
		if err := json.Unmarshal([]byte(js), &w); err != nil {
			out.Results[i].Error = fmt.Sprintf("bad curl -w output: %v", err)
			continue
		}
		r := &Result{Status: w.ResponseCode}
		if w.HTTPVersion == "2" {
			r.Proto = "h2"
		} else if w.HTTPVersion != "" && w.HTTPVersion != "0" {
			r.Proto = "http/" + w.HTTPVersion
		}
		if w.ExitCode != 0 {
			r.Error = fmt.Sprintf("curl exit code %d", w.ExitCode)
			if w.ErrorMsg != nil {
				r.Error += ": " + *w.ErrorMsg
			}
		}
		if f := files[fmt.Sprintf("%d.body", i)]; f != nil {
			f.fill(r)
		}
		if spec.Requests[i].Method == "HEAD" {
			// curl writes the headers to the output for HEAD.
			r.BodyLen = w.SizeDownload
			r.BodySHA256 = ""
			r.Body = ""
		}
		if f := files[fmt.Sprintf("%d.hdr", i)]; f != nil && f.data != nil {
			parseCurlHeaders(r, string(f.data))
		}
		out.Results[i] = r
	}
	return out, nil
}

// parseCurlHeaders parses curl's -D output: zero or more interim
// responses, the final response, and then any trailers.
func parseCurlHeaders(r *Result, s string) {
	blocks := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n")
	sawFinal := false
	for _, b := range blocks {
		lines := strings.Split(strings.TrimSpace(b), "\n")
		if len(lines) == 0 || lines[0] == "" {
			continue
		}
		var fields [][2]string
		start := 0
		status := 0
		if strings.HasPrefix(lines[0], "HTTP/") {
			f := strings.Fields(lines[0])
			if len(f) >= 2 {
				status, _ = strconv.Atoi(f[1])
			}
			start = 1
		}
		for _, l := range lines[start:] {
			k, v, ok := strings.Cut(l, ":")
			if !ok {
				continue
			}
			fields = append(fields, [2]string{strings.ToLower(k), strings.TrimSpace(v)})
		}
		switch {
		case status >= 100 && status < 200:
			r.Informational = append(r.Informational, &Informational{Status: status, Header: fields})
		case status != 0:
			r.Header = fields
			sawFinal = true
		case sawFinal:
			r.Trailer = append(r.Trailer, fields...)
		}
	}
}

// nghttpClient is nghttp, the nghttp2 command-line client.
type nghttpClient struct{}

func (c *nghttpClient) Name() string     { return "nghttp" }
func (c *nghttpClient) Images() []string { return []string{"cli"} }
func (c *nghttpClient) Features() featureSet {
	// nghttp can't cancel a stream, and uses the same request
	// method, headers, and body for all URIs in an invocation.
	// It reports response body lengths, but not their contents.
	return feats(allClientFeatures, "-cancel -body -body-hash")
}

func (c *nghttpClient) Do(ctx context.Context, tc *testCtx, spec *ClientSpec) (*ClientOutput, error) {
	out := &ClientOutput{}
	if !spec.Concurrent {
		// Sequential requests use separate invocations, and hence
		// separate connections.
		for _, r := range spec.Requests {
			res, err := c.run(ctx, tc, spec, []*Req{r})
			if err != nil {
				return nil, err
			}
			out.Results = append(out.Results, res...)
		}
		return out, nil
	}
	for _, r := range spec.Requests[1:] {
		r0 := spec.Requests[0]
		if r.Method != r0.Method || r.BodyLen != r0.BodyLen || !slices.Equal(r.Header, r0.Header) {
			tc.Skipf("nghttp can't send different concurrent requests")
			return nil, errSkip
		}
	}
	res, err := c.run(ctx, tc, spec, spec.Requests)
	if err != nil {
		return nil, err
	}
	out.Results = res
	return out, nil
}

var (
	nghttpFrameRE  = regexp.MustCompile(`^\[\s*[0-9.]+\] (send|recv) ([A-Z_]+) frame <length=(\d+), flags=0x([0-9a-f]+), stream_id=(\d+)>`)
	nghttpFieldRE  = regexp.MustCompile(`^\[\s*[0-9.]+\] recv \(stream_id=(\d+)\) ([^:]*|:[^:]*): (.*)$`)
	nghttpErrorRE  = regexp.MustCompile(`error_code=([A-Z_0-9]+)`)
	nghttpPadlenRE = regexp.MustCompile(`\(padlen=(\d+)\)`)
)

func (c *nghttpClient) run(ctx context.Context, tc *testCtx, spec *ClientSpec, reqs []*Req) ([]*Result, error) {
	r0 := reqs[0]
	// nghttp -v writes response data interleaved with its frame log
	// in a way that can't be reliably separated, so discard it.
	args := []string{"nghttp", "-nv", "-t", "50"}
	if r0.Method != "GET" && r0.Method != "POST" {
		args = append(args, "-H", ":method: "+r0.Method)
	}
	if spec.Authority != "" {
		args = append(args, "-H", ":authority: "+spec.Authority)
	}
	for _, kv := range r0.Header {
		args = append(args, "-H", kv[0]+": "+kv[1])
	}
	if r0.BodyLen > 0 {
		f, err := tc.env.Docker.BodyFile(r0.BodyLen)
		if err != nil {
			return nil, err
		}
		args = append(args, "-d", f)
		if r0.NoContentLength {
			args = append(args, "--no-content-length")
		}
	} else if r0.Method == "POST" {
		args = append(args, "-d", "/dev/null")
	}
	for _, kv := range r0.Trailer {
		args = append(args, "--trailer", kv[0]+": "+kv[1])
	}
	if r0.ExpectContinue {
		args = append(args, "--expect-continue")
	}
	samePath := true
	for _, r := range reqs {
		samePath = samePath && r.Path == r0.Path
	}
	if samePath && len(reqs) > 1 {
		// nghttp ignores repeated URIs, so use --multiply.
		args = append(args, "-m", strconv.Itoa(len(reqs)), spec.URL+r0.Path)
	} else {
		for _, r := range reqs {
			args = append(args, spec.URL+r.Path)
		}
	}
	rawOut, _, exit, err := runCollect(ctx, tc, "cli", nil, args)
	if err != nil {
		return nil, err
	}
	tc.Logf("nghttp exit status %d", exit)
	stdout := rawOut
	tc.Logf("nghttp output:\n%s", truncate(stdout, 20000))

	type streamState struct {
		res      *Result
		fields   [][2]string
		gotFinal bool
		ended    bool
	}
	var order []uint32
	streams := map[uint32]*streamState{}
	var connErr string
	lines := strings.Split(stdout, "\n")
	for i, line := range lines {
		if m := nghttpFrameRE.FindStringSubmatch(line); m != nil {
			dir, typ := m[1], m[2]
			length, _ := strconv.Atoi(m[3])
			flags, _ := strconv.ParseUint(m[4], 16, 8)
			id64, _ := strconv.ParseUint(m[5], 10, 32)
			id := uint32(id64)
			next := ""
			if i+1 < len(lines) {
				next = lines[i+1] + " " + strings.Join(lines[i+2:min(i+4, len(lines))], " ")
			}
			if dir == "send" {
				if typ == "HEADERS" && strings.Contains(next, "; Open new stream") || typ == "HEADERS" && i+3 < len(lines) && strings.Contains(lines[i+3], "Open new stream") {
					if streams[id] == nil {
						streams[id] = &streamState{res: &Result{Proto: "h2"}}
						order = append(order, id)
					}
				}
				continue
			}
			st := streams[id]
			switch typ {
			case "HEADERS":
				if st == nil {
					continue
				}
				status := 0
				var regular [][2]string
				for _, kv := range st.fields {
					if kv[0] == ":status" {
						status, _ = strconv.Atoi(kv[1])
					} else {
						regular = append(regular, kv)
					}
				}
				switch {
				case status >= 100 && status < 200:
					st.res.Informational = append(st.res.Informational, &Informational{Status: status, Header: regular})
				case status != 0:
					st.res.Status = status
					st.res.Header = regular
					st.gotFinal = true
				default:
					st.res.Trailer = append(st.res.Trailer, regular...)
				}
				st.fields = nil
				if flags&0x1 != 0 {
					st.ended = true
				}
			case "DATA":
				if st == nil {
					continue
				}
				if flags&0x8 != 0 {
					if pm := nghttpPadlenRE.FindStringSubmatch(next); pm != nil {
						pad, _ := strconv.Atoi(pm[1])
						length -= pad + 1
					}
				}
				st.res.BodyLen += int64(length)
				if flags&0x1 != 0 {
					st.ended = true
				}
			case "RST_STREAM":
				if st != nil && !st.ended {
					code := "unknown"
					if em := nghttpErrorRE.FindStringSubmatch(next); em != nil {
						code = em[1]
					}
					st.res.Error = "stream reset by server: " + code
				}
			case "GOAWAY":
				if em := nghttpErrorRE.FindStringSubmatch(next); em != nil && em[1] != "NO_ERROR" {
					connErr = "GOAWAY " + em[1]
				}
			}
			continue
		}
		if m := nghttpFieldRE.FindStringSubmatch(line); m != nil {
			id64, _ := strconv.ParseUint(m[1], 10, 32)
			if st := streams[uint32(id64)]; st != nil {
				st.fields = append(st.fields, [2]string{m[2], m[3]})
			}
		}
	}
	results := make([]*Result, len(reqs))
	for i := range results {
		if i >= len(order) {
			results[i] = &Result{Error: fmt.Sprintf("nghttp did not send request (exit status %d)", exit)}
			continue
		}
		st := streams[order[i]]
		r := st.res
		if r.Error == "" && !st.ended {
			r.Error = fmt.Sprintf("stream did not complete (exit status %d)", exit)
			if connErr != "" {
				r.Error += "; " + connErr
			}
		}
		results[i] = r
	}
	return results, nil
}

// nghttpdServer is nghttpd, the nghttp2 server, serving static files.
type nghttpdServer struct {
	profile  string
	settings map[string]uint32
}

func (s *nghttpdServer) Name() string {
	if s.profile == "" {
		return "nghttpd"
	}
	return "nghttpd:" + s.profile
}
func (s *nghttpdServer) Images() []string { return []string{"cli"} }

func (s *nghttpdServer) Features() featureSet {
	// nghttpd serves static files from /work/htdocs, echoes uploads
	// with --echo-upload, and adds a trailer to every response.
	return feats("tls h2c route:hello route:bytes route:echo")
}

func (s *nghttpdServer) Start(ctx context.Context, tc *testCtx, mode string) (string, error) {
	d := tc.env.Docker
	_, addr, err := d.Shared("server:"+s.Name()+"/"+mode, func() (string, string, error) {
		port, err := freePort()
		if err != nil {
			return "", "", err
		}
		args := []string{"--entrypoint", "nghttpd", "--", "-d", "/work/htdocs", "--echo-upload",
			"--address=127.0.0.1"}
		for k, v := range s.settings {
			switch k {
			case "SETTINGS_MAX_CONCURRENT_STREAMS":
				args = append(args, fmt.Sprintf("--max-concurrent-streams=%d", v))
			case "SETTINGS_INITIAL_WINDOW_SIZE":
				// nghttpd takes the window size as a power of two.
				bits := 0
				for 1<<(bits+1) <= int(v)+1 && bits < 30 {
					bits++
				}
				args = append(args, fmt.Sprintf("--window-bits=%d", bits))
			case "SETTINGS_HEADER_TABLE_SIZE":
				args = append(args, fmt.Sprintf("--header-table-size=%d", v))
			}
		}
		if mode == "h2c" {
			args = append(args, "--no-tls", strconv.Itoa(port))
		} else {
			args = append(args, strconv.Itoa(port), "/work/key.pem", "/work/cert.pem")
		}
		id, err := d.Start(context.Background(), "cli", args...)
		if err != nil {
			return "", "", err
		}
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		if err := d.waitListening(ctx, id, addr, 30*time.Second); err != nil {
			return "", "", err
		}
		return id, addr, nil
	})
	return addr, err
}
