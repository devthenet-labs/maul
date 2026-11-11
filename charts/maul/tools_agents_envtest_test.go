// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/kube"
	"github.com/devthenet-labs/maul/internal/rbactest"
	"github.com/devthenet-labs/maul/internal/toolpod"
)

// maul-tools-pod-shape's refusals, each distinct so a case cannot pass on
// the wrong rule.
const (
	tpCreator   = "only intent-controller creates an agents tool pod Job"
	tpLabels    = "agents tool pods carry the agent pod labels and their verification"
	tpToken     = "agents tool pods mount no ServiceAccount token"
	tpHost      = "agents tool pods share no host or process namespace"
	tpShape     = "agents tool pods run tools and the init containers prepare, seed-head and seed-base"
	tpSecurity  = "agents tool pod containers run as a set non-root UID"
	tpUIDs      = "agents tool pods run tools, sandbox-head and sandbox-base as three distinct UIDs"
	tpWorkdir   = "agents tool pod init containers and sandboxes start in /"
	tpCredent   = "agents tool pods carry no credential"
	tpVolumes   = "agents tool pods mount only emptyDirs and their own <job>-tools Secret"
	tpShm       = "agents tool pods give tools, sandbox-head and sandbox-base each their own memory-backed /dev/shm"
	tpSides     = "the head and base sides of an agents tool pod share no volume but the sandboxes' read-only"
	tpToolsSide = "tools shares no writable volume with a seed or sandbox but that sandbox's exec-socket directory"
	tpTolerate  = "agents tool pods tolerate neither the preview taint nor every taint"
	tpSeccomp   = "agents tool pods run under the runtime's default seccomp profile"
	tpSeeds     = "each agents tool pod seed mounts exactly its pristine tree, read-only, and its sandbox's work volume"
	pvByPlace   = "preview-controller writes a slot verification's results; intent-controller an agents verification's"
	tpRepoImage = "registry.example/acme/render-agent@sha256:" +
		"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	tpTaintKey = "maul.devthe.net/preview-only"
)

// agentsEnvArgs renders slot verification with the evaluator and the
// agents-namespace tool pod both on: every policy each placement adds.
var agentsEnvArgs = slices.Concat(toolsEnvArgs, []string{
	"-f", "../../hack/testdata/chart-render/repository-images.yaml",
	"--set", "intentController.verification.agents.enabled=true",
})

// TestAgentsToolPodPoliciesAgainstAPIServer applies the chart rendered with
// both tool pod placements on to a real API server and drives
// maul-tools-pod-shape and the per-placement verification writers as the
// identities they judge: the tool pod S2-10 builds is admitted from
// intent-controller's Job and the Job controller's Pod, and every shape it
// must never take is refused by its own rule; a slot verification's results
// are preview-controller's and an agents verification's intent-controller's.
// Every writer holds the verbs, so each refusal is admission's.
func TestAgentsToolPodPoliciesAgainstAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via mise run envtest")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	admin, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	release := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: egressRelease}}
	if err := admin.Create(t.Context(), release); err != nil {
		t.Fatalf("release namespace: %v", err)
	}
	applySlotRender(t, admin, agentsEnvArgs)
	grantEvaluatorLabelTesters(t, admin)
	as := func(user string) client.Client {
		c := rest.CopyConfig(cfg)
		c.Impersonate = rest.ImpersonationConfig{UserName: user}
		cl, err := client.New(c, client.Options{Scheme: kube.Scheme()})
		if err != nil {
			t.Fatalf("client as %s: %v", user, err)
		}
		return cl
	}
	waitVerificationGrants(t, admin)
	rbactest.WaitAllowed(t, admin, rbactest.Verbs(saIntent, nil, authorizationv1.ResourceAttributes{
		Namespace: egressRelease, Group: v1alpha1.GroupVersion.Group, Resource: "previewverifications",
		Subresource: "status"}, "update")...)
	ic := as(saIntent)
	waitFor(t, func() error {
		for _, probe := range []struct {
			obj  client.Object
			want string
		}{
			{mutateJob(agentsToolJob("probe"), func(j *batchv1.Job) {
				j.Spec.Template.Spec.Containers[0].Image = toolboxImage
			}), tpShape},
			{agentsVerification("probe"), ""},
		} {
			err := ic.Create(t.Context(), probe.obj.DeepCopyObject().(client.Object), client.DryRunAll)
			if probe.want == "" {
				if err != nil {
					return err
				}
				continue
			}
			if !deniedWith(err, probe.want) {
				return &waitError{err: err, want: probe.want}
			}
		}
		return nil
	})
	t.Run("jobs", func(t *testing.T) { checkAgentsToolJobs(t, admin, ic) })
	t.Run("pods", func(t *testing.T) { checkAgentsToolPods(t, admin, ic, as(saJobController)) })
	t.Run("verification writers", func(t *testing.T) {
		checkPlacementWriters(t, admin, as(saPreview), ic)
	})
}

func checkAgentsToolJobs(t *testing.T, admin, ic client.Client) {
	admittedAs(t, ic, agentsToolJob("tools"))
	admittedAs(t, ic, mutateJob(agentsToolJob("profiled"), func(j *batchv1.Job) {
		j.Spec.Template.Labels["maul.run/network-go"] = "true"
		for _, i := range []int{3, 4} {
			j.Spec.Template.Spec.InitContainers[i].Env = append(j.Spec.Template.Spec.InitContainers[i].Env,
				corev1.EnvVar{Name: "GOPROXY", Value: "http://maul-egress-proxy.maul.svc:8090"},
				corev1.EnvVar{Name: "HTTPS_PROXY", Value: "http://maul-egress-proxy.maul.svc:8090"})
		}
	}))
	if err := admin.Create(t.Context(), agentsToolJob("operator"), client.DryRunAll); !deniedWith(err, tpCreator,
		evWriterDenied) {
		t.Errorf("a tool Job by anyone but intent-controller: %v, want refused", err)
	}
	yes, no := true, false
	other := "registry.example/acme/other@sha256:" + strings.Repeat("d", 64)
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.PodSpec)
		want   string
	}{
		{"no verification label", nil, tpLabels},
		{"a projected token", func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "token", VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}}}}})
		}, tpVolumes},
		{"a hostPath", func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/var/run"}}})
		}, tpVolumes},
		{"another Job's Secret", func(p *corev1.PodSpec) { volume(p, "tools-secret").Secret.SecretName = "x-tools" },
			tpVolumes},
		{"the Secret in a sandbox", func(p *corev1.PodSpec) {
			mount(p, "sandbox-head", corev1.VolumeMount{Name: "tools-secret", MountPath: "/key", ReadOnly: true})
		}, tpVolumes},
		{"the token automounted", func(p *corev1.PodSpec) { p.AutomountServiceAccountToken = &yes }, tpToken},
		{"automount unset", func(p *corev1.PodSpec) { p.AutomountServiceAccountToken = nil }, tpToken},
		{"another ServiceAccount", func(p *corev1.PodSpec) { p.ServiceAccountName = "maul-agent" }, tpToken},
		{"a shared process namespace", func(p *corev1.PodSpec) { p.ShareProcessNamespace = &yes }, tpHost},
		{"the host network", func(p *corev1.PodSpec) { p.HostNetwork = true }, tpHost},
		{"a resolver", func(p *corev1.PodSpec) { p.DNSPolicy, p.DNSConfig = corev1.DNSClusterFirst, nil }, tpHost},
		{"service links", func(p *corev1.PodSpec) { p.EnableServiceLinks = &yes }, tpHost},
		{"the tools image by tag", func(p *corev1.PodSpec) {
			p.Containers[0].Image = "ghcr.io/devthenet-labs/maul/preview-tools:v1"
		}, tpShape},
		{"a third image in a seed", func(p *corev1.PodSpec) { p.InitContainers[1].Image = other }, tpShape},
		{"two repository images", func(p *corev1.PodSpec) { p.InitContainers[4].Image = other }, tpShape},
		{"the sandboxes in the tools image", func(p *corev1.PodSpec) {
			p.InitContainers[3].Image, p.InitContainers[4].Image = toolsImage, toolsImage
		}, tpShape},
		{"the repository image by tag", func(p *corev1.PodSpec) {
			p.InitContainers[3].Image, p.InitContainers[4].Image = "registry.example/acme/render-agent:v1",
				"registry.example/acme/render-agent:v1"
		}, tpShape},
		{"a fourth init", func(p *corev1.PodSpec) {
			extra := p.InitContainers[1]
			extra.Name, extra.VolumeMounts = "seed-extra", nil
			p.InitContainers = append(p.InitContainers[:3], append([]corev1.Container{extra},
				p.InitContainers[3:]...)...)
		}, tpShape},
		{"no seed-base", func(p *corev1.PodSpec) {
			p.InitContainers = slices.Delete(p.InitContainers, 2, 3)
		}, tpShape},
		{"a seed after the sandboxes", func(p *corev1.PodSpec) {
			p.InitContainers = []corev1.Container{p.InitContainers[0], p.InitContainers[1], p.InitContainers[3],
				p.InitContainers[4], p.InitContainers[2]}
		}, tpShape},
		{"a sandbox as a plain init", func(p *corev1.PodSpec) { p.InitContainers[3].RestartPolicy = nil }, tpShape},
		{"a second main container", func(p *corev1.PodSpec) {
			c := p.Containers[0]
			c.Name, c.VolumeMounts = "second", nil
			p.Containers = append(p.Containers, c)
		}, tpShape},
		{"a seed as another UID", func(p *corev1.PodSpec) {
			p.InitContainers[1].SecurityContext = toolpod.Security(65550)
		}, tpUIDs},
		{"a sandbox as tools", func(p *corev1.PodSpec) {
			p.InitContainers[1].SecurityContext = toolpod.Security(65532)
			p.InitContainers[3].SecurityContext = toolpod.Security(65532)
		}, tpUIDs},
		{"both sides as one UID", func(p *corev1.PodSpec) {
			p.InitContainers[2].SecurityContext = toolpod.Security(65540)
			p.InitContainers[4].SecurityContext = toolpod.Security(65540)
		}, tpUIDs},
		{"prepare as a sandbox", func(p *corev1.PodSpec) {
			p.InitContainers[0].SecurityContext = toolpod.Security(65540)
		}, tpUIDs},
		{"a privileged sandbox", func(p *corev1.PodSpec) {
			p.InitContainers[3].SecurityContext.Privileged = &yes
			p.InitContainers[3].SecurityContext.AllowPrivilegeEscalation = nil
		}, tpSecurity},
		{"a writable root", func(p *corev1.PodSpec) {
			p.InitContainers[4].SecurityContext.ReadOnlyRootFilesystem = &no
		}, tpSecurity},
		{"a capability kept", func(p *corev1.PodSpec) {
			p.Containers[0].SecurityContext.Capabilities.Drop = nil
		}, tpSecurity},
		{"no UID", func(p *corev1.PodSpec) { p.InitContainers[0].SecurityContext.RunAsUser = nil }, tpSecurity},
		{"a sandbox not in /", func(p *corev1.PodSpec) { p.InitContainers[4].WorkingDir = "" }, tpWorkdir},
		{"prepare not in /", func(p *corev1.PodSpec) { p.InitContainers[0].WorkingDir = "/work" }, tpWorkdir},
		{"an envFrom", func(p *corev1.PodSpec) {
			p.InitContainers[3].EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "env"}}}}
		}, tpCredent},
		{"a Secret in env", func(p *corev1.PodSpec) {
			p.Containers[0].Env = append(p.Containers[0].Env, corev1.EnvVar{Name: "KEY", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "k"},
					Key: "k"}}})
		}, tpCredent},
		{"a model key", func(p *corev1.PodSpec) {
			p.InitContainers[4].Env = append(p.InitContainers[4].Env,
				corev1.EnvVar{Name: "ANTHROPIC_API_KEY", Value: "sk-ant-x"})
		}, tpCredent},
		{"the broker token path", func(p *corev1.PodSpec) {
			p.InitContainers[3].Env = append(p.InitContainers[3].Env,
				corev1.EnvVar{Name: "MAUL_BROKER_TOKEN_FILE", Value: "/var/run/maul/broker/token"})
		}, tpCredent},
		{"no /dev/shm in tools", func(p *corev1.PodSpec) { unmount(p, "tools", "shm-tools") }, tpShm},
		{"one /dev/shm for both sides", func(p *corev1.PodSpec) {
			unmount(p, "sandbox-base", "shm-base")
			mount(p, "sandbox-base", corev1.VolumeMount{Name: "shm-head", MountPath: "/dev/shm"})
		}, tpShm},
		{"a disk-backed /dev/shm", func(p *corev1.PodSpec) { volume(p, "shm-head").EmptyDir.Medium = "" }, tpShm},
		{"a head cache in the base sandbox", func(p *corev1.PodSpec) {
			mount(p, "sandbox-base", corev1.VolumeMount{Name: "cache-head", MountPath: "/head-cache"})
		}, tpSides},
		{"the base work in the head seed", func(p *corev1.PodSpec) {
			mount(p, "seed-head", corev1.VolumeMount{Name: "work-base", MountPath: "/base", ReadOnly: true})
		}, tpSides},
		{"a pristine tree writable in a sandbox", func(p *corev1.PodSpec) {
			mount(p, "sandbox-head", corev1.VolumeMount{Name: "pristine-head", MountPath: "/pristine"})
		}, tpToolsSide},
		{"a pristine tree writable in tools", func(p *corev1.PodSpec) {
			unmount(p, "tools", "pristine-base")
			mount(p, "tools", corev1.VolumeMount{Name: "pristine-base", MountPath: "/pristine/base"})
		}, tpToolsSide},
		{"a sandbox work volume in tools", func(p *corev1.PodSpec) {
			mount(p, "tools", corev1.VolumeMount{Name: "work-head", MountPath: "/work-head"})
		}, tpToolsSide},
		{"a sandbox work volume read-only in tools", func(p *corev1.PodSpec) {
			mount(p, "tools", corev1.VolumeMount{Name: "work-base", MountPath: "/work-base", ReadOnly: true})
		}, tpToolsSide},
		{"an exec directory in a seed", func(p *corev1.PodSpec) {
			mount(p, "seed-base", corev1.VolumeMount{Name: "exec-base", MountPath: "/run/maul-exec"})
		}, tpToolsSide},
		// jobs.toolSideRefusals and podSecured (S2-10), mirrored at admission
		// (S2-21): a side reads nothing of the other, a seed mounts nothing
		// but its tree and work volume, only prepare writes a pristine tree,
		// and nothing runs outside the default seccomp profile.
		{"the base pristine tree read-only in the head sandbox", func(p *corev1.PodSpec) {
			mount(p, "sandbox-head", corev1.VolumeMount{Name: "pristine-base", MountPath: "/base", ReadOnly: true})
		}, tpSides},
		{"the head pristine tree read-only in the base seed", func(p *corev1.PodSpec) {
			mount(p, "seed-base", corev1.VolumeMount{Name: "pristine-head", MountPath: "/head", ReadOnly: true})
		}, tpSides},
		// Both sandboxes reading the head's pristine tree read-only, its
		// seed seeding from another of prepare's volumes: the read-only
		// exception is the tool-exec copy alone, at /maul-bin in prepare and
		// both sandboxes.
		{"the head pristine tree read-only in both sandboxes", func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "extra", VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{}}})
			mount(p, "prepare", corev1.VolumeMount{Name: "extra", MountPath: "/extra"})
			unmount(p, "seed-head", "pristine-head")
			mount(p, "seed-head", corev1.VolumeMount{Name: "extra", MountPath: "/pristine", ReadOnly: true})
			for _, c := range []string{"sandbox-head", "sandbox-base"} {
				mount(p, c, corev1.VolumeMount{Name: "pristine-head", MountPath: "/head", ReadOnly: true})
			}
		}, tpSides},
		{"maul-bin elsewhere in the base sandbox", func(p *corev1.PodSpec) {
			unmount(p, "sandbox-base", "maul-bin")
			mount(p, "sandbox-base", corev1.VolumeMount{Name: "maul-bin", MountPath: "/work/bin", ReadOnly: true})
		}, tpSides},
		{"a cache in a seed", func(p *corev1.PodSpec) {
			mount(p, "seed-head", corev1.VolumeMount{Name: "cache-head", MountPath: "/work/cache"})
		}, tpSeeds},
		{"maul-bin in a seed", func(p *corev1.PodSpec) {
			mount(p, "seed-base", corev1.VolumeMount{Name: "maul-bin", MountPath: "/maul-bin", ReadOnly: true})
		}, tpSides},
		{"maul-bin in the seed of its own side alone", func(p *corev1.PodSpec) {
			unmount(p, "sandbox-head", "maul-bin")
			mount(p, "seed-base", corev1.VolumeMount{Name: "maul-bin", MountPath: "/maul-bin", ReadOnly: true})
		}, tpSeeds},
		{"a seed writing its pristine tree", func(p *corev1.PodSpec) {
			unmount(p, "seed-head", "pristine-head")
			mount(p, "seed-head", corev1.VolumeMount{Name: "pristine-head", MountPath: "/pristine"})
		}, tpToolsSide},
		{"a seed without its work volume", func(p *corev1.PodSpec) { unmount(p, "seed-base", "work-base") }, tpSeeds},
		{"an Unconfined sandbox", func(p *corev1.PodSpec) {
			container(p, "sandbox-head").SecurityContext.SeccompProfile = &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeUnconfined}
		}, tpSeccomp},
		{"a Localhost profile in tools", func(p *corev1.PodSpec) {
			container(p, "tools").SecurityContext.SeccompProfile = &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeLocalhost, LocalhostProfile: ptrTo("profiles/open.json")}
		}, tpSeccomp},
		{"no pod seccomp profile", func(p *corev1.PodSpec) { p.SecurityContext.SeccompProfile = nil }, tpSeccomp},
		{"an Unconfined pod", func(p *corev1.PodSpec) {
			p.SecurityContext.SeccompProfile = &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
		}, tpSeccomp},
		{"tolerating every taint", func(p *corev1.PodSpec) {
			p.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
		}, tpTolerate},
		{"tolerating the preview taint", func(p *corev1.PodSpec) {
			p.Tolerations = []corev1.Toleration{{Key: tpTaintKey, Operator: corev1.TolerationOpEqual,
				Value: "true", Effect: corev1.TaintEffectNoExecute}}
		}, tpTolerate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := agentsToolJob("bad")
			if tc.mutate == nil {
				delete(j.Labels, evEvaluation)
				delete(j.Spec.Template.Labels, evEvaluation)
			} else {
				tc.mutate(&j.Spec.Template.Spec)
			}
			refusedAs(t, ic, j, tc.want, tc.name)
		})
	}
	// Shared read-only by the two sandboxes, the repository's tool-exec
	// copy is no channel; a container's own RuntimeDefault is the pod's.
	admittedAs(t, ic, mutateJob(agentsToolJob("explicit-seccomp"), func(j *batchv1.Job) {
		for _, n := range []string{"tools", "sandbox-head"} {
			container(&j.Spec.Template.Spec, n).SecurityContext.SeccompProfile = &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault}
		}
	}))
	// The tool pods jobs.CreateToolPod renders (its goldens) are admitted as
	// they are: the policy refuses no shape the builder makes.
	for _, golden := range []string{"toolpod_default", "toolpod_profile_go_tunnel"} {
		admittedAs(t, ic, builderToolJob(t, golden))
	}
	// An ordinary agent Job in the agents namespace is not a tool pod.
	admittedAs(t, ic, evJob(egressAgents, "ordinary", nil, nil))
}

func checkAgentsToolPods(t *testing.T, admin, ic, jobController client.Client) {
	ctx := t.Context()
	owner := []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "tools", UID: "u-job",
		Controller: ptrTo(true)}}
	pod := agentsToolPod("tools-abcde", owner)
	// maul-evaluator-labels refuses both too; either refusal will do.
	if err := ic.Create(ctx, pod.DeepCopy(), client.DryRunAll); !deniedWith(err, tpCreator, evPodDenied) {
		t.Errorf("a tool pod by intent-controller: %v, want refused", err)
	}
	if err := jobController.Create(ctx, agentsToolPod("orphan", nil), client.DryRunAll); !deniedWith(err,
		tpCreator, evPodDenied) {
		t.Errorf("a tool pod with no Job: %v, want refused", err)
	}
	foreign := agentsToolPod("foreign", owner)
	volume(&foreign.Spec, "tools-secret").Secret.SecretName = "other-tools"
	refusedAs(t, jobController, foreign, tpVolumes, "a tool pod mounting another Job's Secret")
	if err := jobController.Create(ctx, pod); err != nil {
		t.Fatalf("the Job controller creating the tool pod: %v", err)
	}
	// A metadata-only update (a finalizer's removal) passes.
	pod.Finalizers = []string{"batch.kubernetes.io/job-tracking"}
	if err := jobController.Update(ctx, pod, client.DryRunAll); err != nil {
		t.Errorf("a metadata-only update: %v", err)
	}
	debug := pod.DeepCopy()
	debug.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name: "debug", Image: toolsImage, SecurityContext: toolpod.Security(65532)}}}
	// Anyone but the Job controller is refused by the writer rule first;
	// the shape refuses the ephemeral container itself, even from it.
	if err := admin.SubResource("ephemeralcontainers").Update(ctx, debug.DeepCopy()); !deniedWith(err,
		tpCreator) {
		t.Errorf("an ephemeral container in the tool pod by the operator: %v, want %q", err, tpCreator)
	}
	grantEphemeral(t, admin)
	if err := jobController.SubResource("ephemeralcontainers").Update(ctx, debug); !deniedWith(err, tpHost) {
		t.Errorf("an ephemeral container in the tool pod: %v, want %q", err, tpHost)
	}
}

func checkPlacementWriters(t *testing.T, admin, pc, ic client.Client) {
	ctx := t.Context()
	slot, agents := verification("slot-1-v1-a1"), agentsVerification("agents-1-v1-a1")
	for _, v := range []*v1alpha1.PreviewVerification{slot, agents} {
		if err := ic.Create(ctx, v); err != nil {
			t.Fatalf("intent-controller creating %s: %v", v.Name, err)
		}
		v.Status = v1alpha1.PreviewVerificationStatus{Phase: v1alpha1.VerificationRunning}
	}
	for _, tc := range []struct {
		name string
		c    client.Client
		v    *v1alpha1.PreviewVerification
		want string // "" admits
	}{
		{"preview-controller, a slot verification", pc, slot, ""},
		{"intent-controller, an agents verification", ic, agents, ""},
		{"intent-controller, a slot verification", ic, slot, pvByPlace},
		{"preview-controller, an agents verification", pc, agents, pvByPlace},
		{"the operator, a slot verification", admin, slot, pvByPlace},
		{"the operator, an agents verification", admin, agents, pvByPlace},
	} {
		err := tc.c.Status().Update(ctx, tc.v.DeepCopy(), client.DryRunAll)
		if tc.want == "" && err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
		if tc.want != "" && !deniedWith(err, tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	refusedAs(t, admin, agentsVerification("by-operator"), pvWriters, "an agents request by the operator")
	if err := admin.Delete(ctx, agents.DeepCopy(), client.DryRunAll); !deniedWith(err, pvWriters) {
		t.Errorf("the operator deleting an agents request: %v, want %q", err, pvWriters)
	}
	if err := ic.Delete(ctx, agents.DeepCopy(), client.DryRunAll); err != nil {
		t.Errorf("intent-controller deleting an agents request: %v", err)
	}
}

// grantEphemeral lets the Job controller's identity add an ephemeral
// container in the agents namespace, so the refusal below is admission's.
func grantEphemeral(t *testing.T, admin client.Client) {
	t.Helper()
	ctx := t.Context()
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "ephemeral-tester", Namespace: egressAgents},
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods/ephemeralcontainers"},
			Verbs: []string{"update"}}}}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "ephemeral-tester", Namespace: egressAgents},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects: []rbacv1.Subject{{Kind: "User", APIGroup: rbacv1.GroupName, Name: saJobController}}}
	for _, o := range []client.Object{role, binding} {
		if err := admin.Create(ctx, o); err != nil {
			t.Fatalf("create %s: %v", o.GetName(), err)
		}
	}
	rbactest.WaitAllowed(t, admin, rbactest.Review(saJobController, nil, authorizationv1.ResourceAttributes{
		Namespace: egressAgents, Resource: "pods", Subresource: "ephemeralcontainers", Verb: "update"}))
}

// agentsVerification is a valid agents-placed verification request.
func agentsVerification(name string) *v1alpha1.PreviewVerification {
	v := verification(name)
	s := &v.Spec
	s.Placement = v1alpha1.PlacementAgents
	s.PreviewRef, s.PreviewGeneration = v1alpha1.ObjectReference{}, 0
	s.Outcomes = []v1alpha1.AcceptanceOutcome{{ID: "counts", Kind: v1alpha1.OutcomeKindChange,
		Outcome: "bbcount prints the number of beats.", From: "print the beat count"}}
	s.OutcomesDigest = "sha256:" + strings.Repeat("e", 64)
	s.Rung = v1alpha1.RungFull
	s.Tools = &v1alpha1.ToolsSpec{Sandbox: true, MaxToolCalls: 300}
	s.Workspace = &v1alpha1.VerificationWorkspace{
		Repository: "bbcount", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
		HeadRepositoryRef: v1alpha1.ObjectReference{Name: name + "-head", UID: "u-head"},
		BaseRepositoryRef: v1alpha1.ObjectReference{Name: name + "-base", UID: "u-base"},
		SandboxImage:      tpRepoImage, SandboxSearchPath: "/usr/local/go/bin:/usr/bin:/bin",
		Surface: v1alpha1.WorkspaceSurface{CLI: &v1alpha1.WorkspaceCLI{
			Build: []string{"go", "build", "-o", "/work/bin/bbcount", "."}, Binary: "/work/bin/bbcount",
			BuildTimeout: metav1.Duration{Duration: 10 * time.Minute}}},
	}
	s.Heads = []v1alpha1.VerificationHead{{Component: "bbcount", Repository: "https://github.com/acme/bbcount",
		Revision: s.Workspace.HeadSHA}}
	return v
}

// agentsToolJob is the agents-namespace tool pod's Job as intent-controller
// builds it (SPEC S2-10, the SP-6 shape): tools beside prepare and the two
// seeds in the preview-tools image, two sandboxes in the repository image,
// three distinct UIDs, a memory /dev/shm each, its own <job>-tools Secret.
func agentsToolJob(name string) *batchv1.Job {
	pod := agentsToolPod(name, nil)
	labels := copyLabels(pod.Labels)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: egressAgents, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptrTo(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: copyLabels(pod.Labels)},
				Spec:       pod.Spec,
			},
		},
	}
}

// agentsToolPod is agentsToolJob's pod, named name and owned by owners (its
// Secret is the controlling Job's, else name's).
func agentsToolPod(name string, owners []metav1.OwnerReference) *corev1.Pod {
	job := name
	for _, o := range owners {
		job = o.Name
	}
	always := corev1.ContainerRestartPolicyAlways
	no := false
	empty := func(n string) corev1.Volume {
		return corev1.Volume{Name: n, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
	}
	shm := func(n string) corev1.Volume {
		v := empty(n)
		v.EmptyDir.Medium = corev1.StorageMediumMemory
		q := resource.MustParse("16Mi")
		v.EmptyDir.SizeLimit = &q
		return v
	}
	m := func(n, path string, ro bool) corev1.VolumeMount {
		return corev1.VolumeMount{Name: n, MountPath: path, ReadOnly: ro}
	}
	init := func(n, image string, uid int64, mounts ...corev1.VolumeMount) corev1.Container {
		return corev1.Container{Name: n, Image: image, WorkingDir: "/", Command: []string{"/preview-tools", n},
			SecurityContext: toolpod.Security(uid), VolumeMounts: mounts}
	}
	sandbox := func(side string, uid int64) corev1.Container {
		c := init("sandbox-"+side, tpRepoImage, uid,
			m("work-"+side, "/work", false), m("cache-"+side, "/work/cache", false),
			m("home-"+side, "/work/home", false), m("tmp-"+side, "/tmp", false),
			m("exec-"+side, "/run/maul-exec", false), m("maul-bin", "/maul-bin", true),
			m("shm-"+side, "/dev/shm", false))
		c.Command = []string{"/maul-bin/tool-exec", "--socket", "/run/maul-exec/sock"}
		c.RestartPolicy = &always
		c.Env = []corev1.EnvVar{{Name: "HOME", Value: "/work/home"}, {Name: "GOCACHE", Value: "/work/cache/go-build"}}
		return c
	}
	volumes := make([]corev1.Volume, 0, 18)
	volumes = append(volumes, empty("maul-bin"), shm("shm-tools"), shm("shm-head"), shm("shm-base"),
		empty("tmp-tools"), corev1.Volume{Name: "tools-secret", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: job + "-tools"}}})
	for _, side := range []string{"head", "base"} {
		for _, kind := range []string{"pristine", "work", "cache", "home", "tmp", "exec"} {
			volumes = append(volumes, empty(kind+"-"+side))
		}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: egressAgents, OwnerReferences: owners,
			Labels: map[string]string{
				"app.kubernetes.io/name": "maul-agent", "app.kubernetes.io/managed-by": "maul",
				evRole: "tools", evEvaluation: evUID,
			}},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// The agents namespace enforces Pod Security restricted.
			SecurityContext: &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault}},
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			ShareProcessNamespace:        &no,
			DNSPolicy:                    corev1.DNSNone,
			DNSConfig:                    &corev1.PodDNSConfig{Nameservers: []string{"127.0.0.1"}},
			InitContainers: []corev1.Container{
				init("prepare", toolsImage, 65532, m("maul-bin", "/maul-bin", false),
					m("pristine-head", "/trees/head", false), m("pristine-base", "/trees/base", false)),
				init("seed-head", toolsImage, 65540, m("pristine-head", "/pristine", true), m("work-head", "/work", false)),
				init("seed-base", toolsImage, 65541, m("pristine-base", "/pristine", true), m("work-base", "/work", false)),
				sandbox("head", 65540),
				sandbox("base", 65541),
			},
			Containers: []corev1.Container{{
				Name: "tools", Image: toolsImage, Args: []string{"serve"},
				SecurityContext: toolpod.Security(65532),
				VolumeMounts: []corev1.VolumeMount{m("pristine-head", "/pristine/head", true),
					m("pristine-base", "/pristine/base", true), m("exec-head", "/run/exec-head", false),
					m("exec-base", "/run/exec-base", false), m("tmp-tools", "/tmp", false),
					m("shm-tools", "/dev/shm", false), m("tools-secret", "/etc/maul/tools", true)},
			}},
			Volumes: volumes,
		},
	}
}

// builderToolJob is the tool pod Job internal/jobs pins as its golden
// name, in the agents namespace.
func builderToolJob(t *testing.T, name string) *batchv1.Job {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "jobs", "testdata", name+".yaml"))
	if err != nil {
		t.Fatalf("read the jobs golden %s: %v", name, err)
	}
	var j batchv1.Job
	if err := yaml.UnmarshalStrict(raw, &j); err != nil {
		t.Fatalf("decode the jobs golden %s: %v", name, err)
	}
	if j.Namespace != egressAgents || j.Spec.Template.Spec.Containers[0].Image != toolsImage {
		t.Fatalf("the jobs golden %s is in %s on %s, not %s on %s", name, j.Namespace,
			j.Spec.Template.Spec.Containers[0].Image, egressAgents, toolsImage)
	}
	return &j
}

// volume is the pod's volume named n.
func volume(p *corev1.PodSpec, n string) *corev1.Volume {
	for i := range p.Volumes {
		if p.Volumes[i].Name == n {
			return &p.Volumes[i]
		}
	}
	panic("no volume " + n)
}

// container is the pod's container or init container named n.
func container(p *corev1.PodSpec, n string) *corev1.Container {
	for i := range p.Containers {
		if p.Containers[i].Name == n {
			return &p.Containers[i]
		}
	}
	for i := range p.InitContainers {
		if p.InitContainers[i].Name == n {
			return &p.InitContainers[i]
		}
	}
	panic("no container " + n)
}

func mount(p *corev1.PodSpec, c string, m corev1.VolumeMount) {
	ct := container(p, c)
	ct.VolumeMounts = append(ct.VolumeMounts, m)
}

func unmount(p *corev1.PodSpec, c, v string) {
	ct := container(p, c)
	ct.VolumeMounts = slices.DeleteFunc(ct.VolumeMounts, func(m corev1.VolumeMount) bool { return m.Name == v })
}

func ptrTo[T any](v T) *T { return &v }
