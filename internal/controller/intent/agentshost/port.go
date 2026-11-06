// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package agentshost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// ToolPods is the agents namespace as the host uses it: the tool pod's
// Job, its pod, its tools container's log and its Secret's key. The
// production adapter is Kube; tests fake it.
type ToolPods interface {
	// Create creates the tool pod (jobs.Client.CreateToolPod).
	Create(ctx context.Context, spec jobs.ToolPodSpec) error
	// SandboxEnv is the sandboxes' rendered environment
	// (jobs.Client.ToolPodSandboxEnv).
	SandboxEnv(spec jobs.ToolPodSpec) ([]corev1.EnvVar, error)
	// Key is the key in job's Secret (<job>-tools) and its verification
	// label; found is false when there is no such Secret.
	Key(ctx context.Context, job string) (key, verification string, found bool, err error)
	// Get is job and its newest pod: a nil Job when there is none, a nil
	// pod when it has none yet.
	Get(ctx context.Context, job string) (*batchv1.Job, *corev1.Pod, error)
	// Log is the tools container's log of pod, at most
	// toolwire.MaxLogBytes+1 bytes; never a sandbox's.
	Log(ctx context.Context, pod *corev1.Pod) ([]byte, error)
	// Delete deletes job (background propagation) and its Secret; what is
	// gone is no error.
	Delete(ctx context.Context, job string) error
	// List is every tool Job (maul.run/role tools) in the agents namespace.
	List(ctx context.Context) ([]batchv1.Job, error)
}

// The tool pod's containers, as jobs renders them.
var (
	toolsContainer   = jobs.ToolsContainer
	prepareContainer = jobs.PrepareContainer
	sandboxes        = []string{jobs.ToolSandbox(jobs.ToolSideHead), jobs.ToolSandbox(jobs.ToolSideBase)}
)

// JobName is the tool Job of pv: pv's own name when it is a DNS label short
// enough for the Job's Secret (<job>-tools, Q28's <pv>-tools) to be one
// too, else "tools-" and a hash of it. A verification is one attempt, so
// one Job per verification is one per attempt: a Job adopted under the
// name is always this verification's render (its UID label is checked).
func JobName(pv *v1alpha1.PreviewVerification) string {
	if len(pv.Name) <= jobs.ToolPodNameMax && len(validation.IsDNS1123Label(pv.Name)) == 0 {
		return pv.Name
	}
	sum := sha256.Sum256([]byte(pv.Name))
	return "tools-" + hex.EncodeToString(sum[:10])
}

// ErrKeyNotFound is a verification whose tool pod Secret holds no key of
// its own.
var ErrKeyNotFound = errors.New("agentshost: the tool pod's key was not found")

// ReadKey reads pv's tools key from its tool pod Secret in namespace (the
// agents namespace) through r, uncached: the key the evaluator's handoff
// carries (SPEC-S2 Q28, S2-20). ErrKeyNotFound when there is no Secret, it
// is another verification's, or it holds no well-formed key.
func ReadKey(ctx context.Context, r client.Reader, namespace string, pv *v1alpha1.PreviewVerification) (
	string, error) {
	key, uid, found, err := readKey(ctx, r, namespace, JobName(pv))
	switch {
	case err != nil:
		return "", err
	case !found || uid != string(pv.UID) || !toolwire.IsKey(key):
		return "", ErrKeyNotFound
	}
	return key, nil
}

// readKey is job's Secret's key and verification label.
func readKey(ctx context.Context, r client.Reader, namespace, job string) (key, uid string, found bool, err error) {
	var s corev1.Secret
	err = r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: job + jobs.ToolsSecretSuffix}, &s)
	switch {
	case kerrors.IsNotFound(err):
		return "", "", false, nil
	case err != nil:
		return "", "", false, fmt.Errorf("read the tool pod secret of %s: %w", job, err)
	}
	return string(s.Data[jobs.ToolsKeyFile]), s.Labels[v1alpha1.LabelPreviewVerification], true, nil
}

// Kube is the production ToolPods: Jobs, pods and logs through the
// clientset, the Secret through an uncached reader (ReadKey's), creation
// through the jobs Client.
type Kube struct {
	CS        kubernetes.Interface
	Reader    client.Reader
	Jobs      *jobs.Client
	Namespace string
}

var _ ToolPods = Kube{}

func (k Kube) Create(ctx context.Context, spec jobs.ToolPodSpec) error {
	_, err := k.Jobs.CreateToolPod(ctx, spec)
	return err
}

func (k Kube) SandboxEnv(spec jobs.ToolPodSpec) ([]corev1.EnvVar, error) {
	return k.Jobs.ToolPodSandboxEnv(spec)
}

func (k Kube) Key(ctx context.Context, job string) (string, string, bool, error) {
	return readKey(ctx, k.Reader, k.Namespace, job)
}

func (k Kube) Get(ctx context.Context, name string) (*batchv1.Job, *corev1.Pod, error) {
	job, err := k.CS.BatchV1().Jobs(k.Namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case kerrors.IsNotFound(err):
		return nil, nil, nil
	case err != nil:
		return nil, nil, fmt.Errorf("read tool job %s: %w", name, err)
	}
	list, err := k.CS.CoreV1().Pods(k.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: batchv1.ControllerUidLabel + "=" + string(job.UID)})
	if err != nil {
		return nil, nil, fmt.Errorf("list the pods of tool job %s: %w", name, err)
	}
	var newest *corev1.Pod
	for i := range list.Items {
		if p := &list.Items[i]; newest == nil || newest.CreationTimestamp.Before(&p.CreationTimestamp) {
			newest = p
		}
	}
	return job, newest, nil
}

func (k Kube) Log(ctx context.Context, pod *corev1.Pod) ([]byte, error) {
	limit := int64(toolwire.MaxLogBytes + 1)
	rc, err := k.CS.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name,
		&corev1.PodLogOptions{Container: toolsContainer, LimitBytes: &limit}).Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, limit))
}

func (k Kube) Delete(ctx context.Context, name string) error {
	policy := metav1.DeletePropagationBackground
	err := k.CS.BatchV1().Jobs(k.Namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete tool job %s: %w", name, err)
	}
	err = k.CS.CoreV1().Secrets(k.Namespace).Delete(ctx, name+jobs.ToolsSecretSuffix, metav1.DeleteOptions{})
	if err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("delete tool secret %s: %w", name, err)
	}
	return nil
}

func (k Kube) List(ctx context.Context) ([]batchv1.Job, error) {
	list, err := k.CS.BatchV1().Jobs(k.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: v1alpha1.LabelRole + "=" + v1alpha1.RoleTools})
	if err != nil {
		return nil, fmt.Errorf("list tool jobs: %w", err)
	}
	return list.Items, nil
}
