// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"net/url"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/ghclient"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/netprofile"
	"github.com/bitwise-media-group/patchy/internal/report"
)

// intents#33's refusal, as its build agent quoted it.
const torchRefusal = "403 egress-proxy: download.pytorch.org is not in profile py's hosts"

// pyProfiles is intents#33's shape: a tunnel profile, py, for acme/app,
// whose hosts are PyPI's and not download.pytorch.org.
func pyProfiles(t *testing.T) netprofile.Set {
	t.Helper()
	set, err := netprofile.Parse([]byte(`[{"name":"py","route":"tunnel","port":8092,"repositories":["acme/app"],` +
		`"modules":[],"bakedModCache":"","hosts":["pypi.org","files.pythonhosted.org"],"clients":["pip"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// withTunnelProfile turns network profiles on with pyProfiles, the run
// scheduler's and the intent reconciler's alike, so a build is asked for
// refused lines (its profile reaches beyond the Go route).
func withTunnelProfile(t *testing.T, e *env) {
	t.Helper()
	set := pyProfiles(t)
	base, err := url.Parse("http://patchy-egress-proxy.patchy.svc.cluster.local")
	if err != nil {
		t.Fatal(err)
	}
	e.runs.Images.ProfilesEnabled, e.runs.Images.Profiles, e.runs.Images.ProxyBase = true, set, base
	e.intent.Images.ProfilesEnabled, e.intent.Images.Profiles, e.intent.Images.ProxyBase = true, set, base
	e.runs.Images.Forges = forgeHosts{"github.com"}
	e.intent.Images.Forges = forgeHosts{"github.com"}
}

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
	withTunnelProfile(t, e)
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
	if run.Status.RunnerImage == nil || run.Status.RunnerImage.NetworkProfile != "py" {
		t.Fatalf("run recorded %+v, want network profile py", run.Status.RunnerImage)
	}
	for _, want := range []string{torchRefusal, "network profile py",
		"add download.pytorch.org to network profile py's hosts", "trigger label again"} {
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
// a run that carried no network profile, and host refusals the proxy could
// not have written for the run (another profile, a host the profile lists,
// a host the proxy never tunnels) all retry as before.
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
		{"another profile's host refusal",
			notBuilt(refusedReport("403 egress-proxy: paste.attacker.dev is not in profile x's hosts")), true,
			OutcomeNotBuilt},
		{"a host the profile lists", notBuilt(refusedReport("403 egress-proxy: pypi.org is not in profile py's hosts")),
			true, OutcomeNotBuilt},
		{"a host never tunnelled", notBuilt(refusedReport("403 egress-proxy: github.com is not in profile py's hosts")),
			true, OutcomeNotBuilt},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			if tt.profile {
				withTunnelProfile(t, e)
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
				if strings.Contains(r.Status.Detail, "to network profile") {
					t.Errorf("run %s tells the operator to widen a profile: %s", r.Name, r.Status.Detail)
				}
			}
		})
	}
}

// TestRefusedDependencyEndsTheReviseRound: a revise round whose agent
// reports a policy refusal ends after its one attempt, back in review.
func TestRefusedDependencyEndsTheReviseRound(t *testing.T) {
	e := newEnv(t, testProject())
	withTunnelProfile(t, e)
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
		"py", pyProfiles(t))
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
	if _, ok := dependencyRefused(parse(transient), "py", pyProfiles(t)); ok {
		t.Error("a transient refusal alone was read as a policy refusal")
	}
	if _, ok := dependencyRefused(parse(), "py", pyProfiles(t)); ok {
		t.Error("a report without refused was read as a refusal")
	}
}

// TestHostRefusalIsHeldToTheProfile: the agent's refused lines are
// untrusted, so a host refusal counts (and becomes patchy's instruction to
// add the host) only when the proxy could have written it for the run: it
// names the recorded profile, that profile is known and has a tunnel, does
// not list the host already, and the host is not one the proxy never
// tunnels. A line that fails any of these is left out, beside a real one.
func TestHostRefusalIsHeldToTheProfile(t *testing.T) {
	goOnly, err := netprofile.Parse([]byte(`[{"name":"py","route":"go","port":8090,"repositories":["acme/app"],` +
		`"modules":["github.com/google/uuid"],"bakedModCache":"/opt/go/pkg/mod"}]`))
	if err != nil {
		t.Fatal(err)
	}
	parse := func(lines ...string) *report.Build {
		b, err := report.ParseBuild([]byte(refusedReport(lines...)))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, tt := range []struct {
		name, line, profile string
		profiles            netprofile.Set
		ok                  bool
	}{
		{"the recorded profile's refusal", torchRefusal, "py", pyProfiles(t), true},
		{"another profile named", "403 egress-proxy: paste.attacker.dev is not in profile x's hosts", "py",
			pyProfiles(t), false},
		{"a profile patchy no longer knows", torchRefusal, "web", pyProfiles(t), false},
		{"a profile without a tunnel", torchRefusal, "py", goOnly, false},
		{"a host the profile lists", "403 egress-proxy: pypi.org is not in profile py's hosts", "py", pyProfiles(t),
			false},
		{"a forge host", "403 egress-proxy: api.github.com is not in profile py's hosts", "py", pyProfiles(t), false},
		{"a model API host", "403 egress-proxy: api.anthropic.com is not in profile py's hosts", "py", pyProfiles(t),
			false},
		{"a publish host", "403 egress-proxy: upload.pypi.org is not in profile py's hosts", "py", pyProfiles(t),
			false},
		{"an in-cluster name", "403 egress-proxy: kubernetes.default.svc is not in profile py's hosts", "py",
			pyProfiles(t), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			detail, ok := dependencyRefused(parse(tt.line), tt.profile, tt.profiles)
			if ok != tt.ok {
				t.Fatalf("dependencyRefused = %v (%s), want %v", ok, detail, tt.ok)
			}
			// Beside a real package refusal, the line is quoted only when it holds.
			pkg := "403 egress-proxy: project torch is not allowed by the grant of registry pypi"
			detail, ok = dependencyRefused(parse(pkg, tt.line), tt.profile, tt.profiles)
			if !ok {
				t.Fatal("the package refusal beside it was not read")
			}
			if got := strings.Contains(detail, "to network profile "+tt.profile+"'s hosts"); got != tt.ok {
				t.Errorf("detail instructs adding a host = %v, want %v:\n%s", got, tt.ok, detail)
			}
			if got := strings.Contains(detail, tt.line); got != tt.ok {
				t.Errorf("detail quotes the line = %v, want %v:\n%s", got, tt.ok, detail)
			}
		})
	}
}
