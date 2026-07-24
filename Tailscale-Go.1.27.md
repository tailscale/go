# Tailscale Go 1.27 fork preparation

Status report for rebasing Tailscale's patches onto upstream Go 1.27.

- **New branch:** `tailscale.go1.27`, based on upstream
  `refs/heads/release-branch.go1.27` (at `go1.27rc2`, upstream commit
  `b323620c2f91`).
- **Source branch:** `tailscale.go1.26` (21 carried patches vs. its
  upstream merge-base, `go1.26.5`).
- **Result:** 13 patches cherry-picked; 8 dropped because they are
  already upstream in Go 1.27. Every cherry-picked commit has
  `[tailscale]` in its title and an `Updates tailscale/go#nnn`
  reference.

## Patches cherry-picked onto tailscale.go1.27

| Patch | Issue | Notes |
|---|---|---|
| runtime: add func TailscaleCurrentP | [#109](https://github.com/tailscale/go/issues/109) | clean pick |
| runtime: add TailscaleNumTimers for metrics | [#141](https://github.com/tailscale/go/issues/141) | clean pick; issue is missing the `tsgo` label (has only `need-upstream`, `ts26`) |
| cmd/link: add opt-in mechanism to fail if reflect is used | [#115](https://github.com/tailscale/go/issues/115) | clean pick |
| os: disable pidfd on Android | [#99](https://github.com/tailscale/go/issues/99) | clean pick; golang/go#70508 still unresolved for our supported Android versions |
| runtime/debug: embed Tailscale toolchain git rev | [#49](https://github.com/tailscale/go/issues/49) | clean pick |
| net: add TCP socket creation/close hooks to SockTrace API | [#58](https://github.com/tailscale/go/issues/58) | conflict: upstream `newFD` in `net/sock_posix.go` no longer returns an error; resolved |
| net, net/http: add enforcement hooks | [#55](https://github.com/tailscale/go/issues/55) | clean pick |
| net/http: add SetResponseWriteEnforcer | [#55](https://github.com/tailscale/go/issues/55) | **ported**: upstream deleted `h2_bundle.go` in favor of the new `net/http/internal/http2` package. The HTTP/2 hook now lives there as `http2.ResponseWriteEnforcer`, wired up via a new `net/http/tailscale_http2.go` (build-tagged `!nethttpomithttp2`) |
| .github: add Tailscale .github files | [#47](https://github.com/tailscale/go/issues/47) | branch references in `.github/workflows/build.yml` updated from `tailscale.go1.26` to `tailscale.go1.27` |
| cmd/go/internal/test: add opt-in file hashing instead of modtime for test caching (w/ git) | [#150](https://github.com/tailscale/go/issues/150) | clean pick |
| cmd/go: add -cachebinary build flag | [#149](https://github.com/tailscale/go/issues/149) | **replaced**: instead of re-picking the 1.26 branch's `go test -cachelink` commit, this picks patchset 4 of upstream [CL 739161](https://go-review.googlesource.com/c/go/+/739161), where the flag was renamed to `-cachebinary` and generalized to a build flag (default true for `go run`, false elsewhere) |
| cmd/go: set "tailscale_go" build tag | [#164](https://github.com/tailscale/go/issues/164) | clean pick |
| cmd/go: include tailscale.toolchain.rev in build cache salt | [#166](https://github.com/tailscale/go/issues/166) | clean pick |

Housekeeping suggestion: add a `ts27` label to the 13 issues above,
matching the `ts24`/`ts26` convention.

## Patches dropped (already upstream in Go 1.27) — issues to close

| Patch | Upstream commit in 1.27 | Issue | Action |
|---|---|---|---|
| runtime: tolerate vendor suffixes in Linux kernel release strings | `6e808ba41a` | [#162](https://github.com/tailscale/go/issues/162) | **close** (open, already labeled `upstreamed`) |
| runtime: use uname version check for 64-bit time on 32-bit arch codepaths | `04dc12c1a1` | [#162](https://github.com/tailscale/go/issues/162) | same issue as above |
| runtime: fix value of ENOSYS on mips from 38 to 89 | `c918cbd556` | [#160](https://github.com/tailscale/go/issues/160) | **close** (open, labeled `upstreamed`) |
| internal/poll: move rsan to heap on windows | `e2ce40125f` | [#158](https://github.com/tailscale/go/issues/158) | **close** (open, labeled `upstreamed`) |
| net: don't wait 5 seconds to re-read /etc/resolv.conf | `6de7a19fea` (+ follow-up `2433a3f2d6`) | [#93](https://github.com/tailscale/go/issues/93) | **close** (open, labeled `upstreamed`) |
| cmd/go/internal/modfetch: quiet read-only filesystem stat cache warnings | `21c9de8c1d` | none | no issue was ever filed (see audit); upstreamed, nothing to do |
| syscall: define no-op Errno type on plan9 | `b5c2bd7e05` | none | no issue was ever filed (see audit); upstreamed, nothing to do |
| syscall: make plan9 Errno implement the error interface | `76ebf63307` | none | no issue was ever filed (see audit); upstreamed, nothing to do |

Note: these patches were all absorbed into upstream by `go1.26.5`
already, so they contributed no diff between `tailscale.go1.26` and
its merge-base; dropping them for 1.27 loses nothing.

## Audit: tailscale.go1.26 vs upstream release-branch.go1.26

Method: diffed `tailscale.go1.26` against its merge-base with upstream
`release-branch.go1.26` (`go1.26.5`, commit `c19862e5f8`) and checked
that every changed file is attributable to one of the 21 carried patch
commits.

- **Result: clean.** All 36 changed files map to known patch commits.
  No accidental/undocumented changes were smuggled in via merge
  commits.
- **Findings — patches carried without a tailscale/go tracking issue**
  (the process gap the audit was looking for):
  1. `cmd/go/internal/modfetch: quiet read-only filesystem stat cache warnings`
  2. `syscall: define no-op Errno type on plan9`
  3. `syscall: make plan9 Errno implement the error interface`

  All three were Tailscale-authored upstream CLs cherry-picked into
  the fork while they waited for upstream review, and all three are in
  Go 1.27, so no new issues are needed — but future interim
  cherry-picks should get a tracking issue at merge time.
- Minor label housekeeping: [#141](https://github.com/tailscale/go/issues/141)
  lacks the `tsgo` label.

## Validation

- `./make.bash` succeeds on `tailscale.go1.27` (linux/amd64,
  bootstrapped with go1.26.3).
- **tsgotest** (`github.com/tailscale/tsgotest`): all tests pass
  against the new toolchain (with the
  `TAILSCALE_GIT_REV_TO_BE_REPLACED_AT_BUILD_TIME` placeholder
  substituted, as CI does).
- In-tree `go test net/http -run 'Tailscale|Enforcer|ResponseWrite'`
  passes, including the h1 and h2 subtests of
  `TestSetResponseWriteEnforcer`, which exercises the ported HTTP/2
  hook.
- `go test -short runtime/debug syscall net net/http` passes.
- **tsgotest expanded** with two new tests:
  - `TestIssue55_ResponseWriteEnforcerHTTP2`: the response write
    enforcer on the HTTP/2 server path, which is a new distinct code
    path after the 1.27 h2_bundle removal.
  - `TestIssue58_NetSockTraceTCP`: the `DidCreateTCPConn` /
    `WillCloseTCPConn` SockTrace hooks, whose wiring in
    `net/sock_posix.go` was the site of a 1.27 merge conflict.

## Follow-ups

- [ ] Close issues [#162](https://github.com/tailscale/go/issues/162),
      [#160](https://github.com/tailscale/go/issues/160),
      [#158](https://github.com/tailscale/go/issues/158),
      [#93](https://github.com/tailscale/go/issues/93) (all upstreamed
      in Go 1.27).
- [ ] Add `ts27` labels to the 13 carried issues; add `tsgo` label to
      [#141](https://github.com/tailscale/go/issues/141).
- [ ] Push `tailscale.go1.27` and the tsgotest additions after review.
- [ ] Re-merge upstream `release-branch.go1.27` when go1.27.0 final is
      tagged (branch is currently at go1.27rc2).
