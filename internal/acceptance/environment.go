// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

// EnvClass sorts a verification's environment.
type EnvClass string

const (
	// EnvOK: no part shows a failure.
	EnvOK EnvClass = "OK"
	// EnvAppFailure: a part the change under test runs (a component, or a
	// command Job in a component's image) crashed (or, a component,
	// restarted during the attempt), never became Ready, was evicted, ran
	// out of memory, could not pull its PR-head image, or, a component,
	// accepted no connection from the tester for its whole readiness window
	// while Ready. It is
	// charged to the intent: it counts as a verification and may start a
	// verify-fix round (critique fix 13).
	EnvAppFailure EnvClass = "AppFailure"
	// EnvInfra: trouble the change cannot cause (the tester, a backing
	// service, the seed, a part no node would take, a role maul does not
	// know). It gets maul's free retries, then Unavailable.
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
		// A Ready component the tester could not connect to for its whole
		// readiness window refused it: the window absorbs maul's slot policy
		// being programmed (about a second on k3s), and past it only the
		// application can be the cause (a capability-less process can drop
		// SYNs from every source but the node with a classic BPF socket
		// filter and keep passing its probe). Never maul's Infra.
		if crashed || !p.Ready || p.RestartedDuring > 0 || p.UnreachableForMs > 0 {
			return EnvAppFailure
		}
	case RoleCommand:
		if crashed {
			return EnvAppFailure
		}
	case RoleTester, RoleSeed:
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

func completed(t Termination) bool {
	return t.ExitCode == 0 && (t.Reason == "Completed" || t.Reason == "")
}
