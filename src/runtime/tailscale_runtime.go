package runtime

// TailscaleCurrentP returns the runtime's currently executing 'p' ID.
//
// See https://github.com/tailscale/go/issues/109.
func TailscaleCurrentP() int {
	return int(getg().m.p.ptr().id)
}
