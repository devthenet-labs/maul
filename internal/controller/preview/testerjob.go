// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// The tester runs a verification's http criteria from inside the Preview's
// slot, as a Job the controller renders here: run once (no retry, no
// self-deletion), bounded by the verification's timeout on the Job and on
// its Pod, under the same placement and restricted posture as a component,
// with no ServiceAccount token, no port, and its whole input in one literal
// environment variable. It observes and never judges; the maul-preview-jobs
// admission policy holds every Job written into a slot to this shape.

const (
	// testerInfix separates a tester Job's name from its verification's
	// hash.
	testerInfix = "-vt-"
	// testerContainer names the tester's one container.
	testerContainer = "tester"
	// envVerifySpec carries the tester's input: the criteria, the run token
	// and the targets, as JSON.
	envVerifySpec = "MAUL_VERIFY_SPEC"
	// maxTesterInput bounds that input, well inside a Pod spec's size.
	maxTesterInput = 32 << 10
	// testerTimeoutMin and testerTimeoutMax bound a verification's timeout,
	// as the PreviewVerification schema does.
	testerTimeoutMin = time.Minute
	testerTimeoutMax = time.Hour
)

// testerJobName names verification pv's tester Job in Preview p's slot:
// preview-<p>-vt-<the first 10 hex of sha256(pv's UID)>. A new attempt is a
// new PreviewVerification, so a new Job.
func testerJobName(p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification) string {
	sum := sha256.Sum256([]byte(pv.UID))
	return "preview-" + p.Name + testerInfix + hex.EncodeToString(sum[:])[:10]
}

// testerJob renders verification pv's tester Job in Preview p's slot, its
// input the JSON the tester reads. It refuses rather than render a Job that
// does not belong: verification off, a Preview that is not
// verification-capable, a verification of another Preview, a timeout out of
// bounds, an oversized input or name.
func (s Settings) testerJob(p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification, slot int32,
	input []byte) (*batchv1.Job, error) {
	timeout := pv.Spec.Timeout.Duration
	switch {
	case !s.verifying():
		return nil, fmt.Errorf("verification is off: no tester image")
	case p.Spec.Environment == nil || !p.Spec.Environment.Verification:
		return nil, fmt.Errorf("preview %s is not verification-capable", p.Name)
	case pv.UID == "" || pv.Spec.PreviewRef.Name != p.Name || pv.Spec.PreviewRef.UID != p.UID:
		return nil, fmt.Errorf("verification %s is not preview %s's", pv.Name, p.Name)
	case timeout < testerTimeoutMin || timeout > testerTimeoutMax:
		return nil, fmt.Errorf("verification timeout %s is outside [%s, %s]", timeout, testerTimeoutMin,
			testerTimeoutMax)
	case len(input) > maxTesterInput:
		return nil, fmt.Errorf("tester input is %d bytes, over %d", len(input), maxTesterInput)
	}
	name := testerJobName(p, pv)
	if len(name) > maxObjectName {
		return nil, fmt.Errorf("tester Job name %s is over %d characters", name, maxObjectName)
	}
	deadline := int64(timeout / time.Second)
	meta := labelsFor(p)
	meta[labelVerificationUID] = string(pv.UID)
	pod := labelsFor(p)
	pod[labelVerificationUID] = string(pv.UID)
	pod[labelPart] = v1alpha1.PreviewPartTester
	pod[labelRole] = string(v1alpha1.PreviewRoleTester)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.slotName(slot), Labels: meta},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr(int32(0)), Parallelism: ptr(int32(1)), Completions: ptr(int32(1)),
			ActiveDeadlineSeconds: ptr(deadline),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: pod},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever, ActiveDeadlineSeconds: ptr(deadline),
					ServiceAccountName: "default", AutomountServiceAccountToken: ptr(false),
					NodeSelector: s.nodeSelector(),
					Tolerations:  s.tolerations(),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: ptr(true), RunAsUser: ptr(int64(65532)), RunAsGroup: ptr(int64(65532)),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name: testerContainer, Image: s.TesterImage, ImagePullPolicy: corev1.PullIfNotPresent,
						Env:                      []corev1.EnvVar{{Name: envVerifySpec, Value: string(input)}},
						TerminationMessagePath:   corev1.TerminationMessagePathDefault,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("25m"),
								corev1.ResourceMemory: resource.MustParse("32Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("250m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
					}},
				},
			},
		},
	}, nil
}

// deleteJob deletes a Job and, in the background, its Pods: a Job deleted
// without a propagation policy orphans them.
func deleteJob(ctx context.Context, c client.Client, job *batchv1.Job) error {
	err := c.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if kerrors.IsNotFound(err) {
		return nil
	}
	return err
}
