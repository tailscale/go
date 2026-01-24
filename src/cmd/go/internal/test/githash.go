// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package test

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

type gitHash string // hex blog hash from git (probably SHA-1, but not necessarily)

var useGitHash = sync.OnceValue(func() bool {
	s := os.Getenv("CMD_GO_USE_GIT_HASH")
	if s == "" {
		return false
	}
	v, _ := strconv.ParseBool(s)
	return v
})

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
	hash gitHash

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
			hash: gitHash(hash),
		})
	}
	return m
}

func getGitHash(info fs.FileInfo) (gitHash, bool) {
	if !useGitHash() || info == nil || !info.Mode().IsRegular() {
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
