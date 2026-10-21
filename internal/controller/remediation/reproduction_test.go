// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/templates"
)

// TestRemediationReproduction: what the agent reported about reproducing
// the bug reaches the pull request body — stated plainly, or as the warning
// that maul could not reproduce it — and is recorded on the Remediation's
// status, where an agent that said nothing (an older pod, a report without
// the block) leaves nothing. A reproduction claimed on maul's default
// image, which can run none of the repository's tests, still gets the
// warning in the body; the status keeps the agent's account as reported.
func TestRemediationReproduction(t *testing.T) {
	for _, tt := range []struct {
		name         string
		defaultImage bool
		repr         *envelope.Reproduction
		wantWarning  bool
	}{
		{"reproduced", false, &envelope.Reproduction{Reproduced: true, Command: "go test ./...",
			Before: "--- FAIL: TestEscapes", After: "ok"}, false},
		{"not reproduced", false, &envelope.Reproduction{Reproduced: false}, true},
		{"not reported", false, nil, true},
		{"reproduced on the default image", true, &envelope.Reproduction{Reproduced: true, Command: "go test ./...",
			Before: "--- FAIL: TestEscapes", After: "ok"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			events := crdRemediationEvent(true)
			events[0].Remediation.Reproduction = tt.repr
			fw := &fakeForge{}
			objs := withImage(acceptedImage(), v1alpha1.RunnerImageSourceRepository)
			if tt.defaultImage {
				objs = runningRemediation()
			}
			r, c := newRemediation(t, &fakeCRRunner{done: true, events: events}, fw, objs...)
			remOnce(t, r, "finding-aa-1-rem-1")

			if len(fw.bodies) != 1 {
				t.Fatalf("PR bodies = %d, want 1", len(fw.bodies))
			}
			body := fw.bodies[0]
			if got := strings.Contains(body, templates.NotReproducedMarker); got != tt.wantWarning {
				t.Errorf("not-reproduced warning present = %v, want %v:\n%s", got, tt.wantWarning, body)
			}
			if tt.repr != nil && tt.repr.Reproduced && !strings.Contains(body, "--- FAIL: TestEscapes") {
				t.Errorf("body does not quote the failing run:\n%s", body)
			}

			rem := remediationNow(t, c)
			switch got := rem.Status.Reproduction; {
			case tt.repr == nil && got != nil:
				t.Errorf("status reproduction = %+v, want none", got)
			case tt.repr != nil && (got == nil || got.Reproduced != tt.repr.Reproduced || got.Before != tt.repr.Before):
				t.Errorf("status reproduction = %+v, want %+v", got, tt.repr)
			}
		})
	}
}

// TestRemediationHandOffRecordsReproduction: an agent that reproduced the
// bug but could not fix it safely still has its reproduction recorded, so
// the human it is handed to sees the failing test.
func TestRemediationHandOffRecordsReproduction(t *testing.T) {
	events := crdRemediationEvent(false)
	events[0].Remediation.Changeset = nil
	events[0].Remediation.Reproduction = &envelope.Reproduction{Reproduced: true, Command: "go test ./...",
		Before: "FAIL", After: "FAIL"}
	r, c := newRemediation(t, &fakeCRRunner{done: true, events: events}, &fakeForge{}, runningRemediation()...)
	remOnce(t, r, "finding-aa-1-rem-1")
	if got := remediationNow(t, c).Status.Reproduction; got == nil || !got.Reproduced || got.Command != "go test ./..." {
		t.Errorf("status reproduction = %+v, want the reported one", got)
	}
}

func remediationNow(t *testing.T, c client.Client) *v1alpha1.Remediation {
	t.Helper()
	var rem v1alpha1.Remediation
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "maul", Name: "finding-aa-1-rem-1"}, &rem); err != nil {
		t.Fatalf("Get remediation: %v", err)
	}
	return &rem
}
