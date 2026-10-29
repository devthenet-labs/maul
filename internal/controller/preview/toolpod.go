// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/previewtools"
)

// The slot tool pod (DESIGN 2.1, 2.2, 4.2; SPEC S1-13, Q4): an outcome
// verification runs not the tester but a Job whose main container is the
// tools relay (cmd/preview-tools) and whose browser (the keyed CDP shim and
// Chromium) and sandbox (the exec shim and toolbox) are native sidecars,
// init containers that restart always: the Job completes when tools exits.
// Each container runs as its own UID, read-only, with no capability, no
// ServiceAccount token and no host namespace. tools alone mounts the tool
// ConfigMap (its config, the outcomes, the captured logs and events);
// tools and browser alone mount the per-verification CDP key; tools and
// sandbox share only the exec shim's socket directory.
//
// The pod is the slot's tester part (maul.run/preview-part tester), so the
// Preview's own edge policies give it exactly the tester's edges to the
// components and nothing else (no DNS: preview-isolation does not select
// role tools); its role is tools (maul.run/preview-role), and it carries
// its verification's UID (maul.run/preview-verification), which the
// per-verification ingress policy (toolpolicy.go) admits one evaluator by.
// Its Job is named as a tester Job is (testerJobName), so the slot cleanup,
// the sweep and the Job lookup cover it unchanged; the key Secret and the
// ConfigMap are owned by the Job, so its deletion removes them.

const (
	// toolsContainer is the relay, the Job's one main container; its log
	// and termination message are the ledger. browserContainer and
	// sandboxContainer are the sidecars.
	toolsContainer   = "tools"
	browserContainer = "browser"
	sandboxContainer = "sandbox"
	// toolsPart names the tool pod's environment fact.
	toolsPart = "tools"
	// toolKeyInfix and toolConfigInfix name the key Secret and the tool
	// ConfigMap beside the Job: preview-<p>-vk-<hash>, preview-<p>-vc-<hash>.
	toolKeyInfix    = "-vk-"
	toolConfigInfix = "-vc-"
	// cdpKeyDir holds the CDP key, file cdpKeyFile, in tools and browser.
	cdpKeyDir  = "/etc/maul-cdp"
	cdpKeyFile = "key"
	// cdpKeyBytes is the key's entropy: 256 bits, hex-encoded.
	cdpKeyBytes = 32
	// toolOutcomesKey holds the outcomes under evaluation, JSON.
	toolOutcomesKey = "outcomes.json"
	// execSocketDir is the exec shim's socket directory (toolexec).
	execSocketDir = "/run/maul-exec"
	// sandboxScratch is the sandbox's working directory (toolexec).
	sandboxScratch = "/work/scratch"
	// toolsGrace bounds a sidecar's stop: browser and sandbox exit on
	// SIGTERM, so the Job completes promptly once tools has (SP-2).
	toolsGrace = 10
	// toolViewer is the X-Maul-Viewer the relay sends for the evaluator.
	toolViewer = "maul-evaluator"
)

// The containers' own UIDs: no container can read another's files.
const (
	toolsUID   int64 = 65532
	browserUID int64 = 65533
	sandboxUID int64 = 65534
)

// toolResources are each container's requests, equal to its limits, sized
// to the slot LimitRange's per-container maximum and, together, to what the
// slot quota keeps for one tool pod beside a full application (SP-2): 2500m
// and 2Gi with both sidecars. The browser's /dev/shm (toolShm) counts
// within its memory.
var toolResources = map[string][2]string{
	toolsContainer:   {"500m", "384Mi"},
	browserContainer: {"1500m", "1280Mi"},
	sandboxContainer: {"500m", "384Mi"},
}

// toolShm bounds the browser's shared memory: two Chromiums peaked at 503
// MiB with it included (SP-2).
const toolShm = "256Mi"

var errToolImages = errors.New("the evaluator needs the tools, browser and toolbox images")

// evaluating reports whether outcome verifications run on the tool pod.
func (s Settings) evaluating() bool { return s.Evaluator && s.verifying() }

// maxEvaluations is MaxEvaluations, or SlotCount when unset.
func (s Settings) maxEvaluations() int {
	if s.MaxEvaluations > 0 {
		return s.MaxEvaluations
	}
	return s.SlotCount
}

// validateEvaluator refuses an evaluator without verification, without
// each of its three images digest-pinned outside the preview image prefix,
// or without the agents namespace its ingress policy admits.
func (s Settings) validateEvaluator() error {
	if s.MaxEvaluations < 0 {
		return errors.New("max evaluations is negative")
	}
	if !s.Evaluator {
		return nil
	}
	if !s.verifying() {
		return errors.New("the evaluator needs verification on (a tester image)")
	}
	if s.ToolsImage == "" || s.BrowserImage == "" || s.ToolboxImage == "" {
		return errToolImages
	}
	for _, img := range []string{s.ToolsImage, s.BrowserImage, s.ToolboxImage} {
		if !testerImagePattern.MatchString(img) {
			return fmt.Errorf("tool image %q is not pinned by digest (<repository>@sha256:<64 hex>)", img)
		}
		if strings.HasPrefix(img, s.ImagePrefix) {
			return fmt.Errorf("tool image %q lies under the preview image prefix", img)
		}
	}
	if !namePattern.MatchString(s.AgentsNamespace) || len(s.AgentsNamespace) > maxObjectName {
		return fmt.Errorf("agents namespace %q is not a namespace name", s.AgentsNamespace)
	}
	return nil
}

// toolName names one of verification pv's tool objects in Preview p's slot
// by its infix, as testerJobName names the Job.
func toolName(p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification, infix string) string {
	return "preview-" + p.Name + infix + strings.TrimPrefix(testerJobName(p, pv), "preview-"+p.Name+testerInfix)
}

// toolDeadlines splits a verification's timeout under the slot's Job
// deadline bound: the Job's deadline, the pod's below it (a Job deadline
// deletes the pod, and with it the log and termination message; a pod
// deadline keeps both, SP-2), and the relay's own wall clock below that, so
// tools finishes and writes its ledger before the kubelet stops it.
func toolDeadlines(timeout, max time.Duration) (job, pod, wall time.Duration) {
	const margin = 3 * time.Minute
	job = min(timeout+2*margin, max)
	gap := min(margin, job/4)
	pod = job - gap
	wall = min(timeout, pod-gap)
	return job, pod, wall
}

// toolJob renders verification pv's tool Job in Preview p's slot under a
// deadline at most deadlineMax. It refuses rather than render a Job that
// does not belong: the evaluator off, a Preview that is not
// verification-capable, a verification of another Preview or of no
// outcomes, a timeout out of bounds, an oversized name.
func (s Settings) toolJob(p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification, slot int32,
	deadlineMax time.Duration) (*batchv1.Job, error) {
	timeout := pv.Spec.Timeout.Duration
	switch {
	case !s.evaluating():
		return nil, errors.New("the evaluator is off")
	case p.Spec.Environment == nil || !p.Spec.Environment.Verification:
		return nil, fmt.Errorf("preview %s is not verification-capable", p.Name)
	case pv.UID == "" || pv.Spec.PreviewRef.Name != p.Name || pv.Spec.PreviewRef.UID != p.UID:
		return nil, fmt.Errorf("verification %s is not preview %s's", pv.Name, p.Name)
	case len(pv.Spec.Outcomes) == 0 || pv.Spec.Tools == nil:
		return nil, fmt.Errorf("verification %s evaluates no outcomes", pv.Name)
	case timeout < testerTimeoutMin || timeout > testerTimeoutMax:
		return nil, fmt.Errorf("verification timeout %s is outside [%s, %s]", timeout, testerTimeoutMin,
			testerTimeoutMax)
	}
	name := testerJobName(p, pv)
	if len(toolName(p, pv, toolConfigInfix)) > maxObjectName {
		return nil, fmt.Errorf("tool Job name %s is over %d characters", name, maxObjectName)
	}
	jobDeadline, podDeadline, _ := toolDeadlines(timeout, deadlineMax)
	meta := toolLabels(p, pv)
	pod := toolLabels(p, pv)
	pod[labelPart] = v1alpha1.PreviewPartTester
	tools := pv.Spec.Tools
	spec := corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever, ActiveDeadlineSeconds: ptr(int64(podDeadline / time.Second)),
		TerminationGracePeriodSeconds: ptr(int64(toolsGrace)),
		ServiceAccountName:            "default", AutomountServiceAccountToken: ptr(false),
		EnableServiceLinks: ptr(false),
		NodeSelector:       s.nodeSelector(),
		Tolerations:        s.tolerations(),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   ptr(true),
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{s.toolsMain(tools)},
		Volumes:    toolVolumes(p, pv, tools),
	}
	if tools.Browser {
		spec.InitContainers = append(spec.InitContainers, s.browserSidecar())
	}
	if tools.Sandbox {
		spec.InitContainers = append(spec.InitContainers, s.sandboxSidecar())
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.slotName(slot), Labels: meta},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr(int32(0)), Parallelism: ptr(int32(1)), Completions: ptr(int32(1)),
			ActiveDeadlineSeconds: ptr(int64(jobDeadline / time.Second)),
			Template:              corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: pod}, Spec: spec},
		},
	}, nil
}

// toolLabels are the tool Job's labels and, with its part, its pod's: the
// Preview's, the verification's UID (the sweep's and the evaluator's), and
// the tools role.
func toolLabels(p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification) map[string]string {
	l := labelsFor(p)
	l[labelVerificationUID] = string(pv.UID)
	l[labelRole] = v1alpha1.PreviewRoleTools
	l[v1alpha1.LabelPreviewVerification] = string(pv.UID)
	return l
}

// toolSecurity is a tool container's posture under its own UID.
func toolSecurity(uid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser: ptr(uid), RunAsGroup: ptr(uid), RunAsNonRoot: ptr(true),
		AllowPrivilegeEscalation: ptr(false), ReadOnlyRootFilesystem: ptr(true),
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// toolResourcesOf is a container's requests and limits, equal.
func toolResourcesOf(container string) corev1.ResourceRequirements {
	r := toolResources[container]
	list := corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse(r[0]), corev1.ResourceMemory: resource.MustParse(r[1]),
	}
	return corev1.ResourceRequirements{Requests: list, Limits: list.DeepCopy()}
}

// toolsMain is the relay: it listens on previewtools.Port, Ready once it
// serves the evaluator, and its stdout and termination message are the
// ledger.
func (s Settings) toolsMain(tools *v1alpha1.ToolsSpec) corev1.Container {
	mounts := []corev1.VolumeMount{
		{Name: "tool-config", MountPath: previewtools.DefaultConfigDir, ReadOnly: true},
		{Name: "tools-tmp", MountPath: "/tmp"},
	}
	if tools.Browser {
		mounts = append(mounts, corev1.VolumeMount{Name: "cdp-key", MountPath: cdpKeyDir, ReadOnly: true})
	}
	if tools.Sandbox {
		mounts = append(mounts, corev1.VolumeMount{Name: "exec-socket", MountPath: execSocketDir})
	}
	return corev1.Container{
		Name: toolsContainer, Image: s.ToolsImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{{Name: "tools", ContainerPort: previewtools.Port,
			Protocol: corev1.ProtocolTCP}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: previewtools.ReadyPath,
				Port: intstr.FromInt32(previewtools.Port)}},
			PeriodSeconds: 2, FailureThreshold: 1,
		},
		TerminationMessagePath:   previewtools.TerminationPath,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		VolumeMounts:             mounts,
		Resources:                toolResourcesOf(toolsContainer),
		SecurityContext:          toolSecurity(toolsUID),
	}
}

// browserSidecar is the keyed CDP shim and Chromium, a native sidecar.
func (s Settings) browserSidecar() corev1.Container {
	return corev1.Container{
		Name: browserContainer, Image: s.BrowserImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Args:          []string{"--key-file=" + cdpKeyDir + "/" + cdpKeyFile},
		RestartPolicy: ptr(corev1.ContainerRestartPolicyAlways),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "cdp-key", MountPath: cdpKeyDir, ReadOnly: true},
			{Name: "browser-tmp", MountPath: "/tmp"},
			{Name: "browser-shm", MountPath: "/dev/shm"},
		},
		Resources:       toolResourcesOf(browserContainer),
		SecurityContext: toolSecurity(browserUID),
	}
}

// sandboxSidecar is the exec shim and toolbox, a native sidecar: nothing
// secret, its own scratch.
func (s Settings) sandboxSidecar() corev1.Container {
	return corev1.Container{
		Name: sandboxContainer, Image: s.ToolboxImage, ImagePullPolicy: corev1.PullIfNotPresent,
		RestartPolicy: ptr(corev1.ContainerRestartPolicyAlways),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "exec-socket", MountPath: execSocketDir},
			{Name: "sandbox-scratch", MountPath: sandboxScratch},
			{Name: "sandbox-tmp", MountPath: "/tmp"},
		},
		Resources:       toolResourcesOf(sandboxContainer),
		SecurityContext: toolSecurity(sandboxUID),
	}
}

// toolVolumes are the pod's volumes: the tool ConfigMap, the CDP key with a
// browser, the exec socket with a sandbox, and bounded emptyDirs.
func toolVolumes(p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification, tools *v1alpha1.ToolsSpec) []corev1.Volume {
	empty := func(name, size string, medium corev1.StorageMedium) corev1.Volume {
		q := resource.MustParse(size)
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: medium, SizeLimit: &q}}}
	}
	vols := []corev1.Volume{
		{Name: "tool-config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: toolName(p, pv, toolConfigInfix)},
			DefaultMode:          ptr(int32(0o444))}}},
		empty("tools-tmp", "256Mi", ""),
	}
	if tools.Browser {
		vols = append(vols,
			corev1.Volume{Name: "cdp-key", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: toolName(p, pv, toolKeyInfix), DefaultMode: ptr(int32(0o444))}}},
			empty("browser-tmp", "512Mi", ""), empty("browser-shm", toolShm, corev1.StorageMediumMemory))
	}
	if tools.Sandbox {
		vols = append(vols, empty("exec-socket", "1Mi", ""), empty("sandbox-scratch", "256Mi", ""),
			empty("sandbox-tmp", "256Mi", ""))
	}
	return vols
}

// jobOwned is an owner reference to the tool Job: its key Secret and
// ConfigMap go when it does, the sweep's deletion included.
func jobOwned(job *batchv1.Job) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID}}
}

// newCDPKey draws a fresh per-verification CDP key: 256 random bits, hex.
func newCDPKey() string {
	b := make([]byte, cdpKeyBytes)
	_, _ = rand.Read(b) // crypto/rand.Read never fails
	return hex.EncodeToString(b)
}

// toolKeySecret is the CDP key Secret, owned by the tool Job, immutable.
func (s Settings) toolKeySecret(p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification, job *batchv1.Job,
	key string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: toolName(p, pv, toolKeyInfix), Namespace: job.Namespace,
			Labels: toolLabels(p, pv), OwnerReferences: jobOwned(job)},
		Immutable: ptr(true),
		Type:      corev1.SecretTypeOpaque,
		Data:      map[string][]byte{cdpKeyFile: []byte(key)},
	}
}

// toolConfigMap is the tool ConfigMap with data, owned by the tool Job.
func (s Settings) toolConfigMap(p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification, job *batchv1.Job,
	data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: toolName(p, pv, toolConfigInfix), Namespace: job.Namespace,
			Labels: toolLabels(p, pv), OwnerReferences: jobOwned(job)},
		Data: data,
	}
}

// toolConfig is what the relay runs: each component by its Service address
// (targets, the ClusterIP on the Service port, as the tester's), its path
// on the preview host, the saved checks, the bounds and the wall clock. It
// is validated as the relay will validate it.
func (s Settings) toolConfig(p *v1alpha1.Preview, pv *v1alpha1.PreviewVerification, targets map[string]string,
	wall time.Duration) (previewtools.Config, error) {
	cfg := previewtools.Config{
		Host: s.host(p), Viewer: toolViewer, MaxToolCalls: int(pv.Spec.Tools.MaxToolCalls),
		TimeoutSeconds: int(wall / time.Second), ReplayOnly: pv.Spec.Rung == v1alpha1.RungReplay,
		Browser: pv.Spec.Tools.Browser, Sandbox: pv.Spec.Tools.Sandbox,
	}
	for _, c := range p.Spec.Components {
		addr, ok := targets[c.Name]
		if !ok {
			return previewtools.Config{}, fmt.Errorf("component %s has no target", c.Name)
		}
		cfg.Components = append(cfg.Components, previewtools.Component{Key: c.Name,
			Path: v1alpha1.PreviewComponentPath(c), Addr: addr})
	}
	for _, c := range pv.Spec.Checks {
		cfg.Checks = append(cfg.Checks, previewtools.StoredCheck{ID: c.ID, Digest: c.Digest, Body: c.Body})
	}
	if err := cfg.Validate(); err != nil {
		return previewtools.Config{}, err
	}
	return cfg, nil
}

// toolConfigData is the tool ConfigMap's own data: the relay's config and
// the outcomes under evaluation. The capture adds its keys beside them.
func toolConfigData(cfg previewtools.Config, outcomes []v1alpha1.AcceptanceOutcome) (map[string]string, error) {
	c, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode the tool config: %w", err)
	}
	o, err := json.Marshal(outcomes)
	if err != nil {
		return nil, fmt.Errorf("encode the outcomes: %w", err)
	}
	return map[string]string{previewtools.ConfigFile: string(c), toolOutcomesKey: string(o)}, nil
}
