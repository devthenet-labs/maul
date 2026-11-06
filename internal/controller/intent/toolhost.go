// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The tool host port (SPEC-S2 section 1): one evaluation flow, two hosts.
// The round (gate, PreviewVerification, tool pod, evaluator, confirm,
// review, collect, Decide, hand-off) is the same whichever host runs the
// evaluator's hands; only where they run differs, and that is all a host
// says:
//
//   - slotHost (toolhost_slot.go, S1): the Intent's Preview, in its slot;
//     preview-controller renders the tool pod and writes the verification's
//     status. Its code paths are the ones S1 shipped, moved here unchanged.
//   - agentsHost (toolhost_agents.go, S2): a Project with a cli or library
//     surface and no Preview, under --intent-evaluator-agents; the PR's head
//     and its merge base are built in an agents-namespace tool pod that
//     intent-controller's agents host renders and observes.
//
// The intent reconciler picks the host per pass (pass.toolHost) and writes
// only the PreviewVerification's spec, never its status, for either.

// toolHost is where an Intent's evaluation rounds run their tools.
type toolHost interface {
	// placement is the PreviewVerification placement this host's rounds
	// carry.
	placement() v1alpha1.VerificationPlacement
	// verifies reports whether the pass's Intent can be verified on this
	// host at all, its Project verifying (Settings.verifiesProject).
	verifies(p *pass) bool
	// heads is the head set a round is about; ok is false while there is
	// none (no Preview derivable, no open pull request at a head yet).
	heads(p *pass) (headSet, bool)
	// gate is the host's part of the kept gate, once a round is due: what
	// must hold before its PreviewVerification is created. A nil place is
	// a round that does not start this pass (changed reports a status
	// write, which ends the pass); otherwise place completes the spec with
	// what this host runs it against, after the round's common fields.
	gate(ctx context.Context, p *pass, v *v1alpha1.IntentVerification, prs []v1alpha1.IntentPullRequest) (
		place func(*v1alpha1.PreviewVerificationSpec), changed bool, err error)
}

// toolHost is the host this pass's Intent is evaluated on: the agents host
// for a Project the agents tool pod evaluates (Settings.agentsProject), the
// slot otherwise.
func (p *pass) toolHost() toolHost {
	if p.set.agentsProject(p.proj) {
		return agentsHost{}
	}
	return slotHost{}
}

// agentsProject reports a Project the agents-namespace tool pod evaluates:
// it declares a cli or library surface (ProjectSurfaces.Agents), and the
// controller runs the evaluator with --intent-evaluator-agents.
func (s Settings) agentsProject(p *v1alpha1.Project) bool {
	return s.Evaluator && s.EvaluatorAgents && p.Spec.Surfaces.Agents()
}

// previewed reports a Project that sets a preview, on itself or on any of
// its repositories: one host per Project (O6), so a cli or library surface
// beside one is not evaluated (SurfaceCombinationUnsupported).
func previewed(p *v1alpha1.Project) bool {
	if p.Spec.Preview != nil {
		return true
	}
	for _, r := range p.Spec.Repositories {
		if r.Preview != nil {
			return true
		}
	}
	return false
}

// toolBuildTimeout is each tree's build timeout in Project p's agents tool
// pod: the cli surface's (CLISurface.Timeout: its default and ceiling), a
// library's the default, never over --intent-tool-build-timeout-max.
func (s Settings) toolBuildTimeout(p *v1alpha1.Project) time.Duration {
	t := v1alpha1.DefaultCLIBuildTimeout
	if sf := p.Spec.Surfaces; sf != nil && sf.CLI != nil {
		t = sf.CLI.Timeout()
	}
	if s.ToolBuildTimeoutMax > 0 && t > s.ToolBuildTimeoutMax {
		t = s.ToolBuildTimeoutMax
	}
	return t
}
