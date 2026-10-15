// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/envelope"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/templates"
)

const baseGoMod = `module example.com/app

go 1.26

require (
	github.com/google/uuid v1.5.0
	golang.org/x/text v0.20.0
)
`

const builtGoMod = `module example.com/app

go 1.26

require (
	github.com/google/uuid v1.6.0
	golang.org/x/mod v0.42.0
	golang.org/x/text v0.20.0
	golang.org/x/tools v0.30.0 // indirect
)
`

// TestPullRequestBodyFlagsGoDependencyDrift is the live gate's regression
// (intents#29): the approved plan listed golang.org/x/mod v0.28.0, the build
// shipped v0.42.0, and nothing told the reviewer. The pull request's body
// now lists each requirement the build added or changed in go.mod against
// its base, and marks each one not as the plan named it.
func TestPullRequestBodyFlagsGoDependencyDrift(t *testing.T) {
	e := newEnv(t, testProject())
	e.gh.files[baseSHA] = map[string]string{goModPath: baseGoMod}
	e.jobs.output = func(spec jobs.Spec) jobs.RunOutput {
		if spec.Phase == "plan" {
			return planOutput(envelope.Stage{Outcome: envelope.OutcomeOK}, strings.Replace(validPlan,
				"new_dependencies: []", "new_dependencies:\n  - \"golang.org/x/mod v0.28.0\"", 1))
		}
		out := defaultOutput(spec)
		cs := out.Events[0].Remediation.Changeset
		cs.Upserts = append(cs.Upserts, envelope.FileChange{Path: goModPath, Mode: "100644",
			ContentB64: base64.StdEncoding.EncodeToString([]byte(builtGoMod))})
		return out
	}
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.gh.mu.Lock()
	body := e.gh.prs[in.Status.PullRequests[0].Number].body
	e.gh.mu.Unlock()
	for _, want := range []string{
		"2 of them differ from the approved plan's new dependencies",
		"- `github.com/google/uuid` `v1.6.0`: changed from `v1.5.0`; **not in the plan**",
		"- `golang.org/x/mod` `v0.42.0`: added; **the plan named `v0.28.0`**",
		"- `golang.org/x/tools` `v0.30.0`: added (indirect)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the pull request body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "golang.org/x/text") {
		t.Errorf("the pull request body lists a requirement the build did not change:\n%s", body)
	}
}

// TestPullRequestBodyWithoutGoModIsUnchanged: a build of a repository with
// no go.mod opens its pull request with the body it always had.
func TestPullRequestBodyWithoutGoModIsUnchanged(t *testing.T) {
	e := newEnv(t, testProject())
	name := e.awaiting()
	e.gh.label(1, "patchy:approved", approver)
	in := e.drive(name, v1alpha1.IntentInReview, repoImage)
	e.gh.mu.Lock()
	body := e.gh.prs[in.Status.PullRequests[0].Number].body
	e.gh.mu.Unlock()
	if strings.Contains(body, "go.mod") {
		t.Errorf("the pull request body accounts for a go.mod no tree holds:\n%s", body)
	}
}

func TestGoRequirementChanges(t *testing.T) {
	planned := []string{"golang.org/x/mod@v0.42.0", "github.com/google/uuid (any tagged release)", "libvips 8.15"}
	got, err := goRequirementChanges([]byte(baseGoMod), []byte(builtGoMod), planned)
	if err != nil {
		t.Fatal(err)
	}
	want := []templates.GoRequirement{
		{Path: "github.com/google/uuid", Version: "v1.6.0", Was: "v1.5.0", InPlan: true},
		{Path: "golang.org/x/mod", Version: "v0.42.0", InPlan: true, PlanVersion: "v0.42.0"},
		{Path: "golang.org/x/tools", Version: "v0.30.0", Indirect: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changes = %+v\nwant %+v", got, want)
	}
	if got, err := goRequirementChanges(nil, []byte(baseGoMod), nil); err != nil || len(got) != 2 || got[0].Was != "" {
		t.Errorf("a new go.mod's changes = %+v, %v; want both requirements added", got, err)
	}
	if got, err := goRequirementChanges([]byte(baseGoMod), []byte(baseGoMod), planned); err != nil || got != nil {
		t.Errorf("an unchanged go.mod's changes = %+v, %v; want none", got, err)
	}
	if _, err := goRequirementChanges([]byte(baseGoMod), []byte("require (\n"), nil); err == nil {
		t.Error("a go.mod that does not parse compared without an error")
	}
}
