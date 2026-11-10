// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/netprofile"
	"github.com/devthenet-labs/maul/internal/previewtools"
	"github.com/devthenet-labs/maul/internal/sandboxprobe"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolpod"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The tool pod fixtures: maul's preview-tools image, the head and base
// commits and their tarballs on the artifact server, and the key.
const (
	toolsImageRef = "ghcr.io/devthenet-labs/maul/preview-tools@sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"
	toolPodName = "maul-tools-6f1c2d3e4b5a"
	headCommit  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseCommit  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	headDigest  = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	baseDigest  = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	toolKey     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	repoHTTPS   = "https://github.com/octo/repo"
)

// toolPodConfig is profileConfig with the controller's resolver known
// (the strict probe's dns target) and the proxy resolvable, as an
// intent-controller with the agents evaluator runs.
func toolPodConfig() Config {
	cfg := profileConfig()
	r := clusterResolver()
	r.answers[proxyHost] = []string{"10.100.50.9"}
	cfg.Resolver = r
	cfg.SandboxProbeDNS = clusterDNS
	return cfg
}

// toolPodSpec is an agents verification's tool pod, its profile taken from
// a build of the same repository that carries none.
func toolPodSpec() ToolPodSpec {
	s := ToolPodSpec{
		Name:         toolPodName,
		Verification: pvUID,
		Repo:         "octo/repo",
		ToolsImage:   toolsImageRef,
		Image:        repoImage,
		SearchPath:   repoSearchPath,
		Trees: []Tree{
			{Key: ToolSideHead, URL: repoHTTPS, BaseSHA: headCommit, ArtifactDigest: headDigest,
				ArtifactURL: "http://" + artifactHost + ":9790/artifacts/head.tar.gz"},
			{Key: ToolSideBase, URL: repoHTTPS, BaseSHA: baseCommit, ArtifactDigest: baseDigest,
				ArtifactURL: "http://" + artifactHost + ":9790/artifacts/base.tar.gz"},
		},
		Data:        map[string]string{toolsConfigFile: `{"mode":"workspace"}`, toolpod.OutcomesKey: "[]"},
		Key:         toolKey,
		Timeout:     30 * time.Minute,
		DeadlineMax: 80 * time.Minute,
	}
	build := buildProfileSpec()
	build.NetworkProfile = nil
	s.ProfileOf(build)
	return s
}

// toolPodSpecAttributed is toolPodSpec as intent-controller renders it
// under --attribution-mode attribute: the chart envtest admits its golden
// through the attribution, budget and tool pod shape policies together.
func toolPodSpecAttributed() ToolPodSpec {
	s := toolPodSpec()
	s.AttributedTo = "org"
	return s
}

// toolPodSpecWith is toolPodSpec with its profile taken from a build that
// asks for p.
func toolPodSpecWith(p *NetworkProfile) ToolPodSpec {
	s := toolPodSpec()
	build := buildProfileSpec()
	build.NetworkProfile = p
	s.ProfileOf(build)
	return s
}

// renderToolPod runs the builder without creating anything.
func renderToolPod(t *testing.T, cfg Config, spec ToolPodSpec) (*batchv1.Job, *corev1.Secret) {
	t.Helper()
	job, secret, err := New(fake.NewClientset(), cfg, nil).buildToolPod(context.Background(), spec)
	if err != nil {
		t.Fatalf("buildToolPod: %v", err)
	}
	return job, secret
}

// goldenBytes pins raw as testdata/<name>.yaml, as goldenJob pins a Job.
func goldenBytes(t *testing.T, name string, raw []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".yaml")
	if *update {
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatalf("update golden %s: %v", path, err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if string(raw) != string(want) {
		t.Errorf("differs from %s (run with -update to accept):\n--- want\n%s\n--- got\n%s", path, want, raw)
	}
}

func containerNamed(t *testing.T, pod corev1.PodSpec, name string) corev1.Container {
	t.Helper()
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container %s", name)
	return corev1.Container{}
}

// TestGoldenToolPod pins the tool pod whole, without a profile and with
// the go route and an npm tunnel, and its Secret.
func TestGoldenToolPod(t *testing.T) {
	for _, tt := range []struct {
		golden string
		spec   ToolPodSpec
	}{
		{"toolpod_default", toolPodSpec()},
		{"toolpod_profile_go_tunnel", toolPodSpecWith(goTunnelProfile())},
		{"toolpod_attributed", toolPodSpecAttributed()},
	} {
		t.Run(tt.golden, func(t *testing.T) {
			job, secret := renderToolPod(t, toolPodConfig(), tt.spec)
			goldenJob(t, tt.golden, job)
			raw, err := yaml.Marshal(secret)
			if err != nil {
				t.Fatal(err)
			}
			goldenBytes(t, tt.golden+"_secret", raw)
		})
	}
}

// TestToolPodShape asserts the wave 0 requirements one by one, so a golden
// accepted in haste cannot lose one.
func TestToolPodShape(t *testing.T) {
	job, _ := renderToolPod(t, toolPodConfig(), toolPodSpecWith(goProfile()))
	pod := job.Spec.Template.Spec
	for what, ok := range toolPodInvariants(job) {
		if !ok {
			t.Errorf("not %s", what)
		}
	}
	inits := make([]string, 0, len(pod.InitContainers))
	for _, c := range pod.InitContainers {
		inits = append(inits, c.Name)
	}
	if want := []string{"prepare", "seed-head", "seed-base", "sandbox-head", "sandbox-base"}; !slices.Equal(inits, want) {
		t.Errorf("init containers %v, want %v", inits, want)
	}
	uids := map[string]int64{"prepare": 65532, "seed-head": 65540, "seed-base": 65541, "tools": 65532,
		"sandbox-head": 65540, "sandbox-base": 65541}
	for name, uid := range uids {
		checkToolContainer(t, containerNamed(t, pod, name), uid)
	}
	for _, name := range []string{"tools", "sandbox-head", "sandbox-base"} {
		if !mountsShm(pod, containerNamed(t, pod, name)) {
			t.Errorf("%s has no /dev/shm emptyDir of its own", name)
		}
	}
	for _, l := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
		if l[v1alpha1.LabelRole] != v1alpha1.RoleTools || l[v1alpha1.LabelPreviewVerification] != pvUID ||
			l[netprofile.LabelKey("go")] != netprofile.LabelValue {
			t.Errorf("labels %v lack the role, the verification or the profile", l)
		}
	}
}

// toolPodInvariants are the pod-level wave 0 requirements, by name.
func toolPodInvariants(job *batchv1.Job) map[string]bool {
	pod := job.Spec.Template.Spec
	return map[string]bool{
		"shareProcessNamespace explicitly false": pod.ShareProcessNamespace != nil && !*pod.ShareProcessNamespace,
		"no ServiceAccount token": pod.AutomountServiceAccountToken != nil &&
			!*pod.AutomountServiceAccountToken && pod.ServiceAccountName == "default",
		"no service links": pod.EnableServiceLinks != nil && !*pod.EnableServiceLinks,
		"no resolver": pod.DNSPolicy == corev1.DNSNone && pod.DNSConfig != nil &&
			slices.Equal(pod.DNSConfig.Nameservers, []string{deadNameserver}),
		"no preview placement": len(pod.Tolerations) == 0 && len(pod.NodeSelector) == 0 && pod.Affinity == nil,
		"tools the one main":   len(pod.Containers) == 1 && pod.Containers[0].Name == "tools",
		"pod deadline below the Job's": job.Spec.ActiveDeadlineSeconds != nil && pod.ActiveDeadlineSeconds != nil &&
			*pod.ActiveDeadlineSeconds < *job.Spec.ActiveDeadlineSeconds,
		"no retry": job.Spec.BackoffLimit != nil && *job.Spec.BackoffLimit == 0,
	}
}

// checkToolContainer holds one container to its UID, read-only non-root
// posture, workingDir and image; a sandbox also to being a native sidecar
// running tool-exec.
func checkToolContainer(t *testing.T, c corev1.Container, uid int64) {
	t.Helper()
	sc := c.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != uid || !*sc.ReadOnlyRootFilesystem || !*sc.RunAsNonRoot {
		t.Errorf("%s runs as %v, want %d, read-only, non-root", c.Name, sc, uid)
	}
	if c.Name != "tools" && c.WorkingDir != "/" {
		t.Errorf("%s workingDir %q, want /", c.Name, c.WorkingDir)
	}
	if !strings.HasPrefix(c.Name, "sandbox-") {
		if c.Image != toolsImageRef {
			t.Errorf("%s runs %s, want the preview-tools image", c.Name, c.Image)
		}
		return
	}
	if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("%s is not a native sidecar", c.Name)
	}
	if c.Image != repoImage || !slices.Equal(c.Command, []string{"/maul-bin/tool-exec"}) {
		t.Errorf("%s runs %s %v", c.Name, c.Image, c.Command)
	}
}

// TestToolPodVolumesFit: each sandbox's disk emptyDirs fit within its
// ephemeral-storage limit, and every volume tools, prepare and the seeds
// alone mount within the tools limit, so the kubelet evicts nothing a
// verification of a maul-scale tree needs.
func TestToolPodVolumesFit(t *testing.T) {
	job, _ := renderToolPod(t, toolPodConfig(), toolPodSpec())
	pod := job.Spec.Template.Spec
	sizes := map[string]int64{}
	for _, v := range pod.Volumes {
		if v.EmptyDir != nil && v.EmptyDir.Medium != corev1.StorageMediumMemory {
			sizes[v.Name] = v.EmptyDir.SizeLimit.Value()
		}
	}
	sandboxSum := map[string]int64{}
	var toolsSum int64
	seen := map[string]bool{}
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		for _, m := range c.VolumeMounts {
			if side, ok := strings.CutPrefix(c.Name, "sandbox-"); ok {
				if m.Name != "maul-bin" {
					sandboxSum[side] += sizes[m.Name]
				}
				continue
			}
			if !seen[m.Name] && !strings.HasPrefix(m.Name, "work-") && !strings.HasPrefix(m.Name, "exec-") {
				toolsSum += sizes[m.Name]
			}
			seen[m.Name] = true
		}
	}
	limit := func(name string) int64 {
		q := containerNamed(t, pod, name).Resources.Limits[corev1.ResourceEphemeralStorage]
		return q.Value()
	}
	for side, sum := range sandboxSum {
		if sum > limit("sandbox-"+side) {
			t.Errorf("sandbox-%s volumes %d bytes exceed its limit %d", side, sum, limit("sandbox-"+side))
		}
	}
	if toolsSum > limit("tools") {
		t.Errorf("tools-side volumes %d bytes exceed the tools limit %d", toolsSum, limit("tools"))
	}
	// The work volume holds the tree's copy and the build output, and the
	// copy of both a check run starts from (toolexec's fresh.go).
	if sizes["work-head"] < 2<<30 {
		t.Errorf("work %d bytes is under the 2 GiB the tree, its output and their kept copy need",
			sizes["work-head"])
	}
	// The shim keeps no copy past toolexec.MaxKeptBytes, which bounds the
	// live tree and output too: both, and 512 MiB of scratch, fit.
	if need := 2*int64(toolexec.MaxKeptBytes) + 512<<20; sizes["work-head"] < need {
		t.Errorf("work %d bytes is under the %d the kept copy's bound needs", sizes["work-head"], need)
	}
	if sizes["cache-head"] < 3<<30+512<<20 {
		t.Errorf("cache %d bytes is under the 3.5 GB a maul-scale tree needs", sizes["cache-head"])
	}
}

// mountsShm reports whether c mounts a memory emptyDir of 16Mi at /dev/shm
// that no other container mounts.
func mountsShm(pod corev1.PodSpec, c corev1.Container) bool {
	for _, m := range c.VolumeMounts {
		if m.MountPath != "/dev/shm" {
			continue
		}
		for _, v := range pod.Volumes {
			if v.Name == m.Name && v.EmptyDir != nil && v.EmptyDir.Medium == corev1.StorageMediumMemory &&
				v.EmptyDir.SizeLimit != nil && v.EmptyDir.SizeLimit.String() == "16Mi" {
				return mountedBy(pod, m.Name) == 1
			}
		}
	}
	return false
}

func mountedBy(pod corev1.PodSpec, volume string) int {
	n := 0
	for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
		for _, m := range c.VolumeMounts {
			if m.Name == volume {
				n++
				break
			}
		}
	}
	return n
}

// TestToolPodMountIsolation: tools mounts no sandbox work volume, and from
// the volumes a sandbox mounts only the exec-socket directories are
// writable in tools; the Secret is mounted in tools alone; each sandbox
// mounts none of the other side's volumes, and neither mounts a pristine
// tree.
func TestToolPodMountIsolation(t *testing.T) {
	for _, spec := range []ToolPodSpec{toolPodSpec(), toolPodSpecWith(goTunnelProfile())} {
		job, _ := renderToolPod(t, toolPodConfig(), spec)
		pod := job.Spec.Template.Spec
		checkToolsMounts(t, pod)
		for _, v := range pod.Volumes {
			if v.Secret == nil {
				continue
			}
			if v.Secret.SecretName != toolPodName+"-tools" {
				t.Errorf("Secret volume %s names %s", v.Name, v.Secret.SecretName)
			}
			if n := mountedBy(pod, v.Name); n != 1 || !slices.ContainsFunc(containerNamed(t, pod, "tools").VolumeMounts,
				func(m corev1.VolumeMount) bool { return m.Name == v.Name }) {
				t.Errorf("the tools Secret is mounted by %d containers, not tools alone", n)
			}
		}
		for side, other := range map[string]string{"head": "base", "base": "head"} {
			checkSideMounts(t, pod, side, other)
		}
	}
}

// checkToolsMounts: tools mounts no work volume and, of the sandboxes'
// volumes, only the exec-socket directories writable.
func checkToolsMounts(t *testing.T, pod corev1.PodSpec) {
	t.Helper()
	sandboxVols := map[string]bool{}
	for _, side := range []string{"head", "base"} {
		for _, m := range containerNamed(t, pod, "sandbox-"+side).VolumeMounts {
			sandboxVols[m.Name] = true
		}
	}
	for _, m := range containerNamed(t, pod, "tools").VolumeMounts {
		if strings.HasPrefix(m.Name, "work-") {
			t.Errorf("tools mounts the sandbox work volume %s", m.Name)
		}
		if sandboxVols[m.Name] && !m.ReadOnly && !strings.HasPrefix(m.Name, "exec-") {
			t.Errorf("tools mounts the sandbox volume %s writable", m.Name)
		}
	}
}

// checkSideMounts: side's sandbox mounts nothing of the other side's and
// no pristine tree, maul-bin read-only; its seed mounts only its own
// pristine tree, read-only, and its work volume.
func checkSideMounts(t *testing.T, pod corev1.PodSpec, side, other string) {
	t.Helper()
	for _, m := range containerNamed(t, pod, "sandbox-"+side).VolumeMounts {
		if strings.HasSuffix(m.Name, "-"+other) || strings.HasPrefix(m.Name, "trees-") ||
			m.Name == "maul-bin" && !m.ReadOnly {
			t.Errorf("sandbox-%s mounts %s (read-only %v)", side, m.Name, m.ReadOnly)
		}
	}
	for _, m := range containerNamed(t, pod, "seed-"+side).VolumeMounts {
		ok := m.Name == "trees-"+side && m.ReadOnly || m.Name == "work-"+side && !m.ReadOnly
		if !ok {
			t.Errorf("seed-%s mounts %s (read-only %v)", side, m.Name, m.ReadOnly)
		}
	}
}

// profileOwnedNames are the env names a profile render owns: profileEnv's
// and the tunnel's.
func profileOwnedNames(p *NetworkProfile) []string {
	var names []string
	for _, e := range profileEnv(p) {
		names = append(names, e.Name)
	}
	tunnel, err := TunnelEnv(*p, []string{"http://" + artifactHost + ":9790/x"})
	if err == nil {
		for _, e := range tunnel {
			names = append(names, e.Name)
		}
	}
	return names
}

// TestToolPodProfileMatchesBuild: the tool pod's network is the build's,
// taken through ProfileOf: the same profile label on the Job and its pod,
// every name the profile owns at the build's value in both sandboxes (and
// nowhere else), and a hosts file that pins nothing the build's does not,
// the proxy at the build's address. A build that gets no profile (none
// asked for, the kill switch off) gives none; a build on another image is
// refused.
func TestToolPodProfileMatchesBuild(t *testing.T) {
	for _, p := range []*NetworkProfile{goProfile(), goTunnelProfile(), tunnelProfile(), jvmTunnelProfile()} {
		t.Run(p.Name+fmt.Sprint(len(p.Hosts)), func(t *testing.T) {
			cfg := toolPodConfig()
			build := buildProfileSpec()
			build.NetworkProfile = p
			buildCfg := cfg
			buildCfg.DNS = DNSNone
			buildJob := buildRoleJob(t, buildCfg, build)
			spec := toolPodSpec()
			spec.ProfileOf(build)
			job, _ := renderToolPod(t, cfg, spec)
			key := netprofile.LabelKey(p.Name)
			for _, l := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
				if l[key] != buildJob.Labels[key] || l[key] != netprofile.LabelValue {
					t.Errorf("profile label %q, build has %q", l[key], buildJob.Labels[key])
				}
			}
			checkProfileEnv(t, job.Spec.Template.Spec, agentOf(t, buildJob.Spec.Template.Spec), p)
			checkAliasesWithin(t, job.Spec.Template.Spec.HostAliases, buildJob.Spec.Template.Spec.HostAliases)
		})
	}
	for name, mutate := range map[string]func(*Config, *Spec){
		"none asked": func(_ *Config, s *Spec) { s.NetworkProfile = nil },
		"switch off": func(c *Config, _ *Spec) { c.NetworkProfiles = false },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := toolPodConfig()
			build := buildProfileSpec()
			mutate(&cfg, &build)
			spec := toolPodSpec()
			spec.ProfileOf(build)
			job, _ := renderToolPod(t, cfg, spec)
			pod := job.Spec.Template.Spec
			if networkProfileOf(job) != "" || pinned(pod.HostAliases, proxyHost) {
				t.Error("the tool pod carries a profile")
			}
			for _, side := range []string{"head", "base"} {
				sb := containerNamed(t, pod, "sandbox-"+side)
				if envValue(sb.Env, netprofile.EnvProfile) != "" || envValue(sb.Env, "GOPROXY") != "off" ||
					envValue(sb.Env, "HTTPS_PROXY") != "" {
					t.Errorf("sandbox-%s env %v is not the no-network one", side, sb.Env)
				}
			}
		})
	}
	t.Run("default image", func(t *testing.T) {
		build := buildProfileSpec()
		build.RunnerImage = ""
		spec := toolPodSpec()
		spec.ProfileOf(build)
		if _, _, err := New(fake.NewClientset(), toolPodConfig(), nil).buildToolPod(context.Background(),
			spec); !errors.Is(err, ErrToolPodRefused) {
			t.Errorf("%v, want a refusal", err)
		}
	})
}

// checkProfileEnv: every name p's render owns is at the build agent's value
// in both sandboxes, and in no other container.
func checkProfileEnv(t *testing.T, pod corev1.PodSpec, agent corev1.Container, p *NetworkProfile) {
	t.Helper()
	for _, name := range profileOwnedNames(p) {
		want := envValue(agent.Env, name)
		for _, side := range []string{"head", "base"} {
			if got := envValue(containerNamed(t, pod, "sandbox-"+side).Env, name); got != want {
				t.Errorf("sandbox-%s %s = %q, the build's is %q", side, name, got, want)
			}
		}
		for _, c := range []string{"tools", "prepare", "seed-head", "seed-base"} {
			if slices.ContainsFunc(containerNamed(t, pod, c).Env,
				func(e corev1.EnvVar) bool { return e.Name == name }) {
				t.Errorf("%s carries the profile's %s", c, name)
			}
		}
	}
}

// checkAliasesWithin: the tool pod pins nothing the build does not, at the
// build's address, and pins the proxy.
func checkAliasesWithin(t *testing.T, tool, build []corev1.HostAlias) {
	t.Helper()
	buildAt := map[string]string{}
	for _, a := range build {
		for _, h := range a.Hostnames {
			buildAt[h] = a.IP
		}
	}
	for _, a := range tool {
		for _, h := range a.Hostnames {
			if buildAt[h] != a.IP {
				t.Errorf("the tool pod pins %s at %s; the build pins it at %q", h, a.IP, buildAt[h])
			}
		}
	}
	if !pinned(tool, proxyHost) {
		t.Error("the proxy is not pinned")
	}
}

func pinned(aliases []corev1.HostAlias, host string) bool {
	return slices.ContainsFunc(aliases, func(a corev1.HostAlias) bool { return slices.Contains(a.Hostnames, host) })
}

// TestToolPodSpecRefusals: each contradiction in what the controller asks
// for is refused before anything is created.
func TestToolPodSpecRefusals(t *testing.T) {
	tests := map[string]func(*Config, *ToolPodSpec){
		"repository images off": func(c *Config, _ *ToolPodSpec) { c.AllowRepositoryImages = false },
		"no probe resolver":     func(c *Config, _ *ToolPodSpec) { c.SandboxProbeDNS = "" },
		"bad name":              func(_ *Config, s *ToolPodSpec) { s.Name = "Tools_1" },
		"long name":             func(_ *Config, s *ToolPodSpec) { s.Name = strings.Repeat("a", 58) },
		"no verification":       func(_ *Config, s *ToolPodSpec) { s.Verification = "" },
		"no repo":               func(_ *Config, s *ToolPodSpec) { s.Repo = "" },
		"tag tools image":       func(_ *Config, s *ToolPodSpec) { s.ToolsImage = "ghcr.io/x/preview-tools:1" },
		"tag sandbox image":     func(_ *Config, s *ToolPodSpec) { s.Image = "ghcr.io/x/go:1" },
		"one image":             func(_ *Config, s *ToolPodSpec) { s.Image = s.ToolsImage },
		"one tree":              func(_ *Config, s *ToolPodSpec) { s.Trees = s.Trees[:1] },
		"three trees":           func(_ *Config, s *ToolPodSpec) { s.Trees = append(s.Trees, s.Trees[0]) },
		"two heads":             func(_ *Config, s *ToolPodSpec) { s.Trees[1].Key = ToolSideHead },
		"other key":             func(_ *Config, s *ToolPodSpec) { s.Trees[1].Key = "lib" },
		"two repositories":      func(_ *Config, s *ToolPodSpec) { s.Trees[1].URL = "https://github.com/octo/other" },
		"same commit":           func(_ *Config, s *ToolPodSpec) { s.Trees[1].BaseSHA = headCommit },
		"bad digest":            func(_ *Config, s *ToolPodSpec) { s.Trees[0].ArtifactDigest = "abc" },
		"bad commit":            func(_ *Config, s *ToolPodSpec) { s.Trees[0].BaseSHA = "HEAD" },
		"artifact URL option":   func(_ *Config, s *ToolPodSpec) { s.Trees[0].ArtifactURL = "-o/etc/passwd" },
		"artifact URL space":    func(_ *Config, s *ToolPodSpec) { s.Trees[0].ArtifactURL = "http://a b/x" },
		"bad key":               func(_ *Config, s *ToolPodSpec) { s.Key = "short" },
		"no relay config":       func(_ *Config, s *ToolPodSpec) { delete(s.Data, toolsConfigFile) },
		"key in data":           func(_ *Config, s *ToolPodSpec) { s.Data[ToolsKeyFile] = "x" },
		"bad data key":          func(_ *Config, s *ToolPodSpec) { s.Data["../x"] = "x" },
		"short timeout":         func(_ *Config, s *ToolPodSpec) { s.Timeout = 30 * time.Second },
		"long timeout":          func(_ *Config, s *ToolPodSpec) { s.Timeout = 76 * time.Minute },
		"no deadline bound":     func(_ *Config, s *ToolPodSpec) { s.DeadlineMax = 0 },
		"bad sandbox resources": func(_ *Config, s *ToolPodSpec) { s.Resources.Sandbox.CPU = "lots" },
		"bad search path":       func(_ *Config, s *ToolPodSpec) { s.SearchPath = "bin:/usr/bin" },
		"no build":              func(_ *Config, s *ToolPodSpec) { s.build = nil },
		"build of another repo": func(_ *Config, s *ToolPodSpec) { s.build.Repo = "octo/other" },
		"plan, not build":       func(_ *Config, s *ToolPodSpec) { s.build.Phase = "plan" },
		"remediate, not build": func(_ *Config, s *ToolPodSpec) {
			s.build.Phase, s.build.Kind = "remediate", "remediation"
		},
		"build on another image": func(_ *Config, s *ToolPodSpec) { s.build.RunnerImage = toolsImageRef },
		"build profile refused": func(_ *Config, s *ToolPodSpec) {
			s.build.NetworkProfile = &NetworkProfile{Name: "go", Route: "direct", ProxyURL: "http://p:1"}
		},
		"build harness unknown": func(_ *Config, s *ToolPodSpec) { s.build.Harness = "nope" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := toolPodConfig()
			spec := toolPodSpec()
			spec.Data = maps.Clone(spec.Data)
			spec.Trees = slices.Clone(spec.Trees)
			b := *spec.build
			spec.build = &b
			mutate(&cfg, &spec)
			cs := fake.NewClientset()
			_, err := New(cs, cfg, nil).CreateToolPod(context.Background(), spec)
			if !errors.Is(err, ErrToolPodRefused) {
				t.Fatalf("err %v, want ErrToolPodRefused", err)
			}
			if n := len(cs.Actions()); n != 0 {
				t.Errorf("%d API calls before the refusal", n)
			}
		})
	}
}

// TestToolPodShapeRefusal: the rendered-pod check refuses each thing the
// builder never renders, so a later edit to the builder that adds one
// fails here and in every Create.
func TestToolPodShapeRefusal(t *testing.T) {
	spec := toolPodSpecWith(goProfile())
	allowed := toolPodEnvAllowed(goProfile(), nil)
	sandbox := func(pod *corev1.PodSpec) *corev1.Container { return &pod.InitContainers[3] }
	tests := map[string]func(*corev1.PodSpec){
		"token volume": func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "t", VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{
					ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}}}}})
		},
		"automount":    func(p *corev1.PodSpec) { p.AutomountServiceAccountToken = nil },
		"another SA":   func(p *corev1.PodSpec) { p.ServiceAccountName = "maul-agent" },
		"shared PIDs":  func(p *corev1.PodSpec) { p.ShareProcessNamespace = new(true) },
		"unset PIDs":   func(p *corev1.PodSpec) { p.ShareProcessNamespace = nil },
		"host network": func(p *corev1.PodSpec) { p.HostNetwork = true },
		"cluster DNS":  func(p *corev1.PodSpec) { p.DNSPolicy = corev1.DNSClusterFirst },
		"toleration": func(p *corev1.PodSpec) {
			p.Tolerations = []corev1.Toleration{{Key: "maul.run/preview", Operator: corev1.TolerationOpExists}}
		},
		"node selector": func(p *corev1.PodSpec) { p.NodeSelector = map[string]string{"pool": "preview"} },
		"secretKeyRef": func(p *corev1.PodSpec) {
			c := sandbox(p)
			c.Env = append(c.Env, corev1.EnvVar{Name: "X", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{Key: "k"}}})
		},
		"envFrom": func(p *corev1.PodSpec) {
			sandbox(p).EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{}}}
		},
		"credential env": func(p *corev1.PodSpec) {
			c := sandbox(p)
			c.Env = append(c.Env, corev1.EnvVar{Name: "ANTHROPIC_API_KEY", Value: "x"})
		},
		"reserved env": func(p *corev1.PodSpec) {
			p.Containers[0].Env = append(p.Containers[0].Env, corev1.EnvVar{Name: "MAUL_PHASE", Value: "x"})
		},
		"profile env in tools": func(p *corev1.PodSpec) {
			p.Containers[0].Env = append(p.Containers[0].Env, corev1.EnvVar{Name: "HTTPS_PROXY", Value: "x"})
		},
		"other Secret": func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "s", VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "anthropic"}}})
		},
		"Secret in a sandbox": func(p *corev1.PodSpec) {
			c := sandbox(p)
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "tools-secret", MountPath: "/s"})
		},
		"tag image": func(p *corev1.PodSpec) { sandbox(p).Image = "ghcr.io/x/go:1" },
		"third image": func(p *corev1.PodSpec) {
			sandbox(p).Image = "ghcr.io/x/go@sha256:" + strings.Repeat("9", 64)
		},
		"equal UIDs": func(p *corev1.PodSpec) { sandbox(p).SecurityContext.RunAsUser = new(int64(65532)) },
		"seed UID": func(p *corev1.PodSpec) {
			p.InitContainers[1].SecurityContext.RunAsUser = new(int64(65541))
		},
		"workingDir": func(p *corev1.PodSpec) { sandbox(p).WorkingDir = "/work" },
		"extra init": func(p *corev1.PodSpec) {
			p.InitContainers = append(p.InitContainers, corev1.Container{Name: "debug", Image: toolsImageRef,
				WorkingDir: "/"})
		},
		"no shm": func(p *corev1.PodSpec) {
			c := sandbox(p)
			c.VolumeMounts = slices.DeleteFunc(c.VolumeMounts,
				func(m corev1.VolumeMount) bool { return m.MountPath == "/dev/shm" })
		},
		"privileged": func(p *corev1.PodSpec) { sandbox(p).SecurityContext.Privileged = new(true) },
		"tools mounts work": func(p *corev1.PodSpec) {
			p.Containers[0].VolumeMounts = append(p.Containers[0].VolumeMounts,
				corev1.VolumeMount{Name: "work-head", MountPath: "/w", ReadOnly: true})
		},
		"tools writes cache": func(p *corev1.PodSpec) {
			p.Containers[0].VolumeMounts = append(p.Containers[0].VolumeMounts,
				corev1.VolumeMount{Name: "cache-head", MountPath: "/c"})
		},
		"head mounts base work": func(p *corev1.PodSpec) {
			c := sandbox(p)
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "work-base", MountPath: "/b"})
		},
		"head reads base cache": func(p *corev1.PodSpec) {
			c := sandbox(p)
			c.VolumeMounts = append(c.VolumeMounts,
				corev1.VolumeMount{Name: "cache-base", MountPath: "/b", ReadOnly: true})
		},
		"seed mounts other tree": func(p *corev1.PodSpec) {
			c := &p.InitContainers[1]
			c.VolumeMounts = append(c.VolumeMounts,
				corev1.VolumeMount{Name: "trees-base", MountPath: "/trees/base", ReadOnly: true})
		},
		"seed writes its tree": func(p *corev1.PodSpec) { p.InitContainers[1].VolumeMounts[0].ReadOnly = false },
		"sandbox writes pristine tree": func(p *corev1.PodSpec) {
			c := sandbox(p)
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "trees-head", MountPath: "/t"})
		},
		"tools writes pristine tree": func(p *corev1.PodSpec) { p.Containers[0].VolumeMounts[1].ReadOnly = false },
		"unconfined seccomp": func(p *corev1.PodSpec) {
			sandbox(p).SecurityContext.SeccompProfile = &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeUnconfined}
		},
		"pod seccomp unset": func(p *corev1.PodSpec) { p.SecurityContext.SeccompProfile = nil },
		"hostPath": func(p *corev1.PodSpec) {
			p.Volumes = append(p.Volumes, corev1.Volume{Name: "h", VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			job, _ := renderToolPod(t, toolPodConfig(), spec)
			if err := toolPodShapeRefusal(job, spec, allowed); err != nil {
				t.Fatalf("the rendered pod is refused: %v", err)
			}
			mutate(&job.Spec.Template.Spec)
			if err := toolPodShapeRefusal(job, spec, allowed); !errors.Is(err, ErrToolPodRefused) {
				t.Errorf("err %v, want ErrToolPodRefused", err)
			}
		})
	}
}

// TestToolPodProperty is a seeded property over ToolPodSpec: whatever
// profile the build carries and whatever the Config's runners and env say,
// a rendered tool pod has no ServiceAccount token, no Secret but its own in
// tools, no credential channel and no reserved name but HOME and the
// profile's own, and every image is one of exactly two digests.
func TestToolPodProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(20261105))
	profiles := []*NetworkProfile{nil, goProfile(), goTunnelProfile(), tunnelProfile(), jvmTunnelProfile()}
	credential := ReservedEnvNames()
	for i := range 300 {
		cfg := toolPodConfig()
		cfg.Env = map[string]string{credential[rng.Intn(len(credential))]: "leak", "MAUL_X": "y"}
		cfg.DNS = []DNSMode{DNSCluster, DNSNone}[rng.Intn(2)]
		build := buildProfileSpec()
		build.NetworkProfile = profiles[rng.Intn(len(profiles))]
		spec := toolPodSpec()
		spec.ProfileOf(build)
		spec.Timeout = time.Duration(1+rng.Intn(75)) * time.Minute
		spec.DeadlineMax = spec.Timeout + time.Duration(rng.Intn(30))*time.Minute
		job, secret := renderToolPod(t, cfg, spec)
		pod := job.Spec.Template.Spec
		images := map[string]bool{}
		for _, c := range slices.Concat(pod.InitContainers, pod.Containers) {
			images[c.Image] = true
			if len(c.EnvFrom) > 0 {
				t.Fatalf("case %d: %s has envFrom", i, c.Name)
			}
			for _, e := range c.Env {
				if e.ValueFrom != nil {
					t.Fatalf("case %d: %s %s comes from elsewhere", i, c.Name, e.Name)
				}
				if credentialChannelEnv[e.Name] || (reservedEnv[e.Name] && e.Name != "HOME" &&
					!slices.Contains(profileNames(build.NetworkProfile), e.Name)) {
					t.Fatalf("case %d: %s carries reserved %s", i, c.Name, e.Name)
				}
				if e.Value == "leak" {
					t.Fatalf("case %d: Config.Env reached %s", i, c.Name)
				}
			}
		}
		if len(images) != 2 || !images[toolsImageRef] || !images[repoImage] {
			t.Fatalf("case %d: images %v", i, images)
		}
		for _, v := range pod.Volumes {
			if v.Projected != nil || v.HostPath != nil || v.Secret != nil && v.Secret.SecretName != secret.Name {
				t.Fatalf("case %d: volume %s", i, v.Name)
			}
		}
		if *pod.AutomountServiceAccountToken {
			t.Fatalf("case %d: token automounted", i)
		}
	}
}

func profileNames(p *NetworkProfile) []string {
	if p == nil {
		return nil
	}
	return profileOwnedNames(p)
}

// TestToolPodSandboxUIDs: each side's sandbox runs as the UID the relay's
// peer check holds that side's socket to (review SEC-1).
func TestToolPodSandboxUIDs(t *testing.T) {
	if sideUID[ToolSideHead] != previewtools.HeadSandboxUID || sideUID[ToolSideBase] != previewtools.BaseSandboxUID {
		t.Errorf("sandbox UIDs %v, the relay's peer check expects head %d, base %d", sideUID,
			previewtools.HeadSandboxUID, previewtools.BaseSandboxUID)
	}
}

// TestToolPodPaths: tools, prepare, the seeds and the sandboxes agree on
// where everything is, and with the relay and the exec shim's own
// constants.
func TestToolPodPaths(t *testing.T) {
	if toolsConfigFile != previewtools.ConfigFile || toolsConfigDir != previewtools.DefaultConfigDir ||
		toolsReadyPath != previewtools.ReadyPath || ToolsPort != previewtools.Port ||
		toolsTerminationPath != previewtools.TerminationPath {
		t.Error("the tool pod's relay constants differ from previewtools'")
	}
	if toolExecDir+"/"+toolExecSocket != toolexec.SocketPath {
		t.Errorf("sandbox socket %s/%s, the shim's is %s", toolExecDir, toolExecSocket, toolexec.SocketPath)
	}
	if !toolwire.IsKey(toolKey) {
		t.Fatal("fixture key is not a key")
	}
	job, secret := renderToolPod(t, toolPodConfig(), toolPodSpec())
	pod := job.Spec.Template.Spec
	tools := containerNamed(t, pod, "tools")
	for _, path := range []string{"/etc/maul-tools", "/trees/head", "/trees/base", "/run/maul-exec/head",
		"/run/maul-exec/base"} {
		if !slices.ContainsFunc(tools.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == path }) {
			t.Errorf("tools does not mount %s", path)
		}
	}
	if tools.TerminationMessagePolicy != corev1.TerminationMessageReadFile ||
		tools.ReadinessProbe == nil || tools.ReadinessProbe.HTTPGet.Path != previewtools.ReadyPath {
		t.Error("tools' termination message or readiness is not the relay's")
	}
	for _, side := range []string{"head", "base"} {
		checkSandboxPaths(t, containerNamed(t, pod, "sandbox-"+side), side)
	}
	prep := containerNamed(t, pod, "prepare")
	if envValue(prep.Env, sandboxprobe.DNSEnv) != clusterDNS {
		t.Error("prepare's strict probe has no resolver to probe")
	}
	for _, arg := range []string{"prepare", "--head-sha256=" + headDigest, "--base-sha256=" + baseDigest} {
		if !slices.Contains(prep.Args, arg) {
			t.Errorf("prepare args %v lack %s", prep.Args, arg)
		}
	}
	if string(secret.Data[ToolsKeyFile]) != toolKey || secret.Immutable == nil || !*secret.Immutable {
		t.Error("the Secret does not hold the key immutably")
	}
}

// checkSandboxPaths holds side's sandbox to Q23's paths and environment.
func checkSandboxPaths(t *testing.T, sb corev1.Container, side string) {
	t.Helper()
	got := map[string]string{}
	for _, m := range sb.VolumeMounts {
		got[m.MountPath] = m.Name
	}
	want := map[string]string{"/work": "work-" + side, "/work/cache": "cache-" + side,
		"/work/home": "home-" + side, "/tmp": "tmp-" + side, "/run/maul-exec": "exec-" + side,
		"/maul-bin": "maul-bin", "/dev/shm": "shm-" + side}
	if !maps.Equal(got, want) {
		t.Errorf("sandbox-%s mounts %v, want %v", side, got, want)
	}
	for name, want := range map[string]string{"HOME": "/work/home", "TMPDIR": "/tmp",
		"GOCACHE": "/work/cache/go-build", "GOTOOLCHAIN": "local", "PATH": repoSearchPath} {
		if got := envValue(sb.Env, name); got != want {
			t.Errorf("sandbox-%s %s = %q, want %q", side, name, got, want)
		}
	}
}

// TestCreateToolPod creates the Secret then the Job, owns the Secret by the
// Job, and adopts both on a retry.
func TestCreateToolPod(t *testing.T) {
	cs := fake.NewClientset()
	c := New(cs, toolPodConfig(), nil)
	ctx := context.Background()
	name, err := c.CreateToolPod(ctx, toolPodSpec())
	if err != nil {
		t.Fatalf("CreateToolPod: %v", err)
	}
	if name != toolPodName {
		t.Errorf("name %s", name)
	}
	if _, err := c.CreateToolPod(ctx, toolPodSpec()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	var kinds []string
	for _, a := range cs.Actions() {
		if a.GetVerb() == "create" {
			kinds = append(kinds, a.GetResource().Resource)
		}
	}
	if !slices.Equal(kinds[:2], []string{"secrets", "jobs"}) {
		t.Errorf("created %v, want the Secret first", kinds)
	}
	secret, err := cs.CoreV1().Secrets("maul-agents").Get(ctx, toolPodName+"-tools", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Kind != "Job" ||
		secret.OwnerReferences[0].Name != toolPodName {
		t.Errorf("owner references %v", secret.OwnerReferences)
	}
}

// TestCreateToolPodStaleSecret refuses to adopt a tools Secret holding
// other data (another key): the Job would serve a key the caller does not
// hold.
func TestCreateToolPodStaleSecret(t *testing.T) {
	cs := fake.NewClientset()
	c := New(cs, toolPodConfig(), nil)
	ctx := context.Background()
	if _, err := c.CreateToolPod(ctx, toolPodSpec()); err != nil {
		t.Fatalf("CreateToolPod: %v", err)
	}
	if err := cs.BatchV1().Jobs("maul-agents").Delete(ctx, toolPodName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	spec := toolPodSpec()
	spec.Key = strings.Repeat("e", 64)
	if _, err := c.CreateToolPod(ctx, spec); err == nil {
		t.Fatal("a tools Secret with another key was adopted")
	}
	if _, err := cs.BatchV1().Jobs("maul-agents").Get(ctx, toolPodName, metav1.GetOptions{}); err == nil {
		t.Error("a Job was created over the stale Secret")
	}
}

// TestCreateToolPodJobFailure deletes the Secret it created when the Job
// cannot be.
func TestCreateToolPodJobFailure(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("quota")
	})
	if _, err := New(cs, toolPodConfig(), nil).CreateToolPod(context.Background(), toolPodSpec()); err == nil {
		t.Fatal("no error")
	}
	if _, err := cs.CoreV1().Secrets("maul-agents").Get(context.Background(), toolPodName+"-tools",
		metav1.GetOptions{}); err == nil {
		t.Error("the Secret outlived the failed Job")
	}
}

// TestToolPodAttribution: under --attribution-mode attribute the tool pod's
// Job and pod template both carry the attributed-to annotation "org" (the
// tool pod is no run's and spends nothing; S2-B1: the chart's Job policy
// refuses an intent-controller Job without it); off, neither carries it,
// exactly as before attribution existed. A person, or anything outside the
// grammar, is refused before anything is created.
func TestToolPodAttribution(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string // "" none
	}{{"", ""}, {"org", "org"}} {
		spec := toolPodSpec()
		spec.AttributedTo = tc.value
		job, _ := renderToolPod(t, toolPodConfig(), spec)
		for what, ann := range map[string]map[string]string{"job": job.Annotations,
			"pod template": job.Spec.Template.Annotations} {
			got, ok := ann[v1alpha1.AnnotationAttributedTo]
			if got != tc.want || ok != (tc.want != "") {
				t.Errorf("AttributedTo %q: the %s carries %q (%v), want %q", tc.value, what, got, ok, tc.want)
			}
		}
	}
	for _, bad := range []string{"github-id:42", "github:alice", "Org", " org"} {
		spec := toolPodSpec()
		spec.AttributedTo = bad
		cs := fake.NewClientset()
		if _, err := New(cs, toolPodConfig(), nil).CreateToolPod(context.Background(), spec); !errors.Is(err,
			ErrToolPodRefused) {
			t.Errorf("AttributedTo %q: err %v, want ErrToolPodRefused", bad, err)
		}
		if n := len(cs.Actions()); n != 0 {
			t.Errorf("AttributedTo %q: %d API calls before the refusal", bad, n)
		}
	}
}
