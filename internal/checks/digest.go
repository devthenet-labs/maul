// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package checks

import (
	"crypto/sha256"
	"encoding/hex"
)

// Digest binds a check: "sha256:" and the hex sha256 of Canonical(c), so
// the same check digests alike however its keys were ordered or its
// defaults spelled. A dry run, a confirmation and a review are each
// recorded against it.
func Digest(c Check) string {
	sum := sha256.Sum256(Canonical(c))
	return "sha256:" + hex.EncodeToString(sum[:])
}
