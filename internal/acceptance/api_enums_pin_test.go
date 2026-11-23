// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"testing"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/httpstep"
)

// TestAPIEnumsMatchAcceptance pins the constants this package keeps beside
// their v1alpha1 twins (api/ never imports internal/), so a build part and
// the build-failed reason cross a status write unchanged.
func TestAPIEnumsMatchAcceptance(t *testing.T) {
	for _, c := range []struct{ name, acc, crd string }{
		{"RoleBuild", httpstep.RoleBuild, string(maulv1.PreviewRoleBuild)},
		{"ReasonBuildFailed", ReasonBuildFailed, string(maulv1.VerificationReasonToolsBuildFailed)},
	} {
		if c.acc != c.crd {
			t.Errorf("%s: acceptance %q, v1alpha1 %q", c.name, c.acc, c.crd)
		}
	}
}
