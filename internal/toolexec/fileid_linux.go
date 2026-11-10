// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"io/fs"
	"syscall"
)

// idOf is info's inode and change time.
func idOf(info fs.FileInfo) (fileID, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, false
	}
	return fileID{ino: st.Ino, ctime: st.Ctim.Nano()}, true
}
