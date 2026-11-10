// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/agentrun"
	"github.com/devthenet-labs/maul/internal/attribution"
	"github.com/devthenet-labs/maul/internal/netprofile"
	"github.com/devthenet-labs/maul/internal/runnerimage"
	"github.com/devthenet-labs/maul/internal/sandboxprobe"
	"github.com/devthenet-labs/maul/internal/toolpod"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The agents-namespace tool pod (SPEC S2-10; DESIGN 2.4, 2.5, 4.2): the
// hands of an agents verification's evaluator, for a Project with no
// Preview. It builds one repository at the PR head and at its merge base in
// the repository's accepted image and runs the evaluator's exec, files and
// run calls against them. It is built here by its own builder, never by
// buildJob, podVolumes or agentEnv, so no path from it reaches a broker
// token, a model credential or the per-Job handoff Secret.
//
// The pod, one Job:
//
//   - init prepare (maul's preview-tools image, UID 65532): fetches the
//     head and base tarballs from the artifact server, verifies each
//     against its digest, extracts them into the pristine trees-head and
//     trees-base volumes, copies tool-exec into maul-bin, then runs the
//     strict sandbox probe over every target, the cluster's resolver
//     included (exit 78 on open egress);
//   - inits seed-head and seed-base (the same image, each under its
//     sandbox's UID): copy that side's pristine tree into its own work
//     volume, the only writable copy, mounting nothing else;
//   - main tools (the same image, UID 65532): the relay, its config, saved
//     checks and :8090 key from the one Secret <name>-tools, both pristine
//     trees read-only (the files verb), and each sandbox's exec-socket
//     directory; its log and termination message are the ledger;
//   - native sidecars sandbox-head (UID 65540) and sandbox-base (UID
//     65541): the repository's accepted image running tool-exec from the
//     read-only maul-bin, each with its own work, cache, home, tmp,
//     exec-socket and /dev/shm volumes, so head code can neither touch the
//     base tree nor leave anything in base's caches (O2).
//
// No container holds a ServiceAccount token (automount off, SA default,
// no projection), the process namespace is explicitly not shared, every
// container runs read-only under its own UID with no capability, the pod
// has no resolver (DNS none; the artifact server and, with a profile, the
// egress proxy pinned in its hosts file), no Service links, and no
// preview toleration or node selector: it schedules like any agent pod, so
// the repository image must be pullable on whichever node it lands on.
//
// Its network is the build's and never wider (7.8): ProfileOf takes the
// profile from the build Job of the same repository as the launching
// controller would create it, and the tool pod carries exactly the label,
// hosts-file entry and package-manager environment that build would, in the
// sandboxes only. Every rendered pod is checked by toolPodShapeRefusal
// before anything is created.

// The tool pod's two sides, the keys of its two Trees.
const (
	ToolSideHead = "head"
	ToolSideBase = "base"
)

const (
	// ToolsKeyFile is the :8090 key's file in the tools Secret, beside the
	// relay's config: <toolsConfigDir>/tools-key in the tools container.
	ToolsKeyFile = "tools-key"
	// ToolsSecretSuffix names the tool pod's one Secret: <job>-tools.
	ToolsSecretSuffix = "-tools"
	// ToolPodNameMax bounds a tool Job's name so that its Secret's name is
	// a DNS label too.
	ToolPodNameMax = validation.DNS1123LabelMaxLength - len(ToolsSecretSuffix)
)

// The relay's contract (previewtools; a test pins each): its config file
// and directory, its readiness path, its termination message.
const (
	toolsConfigFile      = "tools.json"
	toolsConfigDir       = "/etc/maul-tools"
	toolsReadyPath       = "/readyz"
	toolsTerminationPath = "/dev/termination-log"
)

// Paths in the tool pod. tools reads the pristine trees at
// toolTreesDir/<side> and dials each side's shim at
// toolExecDir/<side>/sock; a sandbox serves at toolExecDir/sock (the shim's
// default) and works in toolWorkDir: the tree at /work/tree, the build's
// output at /work/bin, per-run scratch under /work/scratch, its caches in
// toolCacheDir and HOME at toolHomeDir, the same paths on both sides so one
// check text runs at both (Q23).
const (
	toolTreesDir   = "/trees"
	toolExecDir    = "/run/maul-exec"
	toolExecSocket = "sock"
	toolMaulBinDir = "/maul-bin"
	toolExecBin    = toolMaulBinDir + "/tool-exec"
	toolWorkDir    = "/work"
	toolCacheDir   = toolWorkDir + "/cache"
	toolHomeDir    = toolWorkDir + "/home"
	toolShmDir     = "/dev/shm"
	// toolGrace bounds a sidecar's stop once tools has exited.
	toolGrace = 10
)

// The containers and their UIDs, pairwise distinct between tools and the
// two sandboxes; each seed runs as its sandbox, prepare as tools.
const (
	toolsContainerName = "tools"
	toolsUID           = int64(runAsUser)
	toolHeadUID        = int64(65540)
	toolBaseUID        = int64(65541)
)

// The containers a tool pod's observer reads by name: the tools container
// (its log and termination message are the ledger), the prepare init
// (exit ExitSandboxUnenforced on open egress) and each side's sandbox.
const (
	ToolsContainer   = toolsContainerName
	PrepareContainer = initContainerName
)

// ToolSandbox is side's sandbox container's name.
func ToolSandbox(side string) string { return "sandbox-" + side }

// sideUID is a side's sandbox (and seed) UID.
var sideUID = map[string]int64{ToolSideHead: toolHeadUID, ToolSideBase: toolBaseUID}

// toolSides are the two sides in pod order.
var toolSides = []string{ToolSideHead, ToolSideBase}

// toolSizes are the tool pod's emptyDir size limits. A side's work volume
// holds the tree's copy and the build's output, and the copy of both the
// sandbox keeps for its check runs (toolexec's fresh.go); its cache holds GOCACHE and
// npm's, about 3.5 GB for a maul-scale tree (SP-6: the module cache stays
// the image's baked, read-only one, or with a go profile the profile's
// under /tmp, about 1.3 GB, hence its tmp); /dev/shm is memory, counted in
// the container's memory. Each side's disk volumes fit within
// DefaultSandboxResources' ephemeral storage, tools' and prepare's within
// DefaultToolsResources'.
var toolSizes = map[string]string{
	"tools-tmp":   "256Mi",
	"prepare-tmp": "512Mi",
	"maul-bin":    "64Mi",
	"trees":       "512Mi",
	"work":        "2Gi",
	"cache":       "3584Mi",
	"home":        "256Mi",
	"tmp":         "2Gi",
	"exec":        "1Mi",
	"shm":         "16Mi",
}

// ToolResources are one tool pod container's requests, equal to its
// limits: CPU, memory and ephemeral storage, Kubernetes quantities. An
// empty field takes the default's.
type ToolResources struct {
	CPU, Memory, EphemeralStorage string
}

// ToolPodResources are the tools container's (prepare and the seeds run
// with them too) and each sandbox's.
type ToolPodResources struct {
	Tools, Sandbox ToolResources
}

// The O9 defaults, re-set from SP-6: the sandbox's CPU is the lever for a
// big cold build (a std+cmd-sized build is 108 s at 1 CPU); its memory held
// a 1.3 GiB working set; its ephemeral storage covers the side's volumes.
var (
	DefaultToolsResources   = ToolResources{CPU: "250m", Memory: "256Mi", EphemeralStorage: "2Gi"}
	DefaultSandboxResources = ToolResources{CPU: "1", Memory: "2Gi", EphemeralStorage: "8Gi"}
)

// ToolPodSpec is one agents verification's tool pod (CreateToolPod).
type ToolPodSpec struct {
	// Name is the Job's name, a DNS label of at most ToolPodNameMax
	// characters; its Secret is Name+ToolsSecretSuffix.
	Name string
	// Verification is the PreviewVerification's UID, the
	// v1alpha1.LabelPreviewVerification value the evaluator's ingress
	// policy admits it by.
	Verification string
	// Repo is the repository, "owner/name".
	Repo string
	// ToolsImage is maul's preview-tools image by digest: prepare, the
	// seeds and tools run it.
	ToolsImage string
	// Image is the repository's accepted runner image by digest (the
	// PreviewVerification's workspace.sandboxImage): both sandboxes run it.
	Image string
	// SearchPath is that image's sanitized PATH, absolute entries joined
	// by ":", the sandboxes' PATH.
	SearchPath string
	// Trees are exactly the head and the base of one repository, keyed
	// ToolSideHead and ToolSideBase: each its commit (BaseSHA), its
	// tarball on the artifact server and that tarball's digest.
	Trees []Tree
	// Data is the relay's configuration data (toolpod.ConfigData: its
	// config and the outcomes), stored in the tools Secret beside Key.
	Data map[string]string
	// Key is the per-evaluation :8090 key (toolpod.NewKey).
	Key string
	// Timeout is the verification's timeout and DeadlineMax the bound on
	// the Job's deadline (toolpod.Deadlines), at least Timeout.
	Timeout, DeadlineMax time.Duration
	// Resources replace the defaults field by field.
	Resources ToolPodResources
	// AttributedTo is the attributed-to annotation value
	// (v1alpha1.AnnotationAttributedTo) the Job and its pod template carry:
	// attribution.ValueOrg under --attribution-mode attribute, empty (no
	// annotation, exactly as before attribution existed) under off. Nothing
	// else is admitted: the tool pod is no run's (it has no IntentRun and so
	// no run stamp), holds no ServiceAccount token, never reaches the egress
	// broker and spends nothing, so it is never a person's.
	AttributedTo string

	// build is the build Job the profile comes from (ProfileOf).
	build *Spec
}

// ProfileOf takes the tool pod's network from build: the intent build Job
// of the same repository on the same image, as the launching controller
// would create it now. The tool pod carries that Job's network profile
// exactly, or none when the build would get none (no profile asked for,
// the profiles kill switch off). It is the only way a tool pod gets one.
func (s *ToolPodSpec) ProfileOf(build Spec) { s.build = &build }

// ErrToolPodRefused wraps every refusal of a tool pod: a contradiction in
// what the controller asked for, never a transient failure.
var ErrToolPodRefused = errors.New("jobs: tool pod refused")

// CreateToolPod creates the tool pod's Secret, then its Job, then owns the
// Secret by the Job so they go together, and returns the Job's name. Like
// Create it is idempotent: a retry adopts what an earlier call created. A
// refused spec creates nothing; a Job that cannot be created takes the
// Secret this call made with it.
func (c *Client) CreateToolPod(ctx context.Context, spec ToolPodSpec) (string, error) {
	job, want, err := c.buildToolPod(ctx, spec)
	if err != nil {
		return "", err
	}
	secrets := c.cs.CoreV1().Secrets(c.cfg.Namespace)
	secret, err := secrets.Create(ctx, want, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		secret, err = secrets.Get(ctx, want.Name, metav1.GetOptions{})
	}
	if err != nil {
		return "", fmt.Errorf("jobs: create secret %s: %w", want.Name, err)
	}
	// An adopted Secret must be this call's: the Secret is immutable, and a
	// Job mounting another call's would serve a key the caller does not hold.
	if !maps.EqualFunc(secret.Data, want.Data, bytes.Equal) {
		return "", fmt.Errorf("jobs: secret %s exists with other data; the tool pod's key or config differs",
			want.Name)
	}
	jobs := c.cs.BatchV1().Jobs(c.cfg.Namespace)
	created, err := jobs.Create(ctx, job, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		created, err = jobs.Get(ctx, job.Name, metav1.GetOptions{})
	}
	if err != nil {
		_ = secrets.Delete(ctx, want.Name, metav1.DeleteOptions{})
		return "", fmt.Errorf("jobs: create job %s: %w", job.Name, err)
	}
	if !slices.ContainsFunc(secret.OwnerReferences, func(r metav1.OwnerReference) bool { return r.UID == created.UID }) {
		secret.OwnerReferences = append(secret.OwnerReferences, metav1.OwnerReference{
			APIVersion: "batch/v1", Kind: "Job", Name: created.Name, UID: created.UID, Controller: new(true),
		})
		if _, err := secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			return "", fmt.Errorf("jobs: own secret %s: %w", want.Name, err)
		}
	}
	c.log.LogAttrs(ctx, slog.LevelInfo, "created tool pod job",
		slog.String("job", created.Name),
		slog.String("repo", spec.Repo),
		slog.String("verification", spec.Verification),
		slog.String("image", spec.Image),
		slog.String("network_profile", networkProfileOf(created)),
		slog.String("head", spec.tree(ToolSideHead).BaseSHA),
		slog.String("base", spec.tree(ToolSideBase).BaseSHA))
	return created.Name, nil
}

// ToolPodSandboxEnv is the environment both of spec's sandboxes are
// rendered with: the search path, the sandbox's own places and the build's
// network profile (ProfileOf), exactly as CreateToolPod renders them. The
// relay's config carries it (previewtools.Workspace.Env), since the exec
// shim passes a program none of its own environment, so spec's Data is not
// yet written: everything else CreateToolPod would refuse is refused.
// Nothing is created.
func (c *Client) ToolPodSandboxEnv(spec ToolPodSpec) ([]corev1.EnvVar, error) {
	probe := spec
	probe.Data = map[string]string{toolsConfigFile: "{}"}
	if err := c.toolPodSpecRefusal(probe); err != nil {
		return nil, err
	}
	profile, tunnel, err := c.toolProfile(*spec.build)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrToolPodRefused, err)
	}
	return toolSandbox(spec, ToolSideHead, profile, tunnel, corev1.ResourceRequirements{}).Env, nil
}

// tree is the spec's tree of side, or the zero Tree.
func (s ToolPodSpec) tree(side string) Tree {
	for _, t := range s.Trees {
		if t.Key == side {
			return t
		}
	}
	return Tree{}
}

// toolProfile is the profile the build Job would carry, its tunnel
// environment and whether there is one. The tunnel's precondition that
// the runner be brokered is the build's (buildJob's), not the tool pod's:
// no sandbox calls a model.
func (c *Client) toolProfile(build Spec) (*NetworkProfile, []corev1.EnvVar, error) {
	runner, err := c.runnerFor(build.Harness)
	if err != nil {
		return nil, nil, err
	}
	if !c.appliesProfile(runner, build) {
		return nil, nil, nil
	}
	tunnel, err := profileTunnel(runner, build)
	if err != nil {
		return nil, nil, err
	}
	return build.NetworkProfile, tunnel, nil
}

// buildToolPod renders the tool pod's Job and Secret, resolving the hosts
// its pod dials, and checks the rendered pod. Nothing is created.
func (c *Client) buildToolPod(ctx context.Context, spec ToolPodSpec) (*batchv1.Job, *corev1.Secret, error) {
	if err := c.toolPodSpecRefusal(spec); err != nil {
		return nil, nil, err
	}
	profile, tunnel, err := c.toolProfile(*spec.build)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrToolPodRefused, err)
	}
	tools, sandbox, err := spec.Resources.requirements()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrToolPodRefused, err)
	}
	job := c.renderToolPod(spec, profile, tunnel, tools, sandbox)
	urls := []string{spec.tree(ToolSideHead).ArtifactURL, spec.tree(ToolSideBase).ArtifactURL}
	if profile != nil {
		urls = append(urls, profile.ProxyURL)
	}
	hosts, err := endpointHosts(urls)
	if err != nil {
		return nil, nil, err
	}
	aliases, err := c.hostAliases(ctx, hosts)
	if err != nil {
		return nil, nil, err
	}
	pod := &job.Spec.Template.Spec
	pod.DNSPolicy, pod.DNSConfig, pod.HostAliases = corev1.DNSNone, dnsConfig(), aliases
	if err := toolPodShapeRefusal(job, spec, toolPodEnvAllowed(profile, tunnel)); err != nil {
		return nil, nil, err
	}
	return job, toolPodSecret(c.cfg.Namespace, spec), nil
}

// toolPodLabels are the tool pod's Secret's labels, and with the profile
// its Job's and pod's: the agent selector, the intent run kind (the
// network-profile admission policy reads it; no owner label, so no
// IntentRun's mapper picks the Job up), the tools role, the
// verification's UID and the repository-image label.
func toolPodLabels(spec ToolPodSpec, profile *NetworkProfile) map[string]string {
	l := map[string]string{
		labelApp:                          appName,
		labelManagedBy:                    managedBy,
		v1alpha1.LabelRunKind:             kindIntent,
		v1alpha1.LabelRole:                v1alpha1.RoleTools,
		v1alpha1.LabelPreviewVerification: spec.Verification,
		labelRunnerImageSource:            v1alpha1.RunnerImageSourceRepository,
	}
	if profile != nil {
		l[netprofile.LabelKey(profile.Name)] = netprofile.LabelValue
	}
	return l
}

// renderToolPod is the Job, before name resolution.
func (c *Client) renderToolPod(spec ToolPodSpec, profile *NetworkProfile, tunnel []corev1.EnvVar,
	tools, sandbox corev1.ResourceRequirements) *batchv1.Job {
	jobDeadline, podDeadline, _ := toolpod.Deadlines(spec.Timeout, spec.DeadlineMax)
	lbls := toolPodLabels(spec, profile)
	ann := map[string]string{
		annotationRepo:              spec.Repo,
		annotationRunnerImage:       spec.Image,
		annotationRunnerImageSource: v1alpha1.RunnerImageSourceRepository,
		annotationToolsImage:        spec.ToolsImage,
	}
	if spec.AttributedTo != "" {
		ann[v1alpha1.AnnotationAttributedTo] = spec.AttributedTo
	}
	inits := make([]corev1.Container, 0, len(toolInitNames))
	inits = append(inits, c.toolPrepare(spec, tools))
	for _, side := range toolSides {
		inits = append(inits, toolSeed(spec, side, tools))
	}
	for _, side := range toolSides {
		inits = append(inits, toolSandbox(spec, side, profile, tunnel, sandbox))
	}
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: c.cfg.Namespace, Labels: lbls,
			Annotations: maps.Clone(ann)},
		Spec: batchv1.JobSpec{
			BackoffLimit: new(int32(0)), Parallelism: new(int32(1)), Completions: new(int32(1)),
			ActiveDeadlineSeconds: new(int64(jobDeadline / time.Second)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: maps.Clone(lbls), Annotations: podAnnotations(ann)},
				Spec: corev1.PodSpec{
					RestartPolicy:                 corev1.RestartPolicyNever,
					ActiveDeadlineSeconds:         new(int64(podDeadline / time.Second)),
					TerminationGracePeriodSeconds: new(int64(toolGrace)),
					ServiceAccountName:            "default",
					AutomountServiceAccountToken:  new(false),
					EnableServiceLinks:            new(false),
					ShareProcessNamespace:         new(false),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   new(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					InitContainers: inits,
					Containers:     []corev1.Container{toolsMain(spec, tools)},
					Volumes:        toolVolumes(spec),
				},
			},
		},
	}
}

// toolPrepare is the trusted init: fetch, verify and extract both trees,
// copy tool-exec into maul-bin, then the strict probe over every target.
func (c *Client) toolPrepare(spec ToolPodSpec, res corev1.ResourceRequirements) corev1.Container {
	head, base := spec.tree(ToolSideHead), spec.tree(ToolSideBase)
	mounts := make([]corev1.VolumeMount, 0, len(toolSides)+2)
	for _, side := range toolSides {
		mounts = append(mounts, corev1.VolumeMount{Name: "trees-" + side, MountPath: toolTreesDir + "/" + side})
	}
	mounts = append(mounts,
		corev1.VolumeMount{Name: "maul-bin", MountPath: toolMaulBinDir},
		corev1.VolumeMount{Name: "prepare-tmp", MountPath: "/tmp"})
	return corev1.Container{
		Name: initContainerName, Image: spec.ToolsImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Args: []string{
			"prepare",
			"--head-url=" + head.ArtifactURL, "--head-sha256=" + head.ArtifactDigest,
			"--base-url=" + base.ArtifactURL, "--base-sha256=" + base.ArtifactDigest,
			"--trees=" + toolTreesDir, "--bin=" + toolMaulBinDir,
		},
		Env: []corev1.EnvVar{
			{Name: sandboxprobe.TimeoutEnv, Value: c.cfg.SandboxProbeTimeout.String()},
			{Name: sandboxprobe.DNSEnv, Value: c.cfg.SandboxProbeDNS},
		},
		WorkingDir:      "/",
		VolumeMounts:    mounts,
		Resources:       res,
		SecurityContext: toolpod.Security(toolsUID),
	}
}

// toolSeed copies side's pristine tree into its work volume, under the
// sandbox's UID, mounting nothing else.
func toolSeed(spec ToolPodSpec, side string, res corev1.ResourceRequirements) corev1.Container {
	return corev1.Container{
		Name: "seed-" + side, Image: spec.ToolsImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Args:       []string{"seed", "--side=" + side},
		WorkingDir: "/",
		VolumeMounts: []corev1.VolumeMount{
			{Name: "trees-" + side, MountPath: toolTreesDir + "/" + side, ReadOnly: true},
			{Name: "work-" + side, MountPath: toolWorkDir},
		},
		Resources:       res,
		SecurityContext: toolpod.Security(sideUID[side]),
	}
}

// toolSandbox is side's sandbox: the repository image running tool-exec,
// a native sidecar, with the build's profile environment.
func toolSandbox(spec ToolPodSpec, side string, profile *NetworkProfile, tunnel []corev1.EnvVar,
	res corev1.ResourceRequirements) corev1.Container {
	env := []corev1.EnvVar{
		{Name: "HOME", Value: toolHomeDir},
		{Name: "TMPDIR", Value: "/tmp"},
		{Name: "PATH", Value: spec.SearchPath},
		{Name: "GOCACHE", Value: toolCacheDir + "/go-build"},
		{Name: "NPM_CONFIG_CACHE", Value: toolCacheDir + "/npm"},
		{Name: "GOTOOLCHAIN", Value: "local"},
		// No profile, no network: the image's baked module cache only.
		{Name: "GOPROXY", Value: "off"},
	}
	if profile != nil {
		env = applyProfileEnv(env, profile, tunnel)
	}
	return corev1.Container{
		Name: ToolSandbox(side), Image: spec.Image, ImagePullPolicy: corev1.PullIfNotPresent,
		Command: []string{toolExecBin},
		Args: []string{
			"--socket=" + toolExecDir + "/" + toolExecSocket, "--workdir=" + toolWorkDir,
			"--home=" + toolHomeDir, "--cache-dir=" + toolCacheDir,
		},
		Env:           env,
		WorkingDir:    "/",
		RestartPolicy: new(corev1.ContainerRestartPolicyAlways),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "work-" + side, MountPath: toolWorkDir},
			{Name: "cache-" + side, MountPath: toolCacheDir},
			{Name: "home-" + side, MountPath: toolHomeDir},
			{Name: "tmp-" + side, MountPath: "/tmp"},
			{Name: "exec-" + side, MountPath: toolExecDir},
			{Name: "maul-bin", MountPath: toolMaulBinDir, ReadOnly: true},
			{Name: "shm-" + side, MountPath: toolShmDir},
		},
		Resources:       res,
		SecurityContext: toolpod.Security(sideUID[side]),
	}
}

// toolsMain is the relay: Ready once it serves the evaluator on ToolsPort;
// its stdout and termination message are the ledger.
func toolsMain(spec ToolPodSpec, res corev1.ResourceRequirements) corev1.Container {
	mounts := make([]corev1.VolumeMount, 0, 1+2*len(toolSides)+2)
	mounts = append(mounts, corev1.VolumeMount{Name: "tools-secret", MountPath: toolsConfigDir, ReadOnly: true})
	for _, side := range toolSides {
		mounts = append(mounts, corev1.VolumeMount{Name: "trees-" + side, MountPath: toolTreesDir + "/" + side,
			ReadOnly: true})
	}
	for _, side := range toolSides {
		mounts = append(mounts, corev1.VolumeMount{Name: "exec-" + side, MountPath: toolExecDir + "/" + side})
	}
	mounts = append(mounts,
		corev1.VolumeMount{Name: "tools-tmp", MountPath: "/tmp"},
		corev1.VolumeMount{Name: "shm-tools", MountPath: toolShmDir})
	return corev1.Container{
		Name: toolsContainerName, Image: spec.ToolsImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Ports: []corev1.ContainerPort{{Name: "tools", ContainerPort: ToolsPort, Protocol: corev1.ProtocolTCP}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: toolsReadyPath,
				Port: intstr.FromInt32(ToolsPort)}},
			PeriodSeconds: 2, FailureThreshold: 1,
		},
		TerminationMessagePath:   toolsTerminationPath,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		VolumeMounts:             mounts,
		Resources:                res,
		SecurityContext:          toolpod.Security(toolsUID),
	}
}

// toolVolumes are the pod's volumes: the tools Secret, then bounded
// emptyDirs, each side's apart.
func toolVolumes(spec ToolPodSpec) []corev1.Volume {
	empty := func(name, size string, medium corev1.StorageMedium) corev1.Volume {
		q := resource.MustParse(toolSizes[size])
		return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: medium, SizeLimit: &q}}}
	}
	vols := make([]corev1.Volume, 0, 5+7*len(toolSides))
	vols = append(vols,
		corev1.Volume{Name: "tools-secret", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: spec.Name + ToolsSecretSuffix, DefaultMode: new(int32(0o444))}}},
		empty("tools-tmp", "tools-tmp", ""),
		empty("shm-tools", "shm", corev1.StorageMediumMemory),
		empty("prepare-tmp", "prepare-tmp", ""),
		empty("maul-bin", "maul-bin", ""),
	)
	for _, side := range toolSides {
		vols = append(vols,
			empty("trees-"+side, "trees", ""),
			empty("work-"+side, "work", ""),
			empty("cache-"+side, "cache", ""),
			empty("home-"+side, "home", ""),
			empty("tmp-"+side, "tmp", ""),
			empty("exec-"+side, "exec", ""),
			empty("shm-"+side, "shm", corev1.StorageMediumMemory))
	}
	return vols
}

// toolPodSecret is the tools Secret: the relay's data and the key,
// immutable. CreateToolPod owns it by the Job.
func toolPodSecret(namespace string, spec ToolPodSpec) *corev1.Secret {
	data := make(map[string][]byte, len(spec.Data)+1)
	for k, v := range spec.Data {
		data[k] = []byte(v)
	}
	data[ToolsKeyFile] = []byte(spec.Key)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name + ToolsSecretSuffix, Namespace: namespace,
			Labels: toolPodLabels(spec, nil)},
		Immutable: new(true),
		Type:      corev1.SecretTypeOpaque,
		Data:      data,
	}
}

// requirements are the tools and sandbox containers' requests and limits,
// equal, the defaults filling each empty field.
func (r ToolPodResources) requirements() (tools, sandbox corev1.ResourceRequirements, err error) {
	if tools, err = r.Tools.requirements(DefaultToolsResources); err != nil {
		return tools, sandbox, fmt.Errorf("tools resources: %w", err)
	}
	if sandbox, err = r.Sandbox.requirements(DefaultSandboxResources); err != nil {
		return tools, sandbox, fmt.Errorf("sandbox resources: %w", err)
	}
	return tools, sandbox, nil
}

func (r ToolResources) requirements(def ToolResources) (corev1.ResourceRequirements, error) {
	pick := func(v, d string) string {
		if v == "" {
			return d
		}
		return v
	}
	list, err := resourceList(pick(r.CPU, def.CPU), pick(r.Memory, def.Memory),
		pick(r.EphemeralStorage, def.EphemeralStorage))
	if err != nil {
		return corev1.ResourceRequirements{}, err
	}
	return corev1.ResourceRequirements{Requests: list, Limits: list.DeepCopy()}, nil
}

// toolPodSpecRefusal reports why a tool pod must not be built from spec,
// or nil: repository images off, a name, label, image, tree, key, data,
// timeout or search path out of grammar, no resolver for the strict
// probe, or a build that is not this repository's intent build on this
// image (or that buildJob would refuse for its profile).
func (c *Client) toolPodSpecRefusal(spec ToolPodSpec) error {
	var errs []error
	refuse := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !c.cfg.AllowRepositoryImages {
		refuse("repository images are off; a tool pod runs the repository's image")
	}
	if err := validProbeDNS(c.cfg.SandboxProbeDNS); err != nil {
		refuse("%v", err)
	}
	if e := validation.IsDNS1123Label(spec.Name); len(e) > 0 || len(spec.Name) > ToolPodNameMax {
		refuse("name %q is not a DNS label of at most %d characters", spec.Name, ToolPodNameMax)
	}
	if e := validation.IsValidLabelValue(spec.Verification); spec.Verification == "" || len(e) > 0 {
		refuse("verification UID %q is not a label value", spec.Verification)
	}
	if owner, name, ok := strings.Cut(spec.Repo, "/"); !ok || owner == "" || name == "" {
		refuse("repository %q is not owner/name", spec.Repo)
	}
	errs = append(errs, toolImageRefusals(spec)...)
	errs = append(errs, toolTreeRefusals(spec.Trees)...)
	if !toolwire.IsKey(spec.Key) {
		refuse("the key is not %d lower-case hex digits", 2*toolpod.KeyBytes)
	}
	errs = append(errs, toolDataRefusals(spec.Data)...)
	switch {
	case spec.Timeout < time.Minute || spec.Timeout > v1alpha1.MaxAgentsVerificationTimeout:
		refuse("timeout %s is outside [1m, %s]", spec.Timeout, v1alpha1.MaxAgentsVerificationTimeout)
	case spec.DeadlineMax < spec.Timeout:
		refuse("the deadline bound %s is below the timeout %s", spec.DeadlineMax, spec.Timeout)
	}
	if _, _, err := spec.Resources.requirements(); err != nil {
		refuse("%v", err)
	}
	if spec.AttributedTo != "" && spec.AttributedTo != attribution.ValueOrg {
		refuse("attributed to %q; a tool pod is only ever the org's", spec.AttributedTo)
	}
	errs = append(errs, c.toolBuildRefusals(spec)...)
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrToolPodRefused, errors.Join(errs...))
}

// toolImageRefusals are why the images or the sandbox PATH cannot run:
// either image not pinned by digest, one image for both, a search path
// with a relative entry.
func toolImageRefusals(spec ToolPodSpec) []error {
	var errs []error
	for what, img := range map[string]string{"tools": spec.ToolsImage, "sandbox": spec.Image} {
		if ref, err := runnerimage.ParseDeclared(img); err != nil || ref.Digest == "" {
			errs = append(errs, fmt.Errorf("%s image %q is not pinned to a sha256 digest", what, img))
		}
	}
	if spec.Image == spec.ToolsImage {
		errs = append(errs, errors.New("the sandboxes run the repository's image, never maul's tools image"))
	}
	if err := validSearchPath(spec.SearchPath); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// toolBuildRefusals are why spec's build cannot give the tool pod its
// network: none given, another repository, image or kind of Job, an
// unconfigured harness, or a profile buildJob would refuse.
func (c *Client) toolBuildRefusals(spec ToolPodSpec) []error {
	b := spec.build
	if b == nil {
		return []error{errors.New("no build given: the tool pod's profile comes only from its build (ProfileOf)")}
	}
	var errs []error
	refuse := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if b.Repo != spec.Repo {
		refuse("the build is of %q, the tool pod of %q", b.Repo, spec.Repo)
	}
	if b.Phase != string(agentrun.PhaseBuild) || b.Kind != kindIntent {
		refuse("the profile comes from an intent build Job, not a %s Job of kind %s", b.Phase, b.Kind)
	}
	if b.RunnerImage != spec.Image {
		refuse("the build runs %q, the sandboxes %q", b.RunnerImage, spec.Image)
	}
	if _, err := c.runnerFor(b.Harness); err != nil {
		refuse("%v", err)
	}
	if err := profileRefusal(*b); err != nil {
		refuse("%v", err)
	}
	return errs
}

// toolTreeRefusals are why trees are not exactly the head and the base of
// one repository, each a line-safe fetch the prepare init can verify.
func toolTreeRefusals(trees []Tree) []error {
	var errs []error
	refuse := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	keys := make([]string, 0, len(trees))
	for _, t := range trees {
		keys = append(keys, t.Key)
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{ToolSideBase, ToolSideHead}) {
		return append(errs, fmt.Errorf("trees %v are not exactly %s and %s", keys, ToolSideHead, ToolSideBase))
	}
	for _, t := range trees {
		if !lineSafeURL(t.URL, "https://") {
			refuse("%s URL %q is not an https URL without whitespace", t.Key, t.URL)
		}
		if !lineSafeURL(t.ArtifactURL, "http://", "https://") {
			refuse("%s artifact URL %q is not an http(s) URL without whitespace", t.Key, t.ArtifactURL)
		}
		if !treeDigest.MatchString(t.ArtifactDigest) {
			refuse("%s artifact digest %q is not 64 hex characters", t.Key, t.ArtifactDigest)
		}
		if !treeCommit.MatchString(t.BaseSHA) {
			refuse("%s commit %q is not a commit SHA", t.Key, t.BaseSHA)
		}
	}
	if trees[0].URL != trees[1].URL {
		refuse("head %q and base %q are not one repository", trees[0].URL, trees[1].URL)
	}
	if trees[0].BaseSHA == trees[1].BaseSHA {
		refuse("head and base are one commit %s", trees[0].BaseSHA)
	}
	return errs
}

// toolDataRefusals are why data cannot be the tools Secret's: no relay
// config, the key's own name, or a key a Secret cannot hold.
func toolDataRefusals(data map[string]string) []error {
	var errs []error
	if _, ok := data[toolsConfigFile]; !ok {
		errs = append(errs, fmt.Errorf("the relay's data has no %s", toolsConfigFile))
	}
	for _, k := range slices.Sorted(maps.Keys(data)) {
		if k == ToolsKeyFile {
			errs = append(errs, fmt.Errorf("the relay's data names %s, which holds the key", k))
		}
		if e := validation.IsConfigMapKey(k); len(e) > 0 {
			errs = append(errs, fmt.Errorf("data key %q: %s", k, strings.Join(e, "; ")))
		}
	}
	return errs
}

// validSearchPath admits ":"-joined absolute entries with no control
// character, as runnerimage.SanitizePath writes them.
func validSearchPath(p string) error {
	if p == "" || len(p) > 4096 {
		return fmt.Errorf("search path %q is empty or over 4096 bytes", p)
	}
	for e := range strings.SplitSeq(p, ":") {
		if !strings.HasPrefix(e, "/") || strings.ContainsFunc(e, isSpaceOrControl) {
			return fmt.Errorf("search path entry %q is not an absolute path", e)
		}
	}
	return nil
}

// toolPodEnvAllowed are the reserved names a tool pod may carry, in a
// sandbox only: HOME and, with a profile, the names the profile's render
// owns (profileEnv's and the tunnel's).
func toolPodEnvAllowed(p *NetworkProfile, tunnel []corev1.EnvVar) map[string]bool {
	allowed := map[string]bool{"HOME": true}
	if p == nil {
		return allowed
	}
	for _, e := range slices.Concat(profileEnv(p), tunnel) {
		allowed[e.Name] = true
	}
	return allowed
}

// toolInitNames are the tool pod's init containers, in order: the
// classic inits, then the two native sidecars.
var toolInitNames = []string{initContainerName, "seed-" + ToolSideHead, "seed-" + ToolSideBase,
	"sandbox-" + ToolSideHead, "sandbox-" + ToolSideBase}

// toolPodShapeRefusal checks a rendered tool pod against what the builder
// promises, so no later edit of the builder can ship one that breaks it:
// no ServiceAccount token, host namespace, resolver, preview placement or
// shared process namespace; exactly its containers on exactly its two
// digests, each read-only and unprivileged under its UID (tools and the
// sandboxes pairwise distinct, each seed its sandbox's) with workingDir /
// on every init and sandbox; no env from elsewhere, no credential channel,
// no reserved name outside a sandbox's allowed ones; no volume but its
// Secret and emptyDirs, the Secret in tools alone; tools mounting no work
// volume and, of the sandboxes' volumes, only the exec-socket directories
// writable; a /dev/shm of its own in tools and each sandbox.
func toolPodShapeRefusal(job *batchv1.Job, spec ToolPodSpec, allowed map[string]bool) error {
	pod := job.Spec.Template.Spec
	errs := toolPodLevelRefusals(pod)
	errs = append(errs, toolVolumeRefusals(pod, spec)...)
	errs = append(errs, toolContainerRefusals(pod, spec, allowed)...)
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrToolPodRefused, errors.Join(errs...))
}

// toolPodLevelRefusals: the pod's own fields, its identity, namespaces,
// posture, resolver and placement.
func toolPodLevelRefusals(pod corev1.PodSpec) []error {
	var errs []error
	refuse := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		pod.ServiceAccountName != "default" {
		refuse("the pod must run as ServiceAccount default with its token off")
	}
	if pod.ShareProcessNamespace == nil || *pod.ShareProcessNamespace {
		refuse("shareProcessNamespace must be explicitly false")
	}
	if pod.HostNetwork || pod.HostPID || pod.HostIPC || pod.HostUsers != nil && *pod.HostUsers {
		refuse("the pod shares a host namespace")
	}
	if !podSecured(pod.SecurityContext) {
		refuse("the pod must run non-root under the runtime's default seccomp profile")
	}
	if pod.DNSPolicy != corev1.DNSNone {
		refuse("the pod resolves names (dnsPolicy %s)", pod.DNSPolicy)
	}
	if len(pod.Tolerations) > 0 || len(pod.NodeSelector) > 0 || pod.Affinity != nil || pod.NodeName != "" {
		refuse("the tool pod takes no toleration, node selector, affinity or node")
	}
	return errs
}

// toolVolumeRefusals: only the tools Secret and emptyDirs.
func toolVolumeRefusals(pod corev1.PodSpec, spec ToolPodSpec) []error {
	var errs []error
	for _, v := range pod.Volumes {
		switch {
		case v.EmptyDir != nil:
		case v.Secret != nil && v.Secret.SecretName == spec.Name+ToolsSecretSuffix:
		default:
			errs = append(errs, fmt.Errorf("volume %s is neither an emptyDir nor the tools Secret", v.Name))
		}
	}
	return errs
}

// toolContainerRefusals checks the containers: exactly the tool pod's,
// each as toolContainerRefusal holds it, and the mounts across them.
func toolContainerRefusals(pod corev1.PodSpec, spec ToolPodSpec, allowed map[string]bool) []error {
	var errs []error
	inits := make([]string, 0, len(pod.InitContainers))
	for _, c := range pod.InitContainers {
		inits = append(inits, c.Name)
	}
	if !slices.Equal(inits, toolInitNames) {
		errs = append(errs, fmt.Errorf("init containers %v are not %v", inits, toolInitNames))
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Name != toolsContainerName {
		errs = append(errs, fmt.Errorf("the one main container must be %s", toolsContainerName))
	}
	vols := toolVolumeKinds(pod)
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		errs = append(errs, toolContainerRefusal(c, spec, allowed, vols)...)
	}
	errs = append(errs, toolMountRefusals(pod, vols.shm)...)
	return append(errs, toolSideRefusals(pod)...)
}

// toolSideRefusals hold the head/base wall (O2): a seed or sandbox mounts
// no volume of the other side, a seed mounts exactly its pristine tree
// (read-only) and its work volume, and only prepare writes a pristine tree.
func toolSideRefusals(pod corev1.PodSpec) []error {
	var errs []error
	other := map[string]string{ToolSideHead: ToolSideBase, ToolSideBase: ToolSideHead}
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		side := ""
		for _, prefix := range []string{"seed-", "sandbox-"} {
			if s, ok := strings.CutPrefix(c.Name, prefix); ok {
				side = s
			}
		}
		for _, m := range c.VolumeMounts {
			if side != "" && strings.HasSuffix(m.Name, "-"+other[side]) {
				errs = append(errs, fmt.Errorf("%s mounts %s, a volume of the other side", c.Name, m.Name))
			}
			if strings.HasPrefix(m.Name, "trees-") && !m.ReadOnly && c.Name != initContainerName {
				errs = append(errs, fmt.Errorf("%s mounts the pristine tree %s writable", c.Name, m.Name))
			}
		}
		if strings.HasPrefix(c.Name, "seed-") {
			names := make([]string, 0, len(c.VolumeMounts))
			for _, m := range c.VolumeMounts {
				names = append(names, m.Name)
			}
			slices.Sort(names)
			if !slices.Equal(names, []string{"trees-" + side, "work-" + side}) {
				errs = append(errs, fmt.Errorf("%s mounts %v, not exactly its tree and work volume", c.Name, names))
			}
		}
	}
	return errs
}

// toolUIDs are each container's UID.
var toolUIDs = map[string]int64{
	toolsContainerName: toolsUID, initContainerName: toolsUID,
	"seed-" + ToolSideHead: toolHeadUID, "sandbox-" + ToolSideHead: toolHeadUID,
	"seed-" + ToolSideBase: toolBaseUID, "sandbox-" + ToolSideBase: toolBaseUID,
}

// toolContainerRefusal checks one container: its image, workingDir and
// posture, its environment and the volumes it mounts.
func toolContainerRefusal(c corev1.Container, spec ToolPodSpec, allowed map[string]bool, vols toolVols) []error {
	var errs []error
	refuse := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	sandbox := strings.HasPrefix(c.Name, "sandbox-")
	image := spec.ToolsImage
	if sandbox {
		image = spec.Image
	}
	if c.Image != image {
		refuse("%s runs %q, not %q", c.Name, c.Image, image)
	}
	if c.Name != toolsContainerName && c.WorkingDir != "/" {
		refuse("%s has workingDir %q, not /", c.Name, c.WorkingDir)
	}
	if uid, ok := toolUIDs[c.Name]; !ok || !securedAs(c.SecurityContext, uid) {
		refuse("%s is not read-only and unprivileged under its UID %d", c.Name, toolUIDs[c.Name])
	}
	errs = append(errs, toolEnvRefusals(c, sandbox, allowed)...)
	for _, m := range c.VolumeMounts {
		switch {
		case m.Name == vols.secret && c.Name != toolsContainerName:
			refuse("%s mounts the tools Secret", c.Name)
		case !vols.emptyDirs[m.Name] && m.Name != vols.secret:
			refuse("%s mounts the unknown volume %s", c.Name, m.Name)
		}
	}
	return errs
}

// toolEnvRefusals: no env from elsewhere, no credential channel, and no
// reserved name but a sandbox's allowed ones.
func toolEnvRefusals(c corev1.Container, sandbox bool, allowed map[string]bool) []error {
	var errs []error
	if len(c.EnvFrom) > 0 {
		errs = append(errs, fmt.Errorf("%s takes envFrom", c.Name))
	}
	for _, e := range c.Env {
		switch {
		case e.ValueFrom != nil:
			errs = append(errs, fmt.Errorf("%s's %s comes from elsewhere", c.Name, e.Name))
		case credentialChannelEnv[e.Name]:
			errs = append(errs, fmt.Errorf("%s carries the credential channel %s", c.Name, e.Name))
		case reservedEnv[e.Name] && (!sandbox || !allowed[e.Name]):
			errs = append(errs, fmt.Errorf("%s carries the reserved %s", c.Name, e.Name))
		}
	}
	return errs
}

// toolVols names the tools Secret's volume, the emptyDirs, and the memory
// emptyDirs.
type toolVols struct {
	secret         string
	emptyDirs, shm map[string]bool
}

// toolVolumeKinds sorts the pod's volumes.
func toolVolumeKinds(pod corev1.PodSpec) toolVols {
	v := toolVols{emptyDirs: map[string]bool{}, shm: map[string]bool{}}
	for _, vol := range pod.Volumes {
		switch {
		case vol.Secret != nil:
			v.secret = vol.Name
		case vol.EmptyDir != nil:
			v.emptyDirs[vol.Name] = true
			if vol.EmptyDir.Medium == corev1.StorageMediumMemory {
				v.shm[vol.Name] = true
			}
		}
	}
	return v
}

// toolMountRefusals: tools mounts no work volume and only the exec-socket
// directories of the sandboxes' volumes writable; tools and each sandbox
// have a /dev/shm memory emptyDir no other container mounts.
func toolMountRefusals(pod corev1.PodSpec, shm map[string]bool) []error {
	var errs []error
	sandboxVols, mounts := map[string]bool{}, map[string]int{}
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		for _, m := range c.VolumeMounts {
			mounts[m.Name]++
			if strings.HasPrefix(c.Name, "sandbox-") {
				sandboxVols[m.Name] = true
			}
		}
	}
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		hasShm := false
		for _, m := range c.VolumeMounts {
			if m.MountPath == toolShmDir && shm[m.Name] && mounts[m.Name] == 1 {
				hasShm = true
			}
			if c.Name != toolsContainerName {
				continue
			}
			if strings.HasPrefix(m.Name, "work-") {
				errs = append(errs, fmt.Errorf("tools mounts the sandbox work volume %s", m.Name))
			}
			if sandboxVols[m.Name] && !m.ReadOnly && !strings.HasPrefix(m.Name, "exec-") {
				errs = append(errs, fmt.Errorf("tools mounts the sandbox volume %s writable", m.Name))
			}
		}
		if (c.Name == toolsContainerName || strings.HasPrefix(c.Name, "sandbox-")) && !hasShm {
			errs = append(errs, fmt.Errorf("%s has no /dev/shm memory emptyDir of its own", c.Name))
		}
	}
	return errs
}

// podSecured reports whether the pod runs non-root under the runtime's
// default seccomp profile.
func podSecured(sc *corev1.PodSecurityContext) bool {
	return sc != nil && sc.RunAsNonRoot != nil && *sc.RunAsNonRoot && sc.SeccompProfile != nil &&
		sc.SeccompProfile.Type == corev1.SeccompProfileTypeRuntimeDefault
}

// securedAs reports whether sc is toolpod.Security(uid) or stricter where
// it matters: that UID and group, non-root, read-only, no escalation, no
// capability, not privileged.
func securedAs(sc *corev1.SecurityContext, uid int64) bool {
	return sc != nil && sc.RunAsUser != nil && *sc.RunAsUser == uid && sc.RunAsGroup != nil &&
		*sc.RunAsGroup == uid && sc.RunAsNonRoot != nil && *sc.RunAsNonRoot && sc.ReadOnlyRootFilesystem != nil &&
		*sc.ReadOnlyRootFilesystem && sc.AllowPrivilegeEscalation != nil && !*sc.AllowPrivilegeEscalation &&
		(sc.Privileged == nil || !*sc.Privileged) && (sc.SeccompProfile == nil ||
		sc.SeccompProfile.Type == corev1.SeccompProfileTypeRuntimeDefault) && sc.Capabilities != nil &&
		slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) && len(sc.Capabilities.Add) == 0
}
