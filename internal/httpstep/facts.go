// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

// Part roles, as PartFact.Role.
const (
	RoleComponent = "component"
	RoleService   = "service"
	RoleTester    = "tester"
	RoleCommand   = "command"
	RoleSeed      = "seed"
	// RoleTools is the agent evaluator's slot tool pod, judged like the
	// tester: by how it ran.
	RoleTools = "tools"
	// RoleBuild is the tool pod's build of the head (slice S2, the agents
	// placement; PreviewPartRole "build"): it built only when it terminated
	// Completed with exit 0. Only the head build is a part: a base the pod
	// could not build is BaseRuns.Unbuildable, never a part.
	RoleBuild = "build"
)

// PartFact is the environment evidence of one part of a Preview's slot,
// from its pod's container status.
type PartFact struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	// Waiting is the container's waiting reason (CrashLoopBackOff,
	// ImagePullBackOff, …), empty when it is not waiting.
	Waiting string `json:"waiting,omitempty"`
	// LastTermination is the container's last (or, for a finished Job's
	// container, current) termination.
	LastTermination *Termination `json:"lastTermination,omitempty"`
	Evicted         bool         `json:"evicted,omitempty"`
	Unschedulable   bool         `json:"unschedulable,omitempty"`
	// CrashedDuring reports that the component's process exited (OOM
	// included) during the attempt, by the kubelet's timestamps within the
	// tester's run: the application's failure, however it exited.
	CrashedDuring bool `json:"crashedDuring,omitempty"`
	// Disrupted reports that the cluster, not the application, took the
	// component's Pod away during the attempt: the Eviction API, a drain,
	// preemption, the taint manager, Pod GC, a deletion, node loss, or a
	// replacement whose old Pod shows no cause of the application's.
	Disrupted bool `json:"disrupted,omitempty"`
	// UnreachableForMs is how long the tester tried, and failed, to connect
	// to the component's Service before its first criterion: its whole
	// readiness window. Zero when it was reached.
	UnreachableForMs int32 `json:"unreachableForMs,omitempty"`
	// LogTail is opaque display text: never judged, never fingerprinted.
	LogTail string `json:"logTail,omitempty"`
}

// Termination is a container termination.
type Termination struct {
	Reason   string `json:"reason"`
	ExitCode int32  `json:"exitCode"`
}
