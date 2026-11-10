// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

//go:build !linux

package toolexec

import "io/fs"

// idOf reports no identity off Linux, so every cache entry is removed
// before a check run: slower, never a different answer.
func idOf(fs.FileInfo) (fileID, bool) { return fileID{}, false }
