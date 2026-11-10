// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/templates"
)

// verifyComments are maul's verification comments of round on the issue.
func (e *env) verifyComments(name string, round int32) []*ghclient.Comment {
	marker := templates.NoticeMarker(testNS, name, templates.VerifyKey(round))
	var out []*ghclient.Comment
	for _, c := range e.gh.ownComments() {
		if markerOf(c.Body) == marker {
			out = append(out, c)
		}
	}
	return out
}
