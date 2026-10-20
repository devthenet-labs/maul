// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// testFindingCommandsSchema writes a fully populated status.commands through
// the status subresource and reads it back, so a field the generated CRD
// does not know (and the API server would silently prune, losing a command
// the webhook recorded) fails here; and it pins the bounds the webhook
// handler and the projection rely on.
func testFindingCommandsSchema(ctx context.Context, t *testing.T, c client.Client) {
	t.Helper()
	f := &maulv1.Finding{
		ObjectMeta: metav1.ObjectMeta{Name: "finding-cmd-1", Namespace: "default"},
		Spec: maulv1.FindingSpec{
			IntegrationRef: maulv1.LocalObjectReference{Name: "gh"},
			Source:         "github-code-scanning",
			Advisories:     []string{"CVE-2026-0001"},
		},
	}
	if err := c.Create(ctx, f); err != nil {
		t.Fatalf("Create(finding) = %v", err)
	}
	at := metav1.NewTime(time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC))
	command := func(id int64) maulv1.FindingCommand {
		return maulv1.FindingCommand{
			CommentID: id,
			Actor:     maulv1.CommandActor{Login: "octo-cat", ID: 583231, Type: "User"},
			Verb:      "approve", Note: "ship it", Legacy: true, ReceivedAt: at,
			Outcome: maulv1.CommandUnavailable, DecidedAt: &at,
			Available: []string{"expedite", "suspend"}, Applied: false,
			Quiet: true, AnsweringSince: &at,
		}
	}
	superseded := command(13)
	superseded.Verb, superseded.Outcome = "suspend", maulv1.CommandSuperseded
	want := &maulv1.FindingCommands{
		Pending:       []maulv1.FindingCommand{command(12), superseded},
		Consumed:      []int64{3, 7},
		LastToggle:    14,
		RefusedActors: []int64{583231, 9},
	}
	f.Status.Commands = want.DeepCopy()
	if err := c.Status().Update(ctx, f); err != nil {
		t.Fatalf("Status().Update(commands) = %v, want nil", err)
	}
	got := &maulv1.Finding{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(f), got); err != nil {
		t.Fatalf("Get(finding) = %v", err)
	}
	if !equality.Semantic.DeepEqual(got.Status.Commands, want) {
		t.Errorf("status.commands = %+v, want %+v", got.Status.Commands, want)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*maulv1.FindingCommands)
	}{
		{"more pending than MaxPendingCommands", func(cs *maulv1.FindingCommands) {
			cs.Pending = nil
			for i := range maulv1.MaxPendingCommands + 1 {
				cs.Pending = append(cs.Pending, command(int64(100+i)))
			}
		}},
		{"more consumed than MaxConsumedCommands", func(cs *maulv1.FindingCommands) {
			cs.Consumed = nil
			for i := range maulv1.MaxConsumedCommands + 1 {
				cs.Consumed = append(cs.Consumed, int64(200+i))
			}
		}},
		{"more refused accounts than MaxRefusedActors", func(cs *maulv1.FindingCommands) {
			cs.RefusedActors = nil
			for i := range maulv1.MaxRefusedActors + 1 {
				cs.RefusedActors = append(cs.RefusedActors, int64(300+i))
			}
		}},
		{"a negative last toggle", func(cs *maulv1.FindingCommands) {
			cs.LastToggle = -1
		}},
		{"two pending commands for one comment", func(cs *maulv1.FindingCommands) {
			cs.Pending = append(cs.Pending, command(12))
		}},
		{"a verb that is not lower-case letters", func(cs *maulv1.FindingCommands) {
			cs.Pending[0].Verb = "<b>"
		}},
		{"a note over 1 KiB", func(cs *maulv1.FindingCommands) {
			cs.Pending[0].Note = strings.Repeat("x", 1025)
		}},
		{"an unknown outcome", func(cs *maulv1.FindingCommands) {
			cs.Pending[0].Outcome = "Maybe"
		}},
		{"a login that is not one", func(cs *maulv1.FindingCommands) {
			cs.Pending[0].Actor.Login = "not a login"
		}},
		{"a comment id of zero", func(cs *maulv1.FindingCommands) {
			cs.Pending[0].CommentID = 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := &maulv1.Finding{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(f), cur); err != nil {
				t.Fatalf("Get(finding) = %v", err)
			}
			tc.mutate(cur.Status.Commands)
			if err := c.Status().Update(ctx, cur); err == nil {
				t.Errorf("Status().Update(%s) = nil, want a schema rejection", tc.name)
			}
		})
	}
}
