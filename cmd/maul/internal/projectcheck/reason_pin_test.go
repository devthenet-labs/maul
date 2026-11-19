// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"testing"

	"github.com/devthenet-labs/maul/internal/controller/intent"
)

// TestReasonPinnedToIntentController: the forge line's wording keys on the
// Ready reason intent-controller writes for an unreadable Forge Secret.
func TestReasonPinnedToIntentController(t *testing.T) {
	if reasonForgeSecretUnreadable != intent.ReasonForgeSecretUnreadable {
		t.Errorf("reasonForgeSecretUnreadable = %q, intent-controller writes %q",
			reasonForgeSecretUnreadable, intent.ReasonForgeSecretUnreadable)
	}
}
