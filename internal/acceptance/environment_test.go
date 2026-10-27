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
		{"never ready", []PartFact{{Name: "api", Role: RoleComponent}}, EnvAppFailure, []string{"api"}},
		{"head image pull", []PartFact{waiting("web", RoleComponent, "ImagePullBackOff")}, app, []string{"web"}},
		{"err image pull", []PartFact{waiting("web", RoleComponent, "ErrImagePull")}, app, []string{"web"}},
		{"evicted", []PartFact{{Name: "api", Role: RoleComponent, Ready: true, Evicted: true}}, app, []string{"api"}},
		{"oom", []PartFact{{Name: "api", Role: RoleComponent, Ready: true,
			LastTermination: &Termination{Reason: "OOMKilled", ExitCode: 137}}}, EnvAppFailure, []string{"api"}},
		// A crash the component recovered from is evidence, not a failure;
		// running out of memory is charged even once recovered.
		{"crashed and back", []PartFact{{Name: "api", Role: RoleComponent, Ready: true,
			LastTermination: &Termination{Reason: "Error", ExitCode: 2}}}, EnvOK, nil},
		{"tester crashed", []PartFact{ready("api", RoleComponent), {Name: "tester", Role: RoleTester,
			LastTermination: &Termination{Reason: "Error", ExitCode: 2}}}, EnvInfra, nil},
		{"two failing in order", []PartFact{
			{Name: "web", Role: RoleComponent, Waiting: "CrashLoopBackOff"},
			{Name: "api", Role: RoleComponent},
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
