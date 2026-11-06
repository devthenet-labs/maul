// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentrun

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/envelope"
	"github.com/devthenet-labs/maul/internal/templates"
)

// cliExecCheck is a cli exec check under review.
var cliExecCheck = mustCheck(templates.ExampleCLIExecCheck)

// wsReviewItem is a review of c on a workspace, green at the head and red
// by assertion at the base.
func wsReviewItem(c checks.Check) CheckReviewItem {
	return CheckReviewItem{
		CheckDigest: checks.Digest(c), OutcomeDigest: reviewDigest("o", 1),
		Outcome: "`bbcount --bpm 120 kick.json` writes a WAV whose kick onsets are 500 ms apart.",
		Check:   string(checks.Canonical(c)),
		Head:    &ReviewRun{Result: "pass"},
		Base:    &ReviewRun{Result: "fail", Assertion: "marker onsets"},
	}
}

// wsReviewHandoff is a checkreview handoff on workspace w of items, edited
// by edit (nil for none).
func wsReviewHandoff(w *EvaluateWorkspace, items []CheckReviewItem, edit func(*CheckReviewHandoff)) string {
	h := CheckReviewHandoff{Reviews: items, Request: "Count beats at any BPM."}
	if w != nil {
		h.Workspace = &CheckReviewWorkspace{EvaluateWorkspace: *w}
	}
	if edit != nil {
		edit(&h)
	}
	raw, err := json.Marshal(h)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// TestCheckReviewWorkspaceFacts (S2-16): a workspace round's reviews carry
// maul's facts beside the untrusted program (the surface and its build, the
// check's runs at the head and the base), which the prompt states; a red run
// check's one program run is its failed step.
func TestCheckReviewWorkspaceFacts(t *testing.T) {
	red := wsReviewItem(onsetsCheck)
	red.OutcomeDigest = reviewDigest("o", 2)
	red.Expect, red.FailedStep, red.FailedAssertion = "fail", 1, "marker onsets"
	red.Head = &ReviewRun{Result: "fail", Assertion: "marker onsets"}
	red.Base = &ReviewRun{Result: "error", Cause: "compile"}
	tests := []struct {
		name  string
		item  CheckReviewItem
		edit  func(*CheckReviewHandoff)
		wants []string
	}{
		{"cli run, base red", wsReviewItem(onsetsCheck), nil, []string{
			"`go build -o /work/bin/ ./cmd/bbcount`", "failed by its assertion `marker onsets`",
		}},
		{"cli exec", wsReviewItem(cliExecCheck), nil, []string{"`/work/bin/bbcount`"}},
		{"red run", red, nil, []string{"could not build or run", "the program did not compile against the tree"}},
		{"base unbuildable", func() CheckReviewItem {
			it := wsReviewItem(onsetsCheck)
			it.Base = nil
			return it
		}(), func(h *CheckReviewHandoff) { h.Workspace.BaseUnbuildable = true }, []string{
			"The base did not build, so the check did not run there",
		}},
		{"base did not run", func() CheckReviewItem {
			it := wsReviewItem(onsetsCheck)
			it.Base = nil
			return it
		}(), nil, []string{"did not run"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, ws := roleConfig(t, PhaseCheckReview,
				wsReviewHandoff(cliWorkspace(), []CheckReviewItem{tt.item}, tt.edit), &out)
			brokeredClaude(t, &cfg)
			fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
				"scratch/review-1.md": reviewReport("proves"),
			}}}}
			ev := onlyEvent(t, cfg, fx, &out)
			if ev.CheckReview == nil || ev.CheckReview.Outcome != envelope.OutcomeOK ||
				len(ev.CheckReview.Answers) != 1 {
				t.Fatalf("event = %+v, want one answer", ev)
			}
			prompt := argvFlag(t, fx.specs[0].Argv, "-p")
			for _, want := range tt.wants {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt lacks %q:\n%s", want, prompt)
				}
			}
		})
	}
}

// TestCheckReviewWorkspaceLibrary: a library round reviews a run check.
func TestCheckReviewWorkspaceLibrary(t *testing.T) {
	var out bytes.Buffer
	c := mustCheck(templates.ExampleLibraryRunCheck)
	cfg, ws := roleConfig(t, PhaseCheckReview,
		wsReviewHandoff(libraryWorkspace(), []CheckReviewItem{wsReviewItem(c)}, nil), &out)
	fx := &fakeExec{steps: []step{{ws: ws, stdout: streamSuccess, writes: map[string]string{
		"scratch/review-1.md": reviewReport("proves"),
	}}}}
	if ev := onlyEvent(t, cfg, fx, &out); ev.CheckReview == nil || len(ev.CheckReview.Answers) != 1 {
		t.Fatalf("event = %+v, want one answer", ev)
	}
}

// TestCheckReviewWorkspaceRefused: a workspace review whose facts
// contradict themselves or the check is refused before any session.
func TestCheckReviewWorkspaceRefused(t *testing.T) {
	item := func(edit func(*CheckReviewItem)) []CheckReviewItem {
		it := wsReviewItem(onsetsCheck)
		edit(&it)
		return []CheckReviewItem{it}
	}
	ok := []CheckReviewItem{wsReviewItem(onsetsCheck)}
	tests := []struct {
		name, handoff, want string
	}{
		{"no head run", wsReviewHandoff(cliWorkspace(), item(func(it *CheckReviewItem) { it.Head = nil }), nil),
			"head"},
		{"green check failed at the head", wsReviewHandoff(cliWorkspace(), item(func(it *CheckReviewItem) {
			it.Head = &ReviewRun{Result: "fail", Assertion: "marker onsets"}
		}), nil), "head"},
		{"red check passed at the head", wsReviewHandoff(cliWorkspace(), item(func(it *CheckReviewItem) {
			it.Expect, it.FailedStep, it.FailedAssertion = "fail", 1, "marker onsets"
		}), nil), "head"},
		{"red at another assertion", wsReviewHandoff(cliWorkspace(), item(func(it *CheckReviewItem) {
			it.Expect, it.FailedStep, it.FailedAssertion = "fail", 1, "marker onsets"
			it.Head = &ReviewRun{Result: "fail", Assertion: "marker other"}
		}), nil), "head"},
		{"unknown result", wsReviewHandoff(cliWorkspace(), item(func(it *CheckReviewItem) {
			it.Base = &ReviewRun{Result: "maybe"}
		}), nil), "base"},
		{"assertion on a pass", wsReviewHandoff(cliWorkspace(), item(func(it *CheckReviewItem) {
			it.Head = &ReviewRun{Result: "pass", Assertion: "marker onsets"}
		}), nil), "head"},
		{"cause on a fail", wsReviewHandoff(cliWorkspace(), item(func(it *CheckReviewItem) {
			it.Base = &ReviewRun{Result: "fail", Assertion: "marker onsets", Cause: "compile"}
		}), nil), "base"},
		{"unknown cause", wsReviewHandoff(cliWorkspace(), item(func(it *CheckReviewItem) {
			it.Base = &ReviewRun{Result: "error", Cause: "timeout"}
		}), nil), "base"},
		{"assertion on two lines", wsReviewHandoff(cliWorkspace(), item(func(it *CheckReviewItem) {
			it.Base = &ReviewRun{Result: "fail", Assertion: "marker\nonsets"}
		}), nil), "base"},
		{"base run of an unbuildable base", wsReviewHandoff(cliWorkspace(), ok,
			func(h *CheckReviewHandoff) { h.Workspace.BaseUnbuildable = true }), "base"},
		{"runs without a workspace", wsReviewHandoff(nil, []CheckReviewItem{func() CheckReviewItem {
			it := CheckReviewItem{CheckDigest: checks.Digest(mustCheck(reviewCheck(0))),
				OutcomeDigest: reviewDigest("o", 0), Outcome: "It rates.", Check: reviewCheck(0),
				Head: &ReviewRun{Result: "pass"}}
			return it
		}()}, nil), "workspace"},
		{"http check on a workspace", wsReviewHandoff(cliWorkspace(), []CheckReviewItem{{
			CheckDigest: checks.Digest(mustCheck(reviewCheck(0))), OutcomeDigest: reviewDigest("o", 0),
			Outcome: "It rates.", Check: reviewCheck(0), Head: &ReviewRun{Result: "pass"},
		}}, nil), "form"},
		{"exec check on a library", wsReviewHandoff(libraryWorkspace(),
			[]CheckReviewItem{wsReviewItem(cliExecCheck)}, nil), "form"},
		{"components on a workspace", wsReviewHandoff(cliWorkspace(), ok, func(h *CheckReviewHandoff) {
			h.Components = []EvaluateComponent{{Key: "api", Path: "/api"}}
		}), "component"},
		{"invalid workspace", wsReviewHandoff(&EvaluateWorkspace{Surface: templates.SurfaceCLI}, ok, nil),
			"cli workspace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			cfg, _ := roleConfig(t, PhaseCheckReview, tt.handoff, &out)
			fx := &fakeExec{}
			err := New(cfg, fx).Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Run() error = %v, want %q", err, tt.want)
			}
			if len(fx.specs) != 0 {
				t.Errorf("commands run = %d, want none", len(fx.specs))
			}
		})
	}
}

// TestCheckStepsRunIsOneProgramRun: a run check, as an exec check, is one
// program run to a reviewer (wave 0 requirement: it was 0).
func TestCheckStepsRunIsOneProgramRun(t *testing.T) {
	for _, c := range []checks.Check{onsetsCheck, cliExecCheck, mustCheck(reviewCheck(0))} {
		if got := checkSteps(c); got != 1 {
			t.Errorf("checkSteps(%s) = %d, want 1", c.Form, got)
		}
	}
}
