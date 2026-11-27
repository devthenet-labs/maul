// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

// The placeholders an agents round's exec and run calls may name, which the
// tools relay substitutes before the call runs.
const (
	// BinPlaceholder is the cli surface's built binary, a path under
	// /work/bin; only a cli workspace has one.
	BinPlaceholder = "{{bin}}"
	// TreePlaceholder is the writable tree the build ran in, /work/tree.
	TreePlaceholder = "{{tree}}"
)

// Viewport is a browser page's size in CSS pixels.
type Viewport struct {
	Width, Height int
}

// DefaultViewport is the fixed viewport every browser session opens at,
// 1280x800, so what one evaluator saw another sees at the same size, and
// the plan prompt can state the window outcomes about layout are seen in.
var DefaultViewport = Viewport{Width: 1280, Height: 800}
