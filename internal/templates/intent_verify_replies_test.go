// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/command"
)

// TestVerifyNoticeGoldens pins the replies to /maul verify and /maul accept.
func TestVerifyNoticeGoldens(t *testing.T) {
	base := func(o VerifyOutcome, verb string) VerifyCommandNotice {
		return VerifyCommandNotice{Namespace: "maul", Intent: "target-1", Key: "comment-4430", Actor: "peter",
			Verb: verb, Outcome: o}
	}
	tests := []struct {
		name string
		n    VerifyCommandNotice
	}{
		{"intent_notice_verify_requested.md", base(VerifyRequested, "verify")},
		{"intent_notice_verify_requested_fix.md", func() VerifyCommandNotice {
			n := base(VerifyRequested, "verify")
			n.Fix, n.Joined = true, true
			return n
		}()},
		{"intent_notice_verify_requested_reset.md", func() VerifyCommandNotice {
			n := base(VerifyRequested, "verify")
			n.Reset, n.AfterRound = true, true
			return n
		}()},
		{"intent_notice_verify_reset_too_soon.md", func() VerifyCommandNotice {
			n := base(VerifyResetTooSoon, "verify")
			n.ResetWait = "5 minutes"
			return n
		}()},
		{"intent_notice_verify_accepted.md", func() VerifyCommandNotice {
			n := base(VerifyAccepted, "accept")
			n.Criterion, n.Heads = "sounds-good", []string{"0123abc", "4567def"}
			return n
		}()},
		{"intent_notice_verify_accept_unknown.md", func() VerifyCommandNotice {
			n := base(VerifyAcceptUnknown, "accept")
			n.Criterion, n.Acceptable = "nope", []string{"sounds-good", "render"}
			return n
		}()},
		{"intent_notice_verify_accept_automatic.md", func() VerifyCommandNotice {
			n := base(VerifyAcceptAutomatic, "accept")
			n.Criterion = "render"
			return n
		}()},
		{"intent_notice_verify_accept_not_yet.md", func() VerifyCommandNotice {
			n := base(VerifyAcceptNotYet, "accept")
			n.Criterion = "sounds-good"
			return n
		}()},
		{"intent_notice_verify_accept_moved.md", func() VerifyCommandNotice {
			n := base(VerifyAcceptMoved, "accept")
			n.Criterion, n.Heads = "sounds-good", []string{"0123abc", "4567def"}
			return n
		}()},
		{"intent_notice_verify_applied.md", base(VerifyApplied, "verify")},
		{"intent_notice_verify_usage_pr.md", func() VerifyCommandNotice {
			n := base(VerifyUsage, "verify")
			n.Surface = command.IntentPR
			return n
		}()},
		{"intent_notice_verify_accepted_all.md", func() VerifyCommandNotice {
			n := base(VerifyAccepted, "accept")
			n.Criterion, n.Heads, n.NoCriteria = "all", []string{"0123abc"}, true
			return n
		}()},
		{"intent_notice_verify_stopped.md", func() VerifyCommandNotice {
			n := base(VerifyStopped, "verify")
			n.Cancelled = true
			return n
		}()},
		{"intent_notice_verify_stopped_running.md", func() VerifyCommandNotice {
			n := base(VerifyStopped, "verify")
			n.Surface, n.Running = command.IntentPR, true
			return n
		}()},
		{"intent_notice_verify_cap_reached.md", func() VerifyCommandNotice {
			n := base(VerifyCapReached, "verify")
			n.MaxVerifications = 20
			return n
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderVerifyCommandNotice(tt.n)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			golden(t, tt.name, got)
		})
	}
}

// TestVerifyNoticeNamesNobody: the actor is code, never a mention, and an
// unknown outcome is an error rather than an empty reply.
func TestVerifyNoticeNamesNobody(t *testing.T) {
	got, err := RenderVerifyCommandNotice(VerifyCommandNotice{Namespace: "maul", Intent: "target-1",
		Key: "comment-1", Actor: "@octocat", Verb: "verify", Outcome: VerifyRequested})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "`@octocat`") {
		t.Errorf("the actor is not code:\n%s", got)
	}
	if _, err := RenderVerifyCommandNotice(VerifyCommandNotice{Verb: "verify", Outcome: "bogus"}); err == nil {
		t.Error("an unknown outcome rendered")
	}
}
