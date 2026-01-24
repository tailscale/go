// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package githash

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
)

// GitHash is a git hash in hex form.
//
// It's usually a SHA-1 hash, but could be SHA-256 depending on the git
// configuration.
type GitHash string

// Enabled is whether git hash lookups are enabled via the CMD_GO_USE_GIT_HASH
// environment variable.
var Enabled bool

func init() {
	s := os.Getenv("CMD_GO_USE_GIT_HASH")
	if s != "" {
		Enabled, _ = strconv.ParseBool(s)
	}
}

// gitHashKey is the key used to look up possible files in
// a git repo that match the same base name & size.
//
// This is used to avoid statting all files in a git repo
// when trying to find the git hash for a given file.
// Instead, we only stat files that match on name & size.
type gitHashKey struct {
	baseName string // base name of file; as that's fs.FileInfo.Name gives us
	size     int64
}

type gitHashMap struct {
	gitRoot string // absolute path to git repo root

	// cands is a list of files in the git repo, bucketed by their (base name,
	// size) bucket key. This makes looking for a file faster later, without
	// statting the whole world, yet still permitting lookup only from a
	// fs.FileInfo that only has a base name & size & Sys info.
	cands map[gitHashKey][]*gitHashCand
}

type gitHashCand struct {
	rel  string // the relative git path from "git ls-files -r"
	hash GitHash

	statOnce sync.Once
	stat     fs.FileInfo
}

func (c *gitHashCand) getStat(m *gitHashMap) fs.FileInfo {
	c.statOnce.Do(func() {
		fullPath := path.Join(m.gitRoot, c.rel)
		info, err := os.Lstat(fullPath)
		if err == nil {
			c.stat = info
		}
	})
	return c.stat
}

var getGitHashMap = sync.OnceValue(buildGitHashMap)

func buildGitHashMap() *gitHashMap {
	m := &gitHashMap{
		cands: make(map[gitHashKey][]*gitHashCand),
	}
	gitRoot, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil
	}
	m.gitRoot = strings.TrimSpace(string(gitRoot))

	cmd := exec.Command("git", "ls-tree",
		"-r",     // recursive
		"--long", // include file sizes
		"-z",     // null-separated entries; don't have to deal with C quoting of some filenames
		"HEAD",
	)
	cmd.Dir = m.gitRoot // effectively git -C <dir>; either way.
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	// Parse lines of the form:
	//
	// 100644 blob cabbb1732c418125f9c773ce7a28ba34f2708554     639    .gitattributes
	// 100644 blob 2b4a5fccdaf12f98cf8e255affa28cfd7e6a784d      95    .github/CODE_OF_CONDUCT.md
	//
	// .... but null-terminated instead of newline-terminated, so we don't have to deal
	// with C quoting of filenames with certain characters.
	//
	// We don't care about the permissions.
	remain := out
	for len(remain) > 0 {
		line, rest, ok := bytes.Cut(remain, []byte{0})
		if !ok {
			break
		}
		remain = rest
		meta, nameB, ok := bytes.Cut(line, []byte("\t"))

		_, hashAndSize, ok := bytes.Cut(meta, []byte(" blob "))
		if !ok {
			continue
		}
		hashB, sizeB, ok := bytes.Cut(hashAndSize, []byte(" "))
		if !ok {
			continue
		}
		size, err := strconv.ParseInt(strings.TrimSpace(string(sizeB)), 10, 64)
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(nameB))
		hash := strings.TrimSpace(string(hashB))
		k := gitHashKey{
			baseName: path.Base(name),
			size:     size,
		}
		m.cands[k] = append(m.cands[k], &gitHashCand{
			rel:  name,
			hash: GitHash(hash),
		})
	}
	return m
}

// Hash returns the git hash for the given file info, if available.
func Hash(info fs.FileInfo) (GitHash, bool) {
	if !Enabled || info == nil || !info.Mode().IsRegular() {
		return "", false
	}
	k := gitHashKey{
		baseName: info.Name(),
		size:     info.Size(),
	}
	m := getGitHashMap()
	if m == nil {
		return "", false
	}
	for _, cand := range m.cands[k] {
		if os.SameFile(info, cand.getStat(m)) {
			return cand.hash, true
		}
	}
	return "", false
}

// ModTimeOrHash returns either the git hash (if enabled and available) or the
// mod time of the given file info.
//
// For non-regular files (notably directories), it returns nil if git hash is
// enabled.
//
// It always returns one of nil, time.Time, or GitHash (a string), all suitable
// for use in Sprintf verb %v.
func ModTimeOrHash(info fs.FileInfo) any {
	if !Enabled {
		return info.ModTime()
	}
	if h, ok := Hash(info); ok {
		return h
	}
	if info.Mode().IsRegular() {
		return info.ModTime()
	}
	return nil
}
