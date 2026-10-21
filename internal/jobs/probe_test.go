// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"slices"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/sandboxprobe"
)

// probeLine is the last line of a prepare script: the probe invocation
// every agent Job ends its trusted init with.
func probeLine(t *testing.T, script string) string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(script), "\n")
	return lines[len(lines)-1]
}

// TestEveryJobProbesItsSandbox: whatever the finding or intent kind, phase, harness, credential
// channel, DNS mode or trees, a Job's prepare init ends with a sandbox
// probe before the agent container may start. A repository-image Job runs
// the strict probe, exactly as before (its exit 78 refuses the run); every
// default-image Job runs the wait probe against the two targets every chart
// posture blocks, which can delay the agent but never refuse it. Evaluation
// Jobs run neither (TestEvalJobRunsNoProbe).
func TestEveryJobProbesItsSandbox(t *testing.T) {
	stages := []struct{ kind, phase string }{
		{"investigation", "investigate"},
		{"remediation", "remediate"},
		{"intent", "plan"},
		{"intent", "build"},
	}
	strict := toolsBinDir + "/" + agentRunnerBin + " " + sandboxprobe.Command
	wait := strict + " " + waitProbeArgs
	type shape struct {
		name   string
		cfg    func() Config
		spec   func(Spec) Spec
		repoOK bool // the shape runs the repository image
		plan   bool // the shape exists only for a plan Job (trees)
	}
	shapes := []shape{
		{"default claude, Secret credential", testConfig, func(s Spec) Spec { return s }, false, false},
		{"default codex", testConfig, func(s Spec) Spec { s.Harness, s.Model = "codex", "openai/gpt-5-codex"; return s },
			false, false},
		{"default brokered claude", brokeredConfig, func(s Spec) Spec { return s }, false, false},
		{"brokered claude, dns none", func() Config { return dnsNoneConfig(clusterResolver()) },
			func(s Spec) Spec { return s }, false, false},
		{"brokered claude with trees", brokeredConfig, func(s Spec) Spec {
			trees := treesSpec()
			s.RepoKey, s.RepoURL, s.Trees = trees.RepoKey, trees.RepoURL, trees.Trees
			return s
		}, false, true},
		{"declared image with the kill switch on", func() Config {
			c := injectedConfig()
			c.AllowRepositoryImages = false
			return c
		}, func(s Spec) Spec { s.RunnerImage = repoImage; s.RunnerSearchPath = repoSearchPath; return s },
			false, false},
		{"repository image", injectedConfig, func(s Spec) Spec {
			s.RunnerImage = repoImage
			s.RunnerSearchPath = repoSearchPath
			return s
		}, true, false},
	}
	for _, st := range stages {
		for _, sh := range shapes {
			if sh.plan && st.phase != "plan" {
				continue
			}
			t.Run(st.phase+"/"+sh.name, func(t *testing.T) {
				spec := testSpec()
				spec.Kind, spec.Phase = st.kind, st.phase
				spec = sh.spec(spec)
				job, err := New(fake.NewClientset(), sh.cfg(), nil).build(context.Background(),
					NameFor(spec.Finding, spec.Kind, int32(spec.Attempt)), spec)
				if err != nil {
					t.Fatalf("build: %v", err)
				}
				prepare := container(t, job, initContainerName)
				script := prepare.Command[2]
				line := probeLine(t, script)
				repo := job.Annotations[annotationRunnerImageSource] == v1alpha1.RunnerImageSourceRepository
				if repo != sh.repoOK {
					t.Fatalf("runs the repository image = %v, want %v", repo, sh.repoOK)
				}
				if repo {
					if line != strict {
						t.Errorf("repository-image probe = %q, want the strict %q", line, strict)
					}
					if strings.Contains(script, "--mode") {
						t.Errorf("repository-image prepare names a probe mode:\n%s", script)
					}
				} else {
					if !strings.HasPrefix(line, wait+" ||") {
						t.Errorf("default-image probe = %q, want the wait probe %q", line, wait)
					}
					if strings.Count(script, sandboxprobe.Command) != 1 {
						t.Errorf("default-image prepare runs the probe more than once:\n%s", script)
					}
				}
				if _, ok := envMap(prepare)[sandboxprobe.TimeoutEnv]; !ok {
					t.Errorf("prepare does not carry %s", sandboxprobe.TimeoutEnv)
				}
			})
		}
	}
}

// TestEvalJobRunsNoProbe: an evaluation Job is the one kind outside
// TestEveryJobProbesItsSandbox. Its prepare init runs in the evolve-runner
// image, which ships no agent-runner, so a probe line there would only fail
// (or, behind the wait mode's guard, be echoed away as an exit 127) while
// the docs went on claiming the window was closed. Adding a probe to it
// must come with an agent-runner in that image and an update to
// docs/deployment/isolation.md, not silently.
func TestEvalJobRunsNoProbe(t *testing.T) {
	job, err := New(fake.NewClientset(), testConfig(), nil).buildEvalJob(EvalNameFor("unit"), testEvalSpec())
	if err != nil {
		t.Fatalf("buildEvalJob: %v", err)
	}
	for _, c := range append(job.Spec.Template.Spec.InitContainers, job.Spec.Template.Spec.Containers...) {
		for _, arg := range append(append([]string{}, c.Command...), c.Args...) {
			if strings.Contains(arg, sandboxprobe.Command) {
				t.Errorf("evaluation container %q runs the sandbox probe:\n%s", c.Name, arg)
			}
		}
	}
}

// TestWaitProbeArgs pins the default Job's probe arguments to the
// subcommand's own vocabulary, so the two sides of the pod boundary agree:
// the wait mode, and only the targets every chart posture blocks — never
// the public internet, which broadEgress opens on purpose.
func TestWaitProbeArgs(t *testing.T) {
	want := []string{"--mode=" + string(sandboxprobe.ModeWait),
		"--targets=" + sandboxprobe.TargetKubernetes + "," + sandboxprobe.TargetMetadata}
	if got := strings.Fields(waitProbeArgs); !slices.Equal(got, want) {
		t.Errorf("waitProbeArgs = %q, want %q", got, want)
	}
	if strings.Contains(waitProbeArgs, sandboxprobe.TargetInternet) {
		t.Errorf("waitProbeArgs %q names the public internet", waitProbeArgs)
	}
}

// TestWaitProbeNeverFailsTheInit: the default Job's probe line is guarded,
// so neither a probe error nor an older agent-runner that ignores the
// arguments (and would exit 78 under broad egress) can fail the prepare
// init, consume an attempt or trip the breaker. It reports the exit status
// and moves on.
func TestWaitProbeNeverFailsTheInit(t *testing.T) {
	line := probeLine(t, buildJobForTest(t, brokeredConfig(), testSpec()).Spec.Template.Spec.InitContainers[0].Command[2])
	_, guard, ok := strings.Cut(line, " || ")
	if !ok || !strings.HasPrefix(guard, "echo ") || !strings.Contains(guard, "$?") {
		t.Errorf("wait probe line %q is not guarded by an echo of its exit status", line)
	}
}
