// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

// The preview environment derivation, beside DesiredPreviewComponents and
// shared the same way: intent-controller's preview-source reconciler writes
// Preview spec.environment from it, and preview-controller re-derives it
// before rendering anything, so the writer and the check cannot drift. It
// reads only operator Project configuration and the components derived from
// the Intent's recorded state; no plan, issue or agent text widens an edge.

// DesiredPreviewEnvironment derives an Intent's Preview environment from its
// Project, and reports false when the Preview has none: when the Project
// does not verify (spec.verification.enabled), or when the Intent has no
// Preview at all (DesiredPreviewComponents). The environment marks the
// Preview verification-capable and admits one edge from the tester to each
// component, on the component's own port, in component order. Nothing calls
// the tester, and no component calls another. A component named like the
// tester part or the evaluator's tool pod part (PreviewRoleTools), which the
// Project schema refuses while verification is on, gives no environment, so
// a Project the API server never validated fails closed.
func DesiredPreviewEnvironment(p *Project, in *Intent) (*PreviewEnvironment, bool) {
	if !p.Spec.VerificationEnabled() {
		return nil, false
	}
	components, ok := DesiredPreviewComponents(p, in)
	if !ok {
		return nil, false
	}
	for _, c := range components {
		if c.Name == PreviewPartTester || c.Name == PreviewRoleTools {
			return nil, false
		}
	}
	env := &PreviewEnvironment{Verification: true, Edges: make([]PreviewEdge, 0, len(components))}
	for _, c := range components {
		env.Edges = append(env.Edges, PreviewEdge{From: PreviewPartTester, To: c.Name, Port: c.Port})
	}
	return env, true
}
