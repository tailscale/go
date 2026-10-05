// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build h2interop

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http/internal/testcert"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed all:peers
var peersFS embed.FS

// containerLabel labels all containers started by h2interop, so that
// stale ones can be cleaned up.
const containerLabel = "org.golang.h2interop=1"

// ownerLabel records the PID of the h2interop process that started a
// container, so that concurrent runs don't remove each other's.
const ownerLabel = "org.golang.h2interop.pid"

// dockerEnv manages peer images and containers.
type dockerEnv struct {
	verbose bool
	// workDir is a host directory mounted read-only at /work in
	// every container. It holds the test certificate and static
	// files for static file servers.
	workDir string

	mu         sync.Mutex
	images     map[string]string // peer dir -> image tag
	containers map[string]bool
	shared     map[string]*sharedContainer
}

type sharedContainer struct {
	once sync.Once
	id   string
	addr string
	err  error
}

func newDockerEnv(verbose bool) (*dockerEnv, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker is required to run peer tests: %v", err)
	}
	if out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("docker not working: %v\n%s", err, out)
	}
	// Remove containers left over from previous runs that have exited.
	if out, err := exec.Command("docker", "ps", "-a", "--filter", "label="+containerLabel,
		"--format", `{{.ID}} {{.Label "`+ownerLabel+`"}}`).Output(); err == nil {
		var stale []string
		for line := range strings.Lines(string(out)) {
			f := strings.Fields(line)
			if len(f) == 0 {
				continue
			}
			pid := 0
			if len(f) > 1 {
				pid, _ = strconv.Atoi(f[1])
			}
			if pid <= 0 || syscall.Kill(pid, 0) == syscall.ESRCH {
				stale = append(stale, f[0])
			}
		}
		if len(stale) > 0 {
			exec.Command("docker", append([]string{"rm", "-f"}, stale...)...).Run()
		}
	}
	dir, err := os.MkdirTemp("", "h2interop-work-")
	if err != nil {
		return nil, err
	}
	d := &dockerEnv{
		verbose:    verbose,
		workDir:    dir,
		images:     map[string]string{},
		containers: map[string]bool{},
		shared:     map[string]*sharedContainer{},
	}
	if err := d.populateWorkDir(); err != nil {
		return nil, err
	}
	return d, nil
}

// populateWorkDir writes the test certificate and static files.
func (d *dockerEnv) populateWorkDir() error {
	files := map[string][]byte{
		"cert.pem":       testcert.LocalhostCert,
		"key.pem":        pkcs8PEM(testcert.LocalhostKey),
		"htdocs/hello":   []byte(helloBody),
		"htdocs/bytes/0": nil,
	}
	for _, n := range staticSizes {
		files["htdocs/bytes/"+strconv.Itoa(n)] = Pattern(n)
	}
	for name, data := range files {
		p := filepath.Join(d.workDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			return err
		}
	}
	return os.Chmod(d.workDir, 0o755)
}

// pkcs8PEM returns the PEM private key, relabeled as "PRIVATE KEY"
// if it contains a PKCS #8 key (testcert's key is PKCS #8 under an
// "RSA PRIVATE KEY" label, which some TLS stacks reject).
func pkcs8PEM(key []byte) []byte {
	b, _ := pem.Decode(key)
	if b == nil {
		return key
	}
	if _, err := x509.ParsePKCS8PrivateKey(b.Bytes); err != nil {
		return key
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b.Bytes})
}

// staticSizes are the sizes of /bytes/N files available to static
// file servers.
var staticSizes = func() []int {
	s := []int{1000, 100000, 1000000, 10000000}
	for i := range 200 {
		s = append(s, 1000+i)
	}
	return s
}()

// Cleanup removes all containers and the work directory.
func (d *dockerEnv) Cleanup() {
	d.mu.Lock()
	var ids []string
	for id := range d.containers {
		ids = append(ids, id)
	}
	d.containers = map[string]bool{}
	d.mu.Unlock()
	if len(ids) > 0 {
		exec.Command("docker", append([]string{"rm", "-f"}, ids...)...).Run()
	}
	os.RemoveAll(d.workDir)
}

// imageTag returns the tag for the image built from peers/<dir>,
// which includes a hash of the directory's contents.
func imageTag(dir string) (string, []byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	h := sha256.New()
	root := path.Join("peers", dir)
	err := fs.WalkDir(peersFS, root, func(p string, de fs.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		data, err := peersFS.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, root+"/")
		fmt.Fprintf(h, "%s %d\n", rel, len(data))
		h.Write(data)
		mode := int64(0o644)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: mode, Size: int64(len(data)), ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		return "", nil, err
	}
	if err := tw.Close(); err != nil {
		return "", nil, err
	}
	return "golang-h2interop-" + dir + ":" + hex.EncodeToString(h.Sum(nil))[:12], buf.Bytes(), nil
}

// BuildImages builds the images needed by tests.
func (d *dockerEnv) BuildImages(ctx context.Context, tests []*Test) error {
	var dirs []string
	for _, t := range tests {
		for _, img := range t.Images {
			if !slices.Contains(dirs, img) {
				dirs = append(dirs, img)
			}
		}
	}
	slices.Sort(dirs)
	var wg sync.WaitGroup
	errs := make([]error, len(dirs))
	sem := make(chan bool, 4)
	for i, dir := range dirs {
		wg.Go(func() {
			sem <- true
			defer func() { <-sem }()
			errs[i] = d.buildImage(ctx, dir)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *dockerEnv) buildImage(ctx context.Context, dir string) error {
	tag, tarData, err := imageTag(dir)
	if err != nil {
		return err
	}
	if exec.Command("docker", "image", "inspect", tag).Run() == nil {
		d.mu.Lock()
		d.images[dir] = tag
		d.mu.Unlock()
		return nil
	}
	log.Printf("building docker image %s (this may take a while the first time)", tag)
	start := time.Now()
	cmd := exec.CommandContext(ctx, "docker", "build", "--network=host", "-t", tag, "-")
	cmd.Stdin = bytes.NewReader(tarData)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building image for peers/%s: %v\n%s", dir, err, out.Bytes())
	}
	log.Printf("built %s in %v", tag, time.Since(start).Round(time.Second))
	// Remove older images of this peer. Removal fails harmlessly
	// for images in use by another run's containers.
	repo, _, _ := strings.Cut(tag, ":")
	if out, err := exec.Command("docker", "images", "--format", "{{.Repository}}:{{.Tag}}", repo).Output(); err == nil {
		for old := range strings.FieldsSeq(string(out)) {
			if old != tag {
				exec.Command("docker", "rmi", old).Run()
			}
		}
	}
	d.mu.Lock()
	d.images[dir] = tag
	d.mu.Unlock()
	return nil
}

func (d *dockerEnv) image(dir string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tag, ok := d.images[dir]
	if !ok {
		return "", fmt.Errorf("image for peers/%s not built", dir)
	}
	return tag, nil
}

// Start starts a detached container from the image for peers/<dir>
// with host networking and the work directory mounted at /work.
// Extra docker run options may precede "--" in args.
func (d *dockerEnv) Start(ctx context.Context, dir string, args ...string) (string, error) {
	tag, err := d.image(dir)
	if err != nil {
		return "", err
	}
	var opts []string
	if i := slices.Index(args, "--"); i >= 0 {
		opts, args = args[:i], args[i+1:]
	}
	runArgs := []string{"run", "-d", "--network=host", "--label", containerLabel,
		"--label", fmt.Sprintf("%s=%d", ownerLabel, os.Getpid()),
		"-v", d.workDir + ":/work:ro"}
	runArgs = append(runArgs, opts...)
	runArgs = append(runArgs, tag)
	runArgs = append(runArgs, args...)
	out, err := exec.CommandContext(ctx, "docker", runArgs...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker run %s: %v\n%s", tag, err, out)
	}
	id := strings.TrimSpace(string(out))
	if i := strings.LastIndexByte(id, '\n'); i >= 0 {
		id = id[i+1:]
	}
	d.mu.Lock()
	d.containers[id] = true
	d.mu.Unlock()
	return id, nil
}

// RunOnce runs a container from the image for peers/<dir> to
// completion, returning its combined output. The opts are extra
// docker run options.
func (d *dockerEnv) RunOnce(ctx context.Context, dir string, opts []string, args ...string) (string, error) {
	tag, err := d.image(dir)
	if err != nil {
		return "", err
	}
	runArgs := []string{"run", "--rm", "--network=host", "--label", containerLabel,
		"--label", fmt.Sprintf("%s=%d", ownerLabel, os.Getpid()),
		"-v", d.workDir + ":/work:ro"}
	runArgs = append(runArgs, opts...)
	runArgs = append(runArgs, tag)
	runArgs = append(runArgs, args...)
	out, err := exec.CommandContext(ctx, "docker", runArgs...).CombinedOutput()
	return string(out), err
}

// Stop removes a container.
func (d *dockerEnv) Stop(id string) {
	exec.Command("docker", "rm", "-f", id).Run()
	d.mu.Lock()
	delete(d.containers, id)
	d.mu.Unlock()
}

// Logs returns a container's output.
func (d *dockerEnv) Logs(id string) string {
	out, _ := exec.Command("docker", "logs", "--tail", "200", id).CombinedOutput()
	return string(out)
}

// Exec runs a command in a running container.
func (d *dockerEnv) Exec(ctx context.Context, id string, stdin []byte, args ...string) (stdout, stderr []byte, err error) {
	return d.ExecEnv(ctx, id, nil, stdin, args...)
}

// ExecEnv is like Exec, but sets environment variables ("K=V") for the command.
func (d *dockerEnv) ExecEnv(ctx context.Context, id string, env []string, stdin []byte, args ...string) (stdout, stderr []byte, err error) {
	execArgs := []string{"exec"}
	if stdin != nil {
		execArgs = append(execArgs, "-i")
	}
	for _, e := range env {
		execArgs = append(execArgs, "-e", e)
	}
	execArgs = append(execArgs, id)
	execArgs = append(execArgs, args...)
	cmd := exec.CommandContext(ctx, "docker", execArgs...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var o, e bytes.Buffer
	cmd.Stdout = &o
	cmd.Stderr = &e
	err = cmd.Run()
	return o.Bytes(), e.Bytes(), err
}

// Shared returns a long-running container shared between tests,
// identified by key, starting it with start if necessary.
func (d *dockerEnv) Shared(key string, start func() (id, addr string, err error)) (id, addr string, err error) {
	d.mu.Lock()
	sc := d.shared[key]
	if sc == nil {
		sc = &sharedContainer{}
		d.shared[key] = sc
	}
	d.mu.Unlock()
	sc.once.Do(func() {
		sc.id, sc.addr, sc.err = start()
	})
	return sc.id, sc.addr, sc.err
}

// ToolContainer returns a shared idle container from peers/<dir> in
// which commands can be run with Exec.
func (d *dockerEnv) ToolContainer(ctx context.Context, dir string) (string, error) {
	id, _, err := d.Shared("tool:"+dir, func() (string, string, error) {
		id, err := d.Start(context.Background(), dir, "--entrypoint", "sleep", "--", "infinity")
		return id, "", err
	})
	return id, err
}

// freePort returns a currently unused TCP port on 127.0.0.1.
//
// Ports for containers are chosen from below the kernel's usual
// ephemeral port range (32768 and up on Linux), from which the
// in-process listeners and dials get theirs, so that another listener
// doesn't take the port before the container binds it. Each process
// starts at a different offset to avoid colliding with concurrent runs.
func freePort() (int, error) {
	const lo, hi = 20000, 32000
	nextPortOnce.Do(func() { nextPort.Store(int32(os.Getpid() * 97 % (hi - lo))) })
	for range hi - lo {
		p := lo + int(nextPort.Add(1))%(hi-lo)
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			ln.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in [%d, %d)", lo, hi)
}

var (
	nextPortOnce sync.Once
	nextPort     atomic.Int32
)

// waitListening waits for a TCP listener at addr, checking that the
// container is still running.
func (d *dockerEnv) waitListening(ctx context.Context, id, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("server in container did not start listening on %s within %v; logs:\n%s", addr, timeout, d.Logs(id))
		}
		out, _ := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", id).Output()
		if strings.TrimSpace(string(out)) == "false" || len(out) == 0 {
			return fmt.Errorf("server container exited; logs:\n%s", d.Logs(id))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
