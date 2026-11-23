// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"testing"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/resourceclass"
)

// TestProjectStatusBoundsFitTheResourceClassMenu holds the Project status
// schema's bounds on status.admits.resourceClasses to the menu they publish.
// The api package cannot import the menu, so it repeats the two figures; if
// the menu ever grew past them, a controller with a full menu would have its
// Project status write refused by the schema, and with it Ready.
func TestProjectStatusBoundsFitTheResourceClassMenu(t *testing.T) {
	if v1alpha1.MaxAdmittedResourceClasses != resourceclass.MaxClasses {
		t.Errorf("MaxAdmittedResourceClasses = %d, want resourceclass.MaxClasses %d",
			v1alpha1.MaxAdmittedResourceClasses, resourceclass.MaxClasses)
	}
	if v1alpha1.MaxResourceClassNameLength != resourceclass.MaxNameLength {
		t.Errorf("MaxResourceClassNameLength = %d, want resourceclass.MaxNameLength %d",
			v1alpha1.MaxResourceClassNameLength, resourceclass.MaxNameLength)
	}
}
