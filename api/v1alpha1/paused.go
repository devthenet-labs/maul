// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReasonAgentsPaused is ConditionAgentsPaused's only reason.
const ReasonAgentsPaused = "Paused"

// AgentsPausedMessage is ConditionAgentsPaused's fixed message: it names the
// switch, so whoever reads the run knows what to lift.
const AgentsPausedMessage = "agents are paused by the operator (Helm agent.paused, --agents-paused): " +
	"this run launches once the pause is lifted"

// HeldByPause reports whether an agent run waits on the operator's agent
// pause: agents are paused and the run is pending, or was granted a slot but
// has no Job yet. A run whose Job exists is not held: it runs to the end and
// is collected as usual.
func HeldByPause(paused bool, phase RunPhase, job *JobReference) bool {
	if !paused {
		return false
	}
	switch phase {
	case "", RunPending:
		return true
	case RunRunning:
		return job == nil
	}
	return false
}

// SyncAgentsPaused makes ConditionAgentsPaused say whether the run is held
// (HeldByPause): True with the fixed reason and message while it is, absent
// otherwise. It reports whether conds changed, so a caller writes status only
// then; a run that never waited on a pause is never touched.
func SyncAgentsPaused(conds *[]metav1.Condition, held bool, now time.Time) bool {
	if !held {
		return meta.RemoveStatusCondition(conds, ConditionAgentsPaused)
	}
	return meta.SetStatusCondition(conds, metav1.Condition{
		Type:               ConditionAgentsPaused,
		Status:             metav1.ConditionTrue,
		Reason:             ReasonAgentsPaused,
		Message:            AgentsPausedMessage,
		LastTransitionTime: metav1.NewTime(now),
	})
}
