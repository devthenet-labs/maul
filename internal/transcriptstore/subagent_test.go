// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package transcriptstore

import (
	"context"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/devthenet-labs/maul/internal/transcript"
)

// TestPersistKeepsCallAndParent: the stored record keeps which call each
// turn is and which call it ran under, so a finished run's subagent turns
// still nest under their Agent call.
func TestPersistKeepsCallAndParent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).Build()
	turns := []transcript.Turn{
		{Seq: 1, Role: transcript.RoleAssistant, Kind: transcript.KindToolUse, Tool: "Agent", Call: "t_agent"},
		{Seq: 2, Role: transcript.RoleAssistant, Kind: transcript.KindToolUse, Tool: "Bash", Call: "c_1",
			Parent: "t_agent"},
	}
	ref, err := Persist(context.Background(), c, "maul", nil, investigation("fnd-s-inv-1"), invGVK, turns)
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	got, err := Load(context.Background(), c, "maul", ref.Name)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 || got[0].Call != "t_agent" || got[1].Call != "c_1" || got[1].Parent != "t_agent" {
		t.Errorf("Load = %+v, want both turns' call and parent kept", got)
	}
}
