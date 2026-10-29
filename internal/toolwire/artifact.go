// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import "strings"

// MaxArtifactBytes bounds one artifact's bytes as served to maul-tool: an
// entry's Artifact.Bytes bound.
const MaxArtifactBytes = MaxEvidenceBytes * 4

// ArtifactPath is the tools container's endpoint serving an artifact's
// bytes by its sha256: GET /v1/artifacts/<sha256> (DESIGN 2.7: screenshots
// are written by tools and copied to the evaluator's /work/scratch/shots/
// by maul-tool). It is "" for anything but a lowercase hex sha256. The
// reader holds the bytes to the digest and to the entry's Bytes.
func ArtifactPath(digest string) string {
	if !isSHA256(digest) {
		return ""
	}
	return "/v1/artifacts/" + digest
}

// IsScreenshot reports an artifact maul-tool copies to the evaluator: a
// PNG image, by name. Other artifacts (snapshot files) stay in tools; their
// text is already the inline output.
func IsScreenshot(a Artifact) bool {
	name := strings.ToLower(a.Name)
	return len(name) > len(".png") && strings.HasSuffix(name, ".png")
}
