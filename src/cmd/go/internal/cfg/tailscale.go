// Copyright 2026 Tailscale Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cfg

func init() {
	BuildContext.ToolTags = append(BuildContext.ToolTags, "tailscale_go")
}
