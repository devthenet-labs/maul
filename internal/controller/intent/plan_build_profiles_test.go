// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bitwise-media-group/patchy/internal/templates"
)

// TestPlanJobIsToldItsBuildsProfile: a plan Job is told, as plain text for
// its prompt, the network profile a build of its repository gets, the one
// the plan comment's advice names, so the planner does not call a module
// the build can fetch a risk (intents#29). The plan Job itself stays sealed:
// it asks for no profile, and carries neither the profile's label nor its
// proxy environment. Without a profile it is told nothing.
func TestPlanJobIsToldItsBuildsProfile(t *testing.T) {
	for _, tt := range []struct {
		name, repos string
		want        []templates.BuildProfile
	}{
		{"profile", "acme/app", []templates.BuildProfile{{URL: appRepoURL, Profile: "go"}}},
		{"no profile matches", "acme/other", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, testProject())
			withProfiles(t, e, tt.repos)
			e.awaiting()
			plans := e.specsOf("plan")
			if len(plans) == 0 {
				t.Fatal("no plan Job launched")
			}
			e.jobs.mu.Lock()
			defer e.jobs.mu.Unlock()
			for name, spec := range e.jobs.specs {
				if spec.Phase != "plan" {
					continue
				}
				if spec.NetworkProfile != nil {
					t.Errorf("plan Job %s asked for network profile %+v", name, spec.NetworkProfile)
				}
				env := e.jobs.envs[name]
				got, err := templates.DecodeBuildProfiles(env[envBuildProfiles])
				if err != nil || !reflect.DeepEqual(got, tt.want) {
					t.Errorf("plan Job %s was told %+v (%v), want %+v", name, got, err, tt.want)
				}
				for k := range env {
					if k != envBuildProfiles && strings.Contains(strings.ToUpper(k), "PROFILE") ||
						strings.Contains(strings.ToUpper(k), "PROXY") {
						t.Errorf("plan Job %s carries %s", name, k)
					}
				}
			}
		})
	}
}
