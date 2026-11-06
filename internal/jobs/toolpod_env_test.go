// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"errors"
	"reflect"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// TestToolPodSandboxEnv: the environment the controller hands the relay is
// exactly what each sandbox is rendered with, with a profile and without,
// so a program the relay runs sees the build's network and nothing wider;
// a spec CreateToolPod would refuse is refused here too.
func TestToolPodSandboxEnv(t *testing.T) {
	for name, spec := range map[string]ToolPodSpec{
		"no profile":   toolPodSpec(),
		"go profile":   toolPodSpecWith(goProfile()),
		"tunnel":       toolPodSpecWith(tunnelProfile()),
		"go and hosts": toolPodSpecWith(goTunnelProfile()),
	} {
		t.Run(name, func(t *testing.T) {
			c := New(fake.NewClientset(), toolPodConfig(), nil)
			env, err := c.ToolPodSandboxEnv(spec)
			if err != nil {
				t.Fatalf("ToolPodSandboxEnv: %v", err)
			}
			job, _ := renderToolPod(t, toolPodConfig(), spec)
			for _, side := range toolSides {
				if sb := containerNamed(t, job.Spec.Template.Spec, "sandbox-"+side); !reflect.DeepEqual(sb.Env, env) {
					t.Errorf("sandbox-%s env\n%v\nwant the relay's\n%v", side, sb.Env, env)
				}
			}
		})
	}
	spec := toolPodSpec()
	spec.Data = nil // the relay's data is what the environment goes into
	if _, err := New(fake.NewClientset(), toolPodConfig(), nil).ToolPodSandboxEnv(spec); err != nil {
		t.Errorf("a spec with no data yet: %v", err)
	}
	spec.Key = "short"
	if _, err := New(fake.NewClientset(), toolPodConfig(), nil).ToolPodSandboxEnv(spec); !errors.Is(err,
		ErrToolPodRefused) {
		t.Errorf("a refused spec: %v, want ErrToolPodRefused", err)
	}
}
