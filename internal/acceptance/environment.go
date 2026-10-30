// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

// EnvClass sorts a verification's environment.
type EnvClass string

const (
	// EnvOK: no part shows a failure.
	EnvOK EnvClass = "OK"
	// EnvAppFailure: a part the change under test runs (a component, or a
	// command Job in a component's image) crashed, was evicted by the
	// kubelet, ran out of memory or could not pull its PR-head image, or, a
	// component, accepted no connection from the tester for its whole
	// readiness window while Ready (componentClass). It is charged to the
	// intent: it counts as a verification and may start a verify-fix round
	// (critique fix 13).
	EnvAppFailure EnvClass = "AppFailure"
	// EnvInfra: trouble the change cannot cause (the tester, a backing
	// service, the seed, a part no node would take, a component the cluster
	// disrupted or that went not-Ready with no evidence of the
	// application's, a role maul does not know). It gets maul's free
	// retries, then Unavailable.
	EnvInfra EnvClass = "Infra"
)

// ClassifyEnvironment classifies parts. Infrastructure trouble anywhere
// wins over an application failure, since it may explain it and must not be
// charged to the intent.
func ClassifyEnvironment(parts []PartFact) EnvClass {
	out := EnvOK
	for _, p := range parts {
		switch partClass(p) {
		case EnvInfra:
			return EnvInfra
		case EnvAppFailure:
			out = EnvAppFailure
		}
	}
	return out
}

// AppFailureParts names the parts showing an application failure, in the
// given order: the first is the one a verify-fix round targets.
func AppFailureParts(parts []PartFact) []string {
	var out []string
	for _, p := range parts {
		if partClass(p) == EnvAppFailure {
			out = append(out, p.Name)
		}
	}
	return out
}

// partClass is one part's class. A component must be Ready; a Job's part
// (tester, command, seed) is judged by how it ran, not by readiness.
func partClass(p PartFact) EnvClass {
	if p.Unschedulable {
		return EnvInfra
	}
	oom := p.LastTermination != nil && p.LastTermination.Reason == "OOMKilled"
	crashed := p.Waiting != "" || p.Evicted || oom
	switch p.Role {
	case RoleComponent:
		return componentClass(p)
	case RoleCommand:
		if crashed {
			return EnvAppFailure
		}
	case RoleTester, RoleTools, RoleSeed:
		if crashed || (p.LastTermination != nil && !completed(*p.LastTermination)) {
			return EnvInfra
		}
	case RoleService:
		if crashed || !p.Ready {
			return EnvInfra
		}
	default:
		return EnvInfra
	}
	return EnvOK
}

// componentClass is a component's class, judged by what happened during the
// attempt; its last termination is evidence, an OOM included, unless it is
// CrashedDuring.
//
// The application is charged only on unambiguous evidence: it is crash
// looping or cannot pull its own image (Waiting), its process exited
// non-zero or was OOM-killed during the tester's run (CrashedDuring), or the
// kubelet evicted it for the memory or storage it used (Evicted). Anything
// else that took it away (Disrupted), and a component not Ready with no such
// evidence, is maul's Infra: a node going NotReady sets Ready False with no
// reason, and the application holds no API credential to drain, delete or
// preempt anything. This is safe: the most an application can do without
// crashing is make itself not-Ready, which earns an honest, bounded Infra
// (retried, then Unavailable), never a pass, while its preview goes
// not-live and shows the error.
//
// A Ready, undisrupted component the tester could not connect to for its
// whole readiness window refused it: the window absorbs maul's slot policy
// being programmed (about a second on k3s), and past it only the
// application can be the cause (a capability-less process can drop SYNs
// from every source but the node with a classic BPF socket filter and keep
// passing its probe). If it is not Ready or disrupted, that cause wins.
func componentClass(p PartFact) EnvClass {
	switch {
	case p.Waiting != "" || p.Evicted || p.CrashedDuring:
		return EnvAppFailure
	case p.Disrupted || !p.Ready:
		return EnvInfra
	case p.UnreachableForMs > 0:
		return EnvAppFailure
	}
	return EnvOK
}

func completed(t Termination) bool {
	return t.ExitCode == 0 && (t.Reason == "Completed" || t.Reason == "")
}
