// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolpod

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/previewtools"
)

const (
	// KeyBytes is a per-evaluation key's entropy: 256 bits, hex-encoded.
	KeyBytes = 32
	// OutcomesKey holds the outcomes under evaluation, JSON, beside the
	// relay's configuration (previewtools.ConfigFile).
	OutcomesKey = "outcomes.json"
)

// Security is a tool container's posture under its own UID: no container
// can read another's files.
func Security(uid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser: ptr(uid), RunAsGroup: ptr(uid), RunAsNonRoot: ptr(true),
		AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// Deadlines splits a verification's timeout under a Job deadline bound
// max: the Job's deadline, the pod's below it (a Job deadline deletes the
// pod, and with it the log and termination message; a pod deadline keeps
// both, SP-2), and the relay's own wall clock below that, so tools
// finishes and writes its ledger before the kubelet stops it.
func Deadlines(timeout, max time.Duration) (job, pod, wall time.Duration) {
	const margin = 3 * time.Minute
	job = min(timeout+2*margin, max)
	gap := min(margin, job/4)
	pod = job - gap
	wall = min(timeout, pod-gap)
	return job, pod, wall
}

// NewKey draws a fresh per-evaluation key: KeyBytes random bytes, hex.
func NewKey() string {
	b := make([]byte, KeyBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}

// ConfigData is the relay's configuration data: its config
// (previewtools.ConfigFile) and the outcomes under evaluation
// (OutcomesKey). A host's capture adds its keys beside them.
func ConfigData(cfg previewtools.Config, outcomes []v1alpha1.AcceptanceOutcome) (map[string]string, error) {
	c, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode the tool config: %w", err)
	}
	o, err := json.Marshal(outcomes)
	if err != nil {
		return nil, fmt.Errorf("encode the outcomes: %w", err)
	}
	return map[string]string{previewtools.ConfigFile: string(c), OutcomesKey: string(o)}, nil
}

func ptr[T any](v T) *T { return &v }
