// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"reflect"
	"testing"
)

func TestClassifyEnvironment(t *testing.T) {
	ready := func(name, role string) PartFact { return PartFact{Name: name, Role: role, Ready: true} }
	waiting := func(name, role, reason string) PartFact { return PartFact{Name: name, Role: role, Waiting: reason} }
	app := EnvAppFailure
	cases := []struct {
		name  string
		parts []PartFact
		want  EnvClass
		app   []string
	}{
		{"empty", nil, EnvOK, nil},
		{"all ready", []PartFact{ready("api", RoleComponent), ready("tester", RoleTester)}, EnvOK, nil},
		{"restarted but ready", []PartFact{{Name: "api", Role: RoleComponent, Ready: true, Restarts: 2}}, EnvOK, nil},
		{"tester finished", []PartFact{
			ready("api", RoleComponent),
			{Name: "tester", Role: RoleTester, LastTermination: &Termination{Reason: "Completed"}},
		}, EnvOK, nil},
		{"crashloop", []PartFact{
			{Name: "api", Role: RoleComponent, Waiting: "CrashLoopBackOff"}, ready("web", RoleComponent),
		}, EnvAppFailure, []string{"api"}},
		// Not Ready with no evidence of the application's (a node going
		// NotReady, a component missing) is the cluster's: the application
		// can at most make itself not-Ready, which earns an honest Infra and
		// then Unavailable, never a pass.
		{"not ready, no evidence", []PartFact{{Name: "api", Role: RoleComponent}}, EnvInfra, nil},
		{"head image pull", []PartFact{waiting("web", RoleComponent, "ImagePullBackOff")}, app, []string{"web"}},
		{"err image pull", []PartFact{waiting("web", RoleComponent, "ErrImagePull")}, app, []string{"web"}},
		{"evicted", []PartFact{{Name: "api", Role: RoleComponent, Ready: true, Evicted: true}}, app, []string{"api"}},
		// Only what happened during the attempt counts: running out of
		// memory then is the application's; a recovered OOM from before it
		// is evidence, not a failure.
		{"oom during", []PartFact{{Name: "api", Role: RoleComponent, Ready: true, CrashedDuring: true,
			LastTermination: &Termination{Reason: "OOMKilled", ExitCode: 137}}}, EnvAppFailure, []string{"api"}},
		{"oom before", []PartFact{{Name: "api", Role: RoleComponent, Ready: true,
			LastTermination: &Termination{Reason: "OOMKilled", ExitCode: 137}}}, EnvOK, nil},
		// A crash from before the attempt that the component recovered from
		// is evidence, not a failure.
		{"crashed before and back", []PartFact{{Name: "api", Role: RoleComponent, Ready: true, Restarts: 3,
			LastTermination: &Termination{Reason: "Error", ExitCode: 2}}}, EnvOK, nil},
		// A component that exited during the attempt and came back Ready
		// crashed: the application's failure, however it exited.
		{"crashed during and back", []PartFact{{Name: "api", Role: RoleComponent, Ready: true, Restarts: 4,
			CrashedDuring: true, LastTermination: &Termination{Reason: "Error", ExitCode: 2}}}, app, []string{"api"}},
		{"exited cleanly during", []PartFact{{Name: "api", Role: RoleComponent, Ready: true, Restarts: 1,
			CrashedDuring: true, LastTermination: &Termination{Reason: "Completed"}}}, app, []string{"api"}},
		// A component disrupted during the attempt by the cluster (an
		// Eviction API call, a drain, preemption, node loss, a replacement
		// with no cause of the application's) is maul's: the application
		// holds no API credential to cause it.
		{"disrupted during", []PartFact{{Name: "api", Role: RoleComponent, Ready: true, Disrupted: true}}, EnvInfra,
			nil},
		// The facts never carry both (componentCause picks one), but if
		// they did, the application's unambiguous evidence wins.
		{"disrupted and crashed", []PartFact{{Name: "api", Role: RoleComponent, Ready: true, Disrupted: true,
			CrashedDuring: true}}, app, []string{"api"}},
		// A Ready component the tester could not connect to for its whole
		// readiness window is the application's failure, never maul's Infra.
		{"unreachable while ready", []PartFact{{Name: "api", Role: RoleComponent, Ready: true,
			UnreachableForMs: 60000}, ready("web", RoleComponent)}, app, []string{"api"}},
		// Unreachable is the application's only while Ready and undisrupted:
		// otherwise the not-Ready or disruption cause wins.
		{"unreachable, not ready", []PartFact{{Name: "api", Role: RoleComponent, UnreachableForMs: 60000}},
			EnvInfra, nil},
		{"unreachable, disrupted", []PartFact{{Name: "api", Role: RoleComponent, Ready: true, Disrupted: true,
			UnreachableForMs: 60000}}, EnvInfra, nil},
		{"tester crashed", []PartFact{ready("api", RoleComponent), {Name: "tester", Role: RoleTester,
			LastTermination: &Termination{Reason: "Error", ExitCode: 2}}}, EnvInfra, nil},
		{"two failing in order", []PartFact{
			{Name: "web", Role: RoleComponent, Waiting: "CrashLoopBackOff"},
			{Name: "api", Role: RoleComponent, Ready: true, CrashedDuring: true},
		}, EnvAppFailure, []string{"web", "api"}},
		{"command image pull", []PartFact{waiting("api-cmd", RoleCommand, "ErrImagePull")}, app, []string{"api-cmd"}},
		{"component unschedulable", []PartFact{{Name: "api", Role: RoleComponent, Unschedulable: true}}, EnvInfra, nil},
		{"tester unschedulable", []PartFact{
			ready("api", RoleComponent), {Name: "tester", Role: RoleTester, Unschedulable: true},
		}, EnvInfra, nil},
		{"tester image pull", []PartFact{
			ready("api", RoleComponent), {Name: "tester", Role: RoleTester, Waiting: "ImagePullBackOff"},
		}, EnvInfra, nil},
		{"tester evicted", []PartFact{
			ready("api", RoleComponent), {Name: "tester", Role: RoleTester, Evicted: true},
		}, EnvInfra, nil},
		{"tester oom", []PartFact{ready("api", RoleComponent), {Name: "tester", Role: RoleTester,
			LastTermination: &Termination{Reason: "OOMKilled", ExitCode: 137}}}, EnvInfra, nil},
		{"service crash", []PartFact{waiting("db", RoleService, "CrashLoopBackOff")}, EnvInfra, nil},
		{"seed failed", []PartFact{{Name: "seed", Role: RoleSeed,
			LastTermination: &Termination{Reason: "Error", ExitCode: 1}}}, EnvInfra, nil},
		{"unknown role", []PartFact{{Name: "x", Role: "sidecar"}}, EnvInfra, nil},
		// The evaluator's slot tool pod is judged like the tester: by how it
		// ran, never as the application.
		{"tools completed", []PartFact{ready("api", RoleComponent), {Name: "tools", Role: RoleTools,
			LastTermination: &Termination{Reason: "Completed"}}}, EnvOK, nil},
		{"tools crashed", []PartFact{ready("api", RoleComponent), {Name: "tools", Role: RoleTools,
			LastTermination: &Termination{Reason: "Error", ExitCode: 1}}}, EnvInfra, nil},
		{"tools image pull", []PartFact{
			ready("api", RoleComponent), {Name: "tools", Role: RoleTools, Waiting: "ImagePullBackOff"},
		}, EnvInfra, nil},
		{"infra wins over app", []PartFact{
			{Name: "api", Role: RoleComponent, Waiting: "CrashLoopBackOff"},
			{Name: "tester", Role: RoleTester, Unschedulable: true},
		}, EnvInfra, []string{"api"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyEnvironment(c.parts); got != c.want {
				t.Errorf("ClassifyEnvironment = %s, want %s", got, c.want)
			}
			if got := AppFailureParts(c.parts); !reflect.DeepEqual(got, c.app) {
				t.Errorf("AppFailureParts = %v, want %v", got, c.app)
			}
		})
	}
}
