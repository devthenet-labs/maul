// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/report"
)

// intents#33's refusal, as its build agent quoted it.
const torchRefusal = "403 egress-proxy: download.pytorch.org is not in profile go's hosts"

// refusedReport is a not-built build report whose refused list is lines.
func refusedReport(lines ...string) string {
	var b strings.Builder
	b.WriteString("---\nsuccess: false\nsummary: \"Could not pin torch\"\ntests:\n  ran: false\n  passed: false\n" +
		"notes: []\nreason: \"torch==2.5.1+cpu needs download.pytorch.org, which the proxy refuses\"\n")
	if len(lines) > 0 {
		b.WriteString("refused:\n")
		for _, l := range lines {
			b.WriteString("  - \"" + l + "\"\n")
		}
	}
	b.WriteString("---\n\nThe proxy refused the host.\n")
	return b.String()
}

// notBuilt plans well and reports every build not built with md.
func notBuilt(md string) func(jobs.Spec) jobs.RunOutput {
	return func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "plan" {
			return defaultOutput(spec)
		}
		return jobs.RunOutput{Events: []envelope.Event{{V: envelope.Version, Type: envelope.TypeRemediation,
			Remediation: &envelope.Remediation{Stage: envelope.Stage{Outcome: envelope.OutcomeOK,
				Usage: envelope.Usage{CostUSD: 0.1}}, ReportMarkdown: md}}}}
	}
}

// TestRefusedDependencyEndsTheBuild (intents#33): a build whose agent
// reports the egress proxy's policy refusal of a dependency the plan needs
// spends exactly one attempt. The intent fails at once, with the trigger
// label removed, and the run's detail, shown on the issue, names the
// refused host, the profile and what the operator changes.
func TestRefusedDependencyEndsTheBuild(t *testing.T) {
	e := newEnv(t, testProject())
	withProfiles(t, e, "acme/app")
	e.jobs.output = notBuilt(refusedReport(torchRefusal))
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentFailed, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageBuild)
	if len(runs) != 1 {
		t.Fatalf("build runs = %d, want exactly one attempt", len(runs))
	}
	run := runs[0]
	if run.Status.Phase != v1alpha1.RunFailed || run.Status.Outcome != OutcomeDependencyRefused {
		t.Fatalf("run = %s %s: %s, want Failed %s", run.Status.Phase, run.Status.Outcome, run.Status.Detail,
			OutcomeDependencyRefused)
	}
	for _, want := range []string{torchRefusal, "network profile go",
		"add download.pytorch.org to network profile go's hosts", "trigger label again"} {
		if !strings.Contains(run.Status.Detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, run.Status.Detail)
		}
	}
	if e.gh.hasLabel("patchy:target") || in.Status.CompletedAt == nil {
		t.Errorf("failed intent: trigger label present %v, completedAt %v", e.gh.hasLabel("patchy:target"),
			in.Status.CompletedAt)
	}
	e.mustIntent(name)
	st := e.gh.withMarker("patchy:intent")
	if len(st) != 1 || !strings.Contains(st[0].Body, OutcomeDependencyRefused) ||
		!strings.Contains(st[0].Body, "download.pytorch.org") {
		t.Errorf("status comment does not explain the refusal: %+v", st)
	}
}

// TestBuildFailuresThatStillRetry: only a policy refusal in the report's
// refused list ends the build at one attempt. A crash, a not-built report
// without refused, one quoting only a transient refusal, a policy line from
// a run that carried no network profile, and a reason that merely mentions
// the proxy all retry as before.
func TestBuildFailuresThatStillRetry(t *testing.T) {
	for _, tt := range []struct {
		name    string
		output  func(jobs.Spec) jobs.RunOutput
		profile bool
		outcome string
	}{
		{"a crash", failingBuild, true, string(envelope.OutcomeRuntimeError)},
		{"not built, no refused", notBuilt(refusedReport()), true, OutcomeNotBuilt},
		{"a transient refusal", notBuilt(refusedReport("429 egress-proxy: over the request limit (rate); retry later")),
			true, OutcomeNotBuilt},
		{"no network profile", notBuilt(refusedReport(torchRefusal)), false, OutcomeNotBuilt},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			if tt.profile {
				withProfiles(t, e, "acme/app")
			}
			e.jobs.output = tt.output
			name := e.awaiting()
			e.gh.label(1, "patchy:approved", approver)
			e.drive(name, v1alpha1.IntentFailed, repoImage)
			runs := e.runsOf(name, v1alpha1.IntentStageBuild)
			if len(runs) != 2 {
				t.Fatalf("build runs = %d, want both attempts", len(runs))
			}
			for _, r := range runs {
				if r.Status.Outcome != tt.outcome {
					t.Errorf("run %s outcome %s (%s), want %s", r.Name, r.Status.Outcome, r.Status.Detail, tt.outcome)
				}
			}
		})
	}
}

// TestRefusedDependencyEndsTheReviseRound: a revise round whose agent
// reports a policy refusal ends after its one attempt, back in review.
func TestRefusedDependencyEndsTheReviseRound(t *testing.T) {
	e := newEnv(t, testProject())
	withProfiles(t, e, "acme/app")
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	number := in.Status.PullRequests[0].Number
	e.jobs.output = notBuilt(refusedReport(torchRefusal))
	e.gh.reviews[number] = []ghclient.Review{{ID: 991, NodeID: "review-991", Author: actorOf(approver),
		State: "CHANGES_REQUESTED", Body: "Please pin torch.", SubmittedAt: e.clock.Now()}}
	e.clock.Advance(3 * time.Minute)
	e.drive(name, v1alpha1.IntentRevising, repoImage)
	e.drive(name, v1alpha1.IntentInReview, repoImage)
	runs := e.runsOf(name, v1alpha1.IntentStageRevise)
	if len(runs) != 1 || runs[0].Status.Outcome != OutcomeDependencyRefused {
		t.Fatalf("revise runs = %+v, want one %s", runs, OutcomeDependencyRefused)
	}
}

// TestDependencyRefusedDetail: the detail quotes only the policy lines, at
// most maxRefusedQuoted of them, names each refused host once and says
// what the operator changes for each kind of refusal.
func TestDependencyRefusedDetail(t *testing.T) {
	parse := func(lines ...string) *report.Build {
		b, err := report.ParseBuild([]byte(refusedReport(lines...)))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	transient := "502 egress-proxy: upstream unreachable (dial)"
	detail, ok := dependencyRefused(parse(transient, torchRefusal, torchRefusal,
		"403 egress-proxy: project torch is not allowed by the grant of registry pypi",
		"403 egress-proxy: github.com is never tunnelled (a forge, model API, control plane, internal or publish host)"),
		"py")
	if !ok {
		t.Fatal("not read as a refusal")
	}
	for _, want := range []string{"network profile py", "add download.pytorch.org to network profile py's hosts",
		"registry grant", "another source", "(and 1 more)"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "upstream unreachable") || strings.Count(detail, "download.pytorch.org, ") != 0 {
		t.Errorf("detail quotes a transient line or repeats a host:\n%s", detail)
	}
	if len(detail) > 4096 {
		t.Errorf("detail is %d bytes, over the run's bound", len(detail))
	}
	if _, ok := dependencyRefused(parse(transient), "py"); ok {
		t.Error("a transient refusal alone was read as a policy refusal")
	}
	if _, ok := dependencyRefused(parse(), "py"); ok {
		t.Error("a report without refused was read as a refusal")
	}
}
