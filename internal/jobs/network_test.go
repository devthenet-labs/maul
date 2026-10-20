// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/netprofile"
	"github.com/devthenet-labs/maul/internal/runnerimage"
)

// proxyHost is the egress proxy's Service name the profile fixtures dial.
const proxyHost = "maul-egress-proxy.maul.svc.cluster.local"

// goProfile is the devthenet "go" profile as a controller hands it to a Job.
func goProfile() *NetworkProfile {
	return &NetworkProfile{
		Name: "go", Route: netprofile.RouteGo, ProxyURL: "http://" + proxyHost + ":8090",
		BakedModCache: "/opt/go/pkg/mod",
	}
}

// profileConfig is injectedConfig with network profiles on.
func profileConfig() Config {
	cfg := injectedConfig()
	cfg.NetworkProfiles = true
	return cfg
}

// remediateProfileSpec is a Finding's remediate Job on the repository's
// image, asking for the go profile.
func remediateProfileSpec() Spec {
	spec := injectedSpec()
	spec.Phase, spec.Kind = "remediate", "remediation"
	spec.Owner = "finding-abc123def0-1-rem-1"
	spec.InvestigationMarkdown = "# Investigation\n"
	spec.NetworkProfile = goProfile()
	return spec
}

// buildProfileSpec is an intent's build Job on the repository's image,
// asking for the go profile.
func buildProfileSpec() Spec {
	spec := injectedSpec()
	spec.Phase, spec.Kind = "build", "intent"
	spec.Owner = "intent-x-bld-r1-a1"
	spec.Finding = "intent-x-bld-r1-a1"
	spec.IssueMarkdown = ""
	spec.InvestigationMarkdown = "# Plan\n"
	spec.NetworkProfile = goProfile()
	return spec
}

// wantGoEnv is the §4.3 environment, verbatim and in order.
var wantGoEnv = []corev1.EnvVar{
	{Name: "GOENV", Value: "off"},
	{Name: "GOPROXY", Value: "http://" + proxyHost + ":8090/go,file:///opt/go/pkg/mod/cache/download"},
	{Name: "GOSUMDB", Value: "sum.golang.org"},
	{Name: "GONOSUMDB", Value: "maul.invalid"},
	{Name: "GONOPROXY", Value: "maul.invalid"},
	{Name: "GOPRIVATE", Value: "maul.invalid"},
	{Name: "GOINSECURE", Value: "maul.invalid"},
	{Name: "GOAUTH", Value: "off"},
	{Name: "GOTOOLCHAIN", Value: "local"},
	{Name: "GOPATH", Value: "/tmp/go"},
	{Name: "GOMODCACHE", Value: "/tmp/go/pkg/mod"},
	{Name: "MAUL_NETWORK_PROFILE", Value: "go"},
}

func agentOf(t *testing.T, pod corev1.PodSpec) corev1.Container {
	t.Helper()
	for _, ct := range pod.Containers {
		if ct.Name == agentContainerName {
			return ct
		}
	}
	t.Fatal("no agent container")
	return corev1.Container{}
}

// TestGoldenNetworkProfileJobs pins the two profile Jobs whole: the label
// on the Job and its pod, the go environment at the end of the agent
// container's, and nothing else. Clearing those gives back the same Job
// built without a profile, byte for byte.
func TestGoldenNetworkProfileJobs(t *testing.T) {
	for _, tt := range []struct {
		golden string
		spec   Spec
	}{
		{"job_remediate_network_profile", remediateProfileSpec()},
		{"job_build_network_profile", buildProfileSpec()},
	} {
		t.Run(tt.golden, func(t *testing.T) {
			job := buildJobForTest(t, profileConfig(), tt.spec)
			goldenJob(t, tt.golden, job)

			plain := tt.spec
			plain.NetworkProfile = nil
			want := buildJobForTest(t, profileConfig(), plain)
			key := netprofile.LabelKey("go")
			delete(job.Labels, key)
			delete(job.Spec.Template.Labels, key)
			ct := &job.Spec.Template.Spec.Containers[0]
			ct.Env = ct.Env[:len(ct.Env)-len(wantGoEnv)]
			// The go names a repository-image Job does not own come back
			// in their usual place once the profile's copies are gone.
			if got, w := marshal(t, job), marshal(t, want); got != w {
				t.Errorf("the profile changed more than its label and env:\n--- want\n%s\n--- got\n%s", w, got)
			}
		})
	}
}

// TestGoldenNetworkProfileDNSNoneJob pins a build Job with a profile and
// no resolver: the hosts file pins the broker, the artifact server and the
// egress proxy.
func TestGoldenNetworkProfileDNSNoneJob(t *testing.T) {
	r := clusterResolver()
	r.answers[proxyHost] = []string{"10.100.50.9"}
	cfg := profileConfig()
	cfg.DNS, cfg.Resolver = DNSNone, r
	spec := buildProfileSpec()
	job, err := New(fake.NewClientset(), cfg, nil).build(context.Background(),
		NameFor(spec.Finding, spec.Kind, int32(spec.Attempt)), spec)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	goldenJob(t, "job_build_network_profile_dns_none", job)
	if !slices.Contains(r.asked, proxyHost) {
		t.Errorf("resolver asked %v, want the proxy host among them", r.asked)
	}
}

// TestNetworkProfileHostAliases: under DNSNone the proxy's host is
// resolved and pinned beside the others; under DNSCluster the pod has no
// hostAliases and the resolver is never asked.
func TestNetworkProfileHostAliases(t *testing.T) {
	r := clusterResolver()
	r.answers[proxyHost] = []string{"10.100.50.9"}
	cfg := profileConfig()
	cfg.DNS, cfg.Resolver = DNSNone, r
	job := createJob(t, cfg, buildProfileSpec())
	var found bool
	for _, ha := range job.Spec.Template.Spec.HostAliases {
		if ha.IP == "10.100.50.9" && slices.Equal(ha.Hostnames, []string{proxyHost}) {
			found = true
		}
	}
	if !found {
		t.Errorf("hostAliases = %+v, want %s pinned at 10.100.50.9", job.Spec.Template.Spec.HostAliases, proxyHost)
	}

	failing := &fakeResolver{errs: map[string]error{}}
	cluster := profileConfig()
	cluster.Resolver = failing
	job = createJob(t, cluster, buildProfileSpec())
	if ha := job.Spec.Template.Spec.HostAliases; len(ha) != 0 {
		t.Errorf("DNSCluster hostAliases = %+v, want none", ha)
	}
	if len(failing.asked) != 0 {
		t.Errorf("DNSCluster asked the resolver %v, want nothing", failing.asked)
	}

	// The proxy host failing to resolve fails the launch, as any endpoint.
	broken := clusterResolver()
	cfg.Resolver = broken
	_, _, err := New(fake.NewClientset(), cfg, nil).Create(context.Background(), buildProfileSpec())
	if !errors.Is(err, ErrUnresolved) {
		t.Errorf("Create with an unresolvable proxy = %v, want ErrUnresolved", err)
	}
}

// TestNetworkProfileEnvOwnsItsNames: whatever set a go name before — the
// operator's Config.Env, the runner's Env, an injectEnv blank — the profile
// Job carries the profile's value for it exactly once, every value
// non-empty (an empty one would fall through to the image's
// $GOROOT/go.env), and GOPROXY is the proxy first, the baked cache last,
// never direct or off.
func TestNetworkProfileEnvOwnsItsNames(t *testing.T) {
	cfg := profileConfig()
	cfg.Env["GOPROXY"] = "direct"
	cfg.Env["GOFLAGS"] = "-mod=mod"
	claude := cfg.Runners["claude"]
	claude.Env = maps.Clone(claude.Env)
	claude.Env["GOSUMDB"] = "off"
	claude.Env["GONOSUMDB"] = ""
	cfg.Runners["claude"] = claude
	for _, spec := range []Spec{remediateProfileSpec(), buildProfileSpec()} {
		ct := agentOf(t, buildJobForTest(t, cfg, spec).Spec.Template.Spec)
		count := map[string]int{}
		for _, e := range ct.Env {
			count[e.Name]++
		}
		env := envMap(ct)
		for _, want := range wantGoEnv {
			if count[want.Name] != 1 {
				t.Errorf("%s: %s listed %d times, want once", spec.Phase, want.Name, count[want.Name])
			}
			if got := env[want.Name]; got.Value != want.Value || got.ValueFrom != nil || got.Value == "" {
				t.Errorf("%s: %s = %+v, want %q", spec.Phase, want.Name, got, want.Value)
			}
		}
		goproxy := strings.Split(env["GOPROXY"].Value, ",")
		if goproxy[0] != spec.NetworkProfile.ProxyURL+"/go" || !strings.HasPrefix(goproxy[len(goproxy)-1], "file://") {
			t.Errorf("GOPROXY = %q, want the proxy first and the baked cache last", env["GOPROXY"].Value)
		}
		for _, entry := range goproxy {
			if entry == "direct" || entry == "off" {
				t.Errorf("GOPROXY = %q holds %q", env["GOPROXY"].Value, entry)
			}
		}
		if got := env["GOFLAGS"].Value; got != "-mod=mod" {
			t.Errorf("GOFLAGS = %q, want the operator's value left alone", got)
		}
		// The proxy variables stay blanked: the route is a GOPROXY URL.
		for _, name := range proxyEnv {
			if e, ok := env[name]; !ok || e.Value != "" {
				t.Errorf("%s = %+v, want blanked", name, e)
			}
		}
	}
}

// TestNetworkProfileWithoutBakedCache: no baked cache is the network only.
func TestNetworkProfileWithoutBakedCache(t *testing.T) {
	spec := buildProfileSpec()
	spec.NetworkProfile.BakedModCache = ""
	env := envMap(agentOf(t, buildJobForTest(t, profileConfig(), spec).Spec.Template.Spec))
	if got, want := env["GOPROXY"].Value, "http://"+proxyHost+":8090/go"; got != want {
		t.Errorf("GOPROXY = %q, want %q", got, want)
	}
}

// TestNetworkProfileRefused: a profile on any Job but a remediate Job of
// kind remediation or a build Job of kind intent, beside trees, or one that
// does not hold together, is refused before anything is created.
func TestNetworkProfileRefused(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Spec)
	}{
		{"investigate", func(s *Spec) { s.Phase, s.Kind = "investigate", "investigation" }},
		{"plan", func(s *Spec) { s.Phase, s.Kind = "plan", "intent" }},
		{"build of kind remediation", func(s *Spec) { s.Phase, s.Kind = "build", "remediation" }},
		{"remediate of kind intent", func(s *Spec) { s.Phase, s.Kind = "remediate", "intent" }},
		{"remediate of kind investigation", func(s *Spec) { s.Phase, s.Kind = "remediate", "investigation" }},
		{"build of kind evaluation", func(s *Spec) { s.Phase, s.Kind = "build", "evaluation" }},
		{"trees", func(s *Spec) {
			s.Trees = []Tree{{Key: "b", URL: "https://github.com/o/b", ArtifactURL: "http://a/b.tar.gz",
				ArtifactDigest: strings.Repeat("a", 64), BaseSHA: strings.Repeat("b", 40)}}
		}},
		{"bad name", func(s *Spec) { s.NetworkProfile.Name = "Go" }},
		{"empty name", func(s *Spec) { s.NetworkProfile.Name = "" }},
		{"route npm", func(s *Spec) { s.NetworkProfile.Route = "npm" }},
		{"https proxy", func(s *Spec) { s.NetworkProfile.ProxyURL = "https://" + proxyHost + ":8090" }},
		{"proxy without port", func(s *Spec) { s.NetworkProfile.ProxyURL = "http://" + proxyHost }},
		{"proxy with path", func(s *Spec) { s.NetworkProfile.ProxyURL = "http://" + proxyHost + ":8090/go" }},
		{"proxy with query", func(s *Spec) { s.NetworkProfile.ProxyURL = "http://" + proxyHost + ":8090?x=1" }},
		{"proxy with user", func(s *Spec) { s.NetworkProfile.ProxyURL = "http://u:p@" + proxyHost + ":8090" }},
		{"proxy with fragment", func(s *Spec) { s.NetworkProfile.ProxyURL = "http://" + proxyHost + ":8090#f" }},
		{"proxy uppercase host", func(s *Spec) { s.NetworkProfile.ProxyURL = "http://Proxy.example:8090" }},
		{"proxy with comma", func(s *Spec) { s.NetworkProfile.ProxyURL = "http://a,direct:8090" }},
		{"proxy port zero", func(s *Spec) { s.NetworkProfile.ProxyURL = "http://" + proxyHost + ":0" }},
		{"relative baked cache", func(s *Spec) { s.NetworkProfile.BakedModCache = "opt/go/pkg/mod" }},
		{"unclean baked cache", func(s *Spec) { s.NetworkProfile.BakedModCache = "/opt/go/../mod" }},
		{"baked cache with comma", func(s *Spec) { s.NetworkProfile.BakedModCache = "/opt/a,direct" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := buildProfileSpec()
			tt.edit(&spec)
			cs := fake.NewClientset()
			_, _, err := New(cs, profileConfig(), nil).Create(context.Background(), spec)
			if !errors.Is(err, ErrNetworkProfileRefused) {
				t.Fatalf("Create = %v, want ErrNetworkProfileRefused", err)
			}
			secrets, _ := cs.CoreV1().Secrets("maul-agents").List(context.Background(), metav1.ListOptions{})
			jobsList, _ := cs.BatchV1().Jobs("maul-agents").List(context.Background(), metav1.ListOptions{})
			if len(secrets.Items) != 0 || len(jobsList.Items) != 0 {
				t.Errorf("a refused Create left %d Secrets and %d Jobs", len(secrets.Items), len(jobsList.Items))
			}
		})
	}
	// An IP literal proxy is a valid one.
	spec := buildProfileSpec()
	spec.NetworkProfile.ProxyURL = "http://10.100.50.9:8090"
	if err := profileRefusal(spec); err != nil {
		t.Errorf("IP proxy refused: %v", err)
	}
}

// TestNetworkProfileDropped: with the profile kill switch off, repository
// images off, a runner with nothing to inject, or no pinned image, the Job
// is the profile-less Job byte for byte, and the read-back is "".
func TestNetworkProfileDropped(t *testing.T) {
	tests := []struct {
		name string
		cfg  func() Config
		spec func() Spec
	}{
		{"profiles off", func() Config {
			c := profileConfig()
			c.NetworkProfiles = false
			return c
		}, buildProfileSpec},
		{"repository images off", func() Config {
			c := profileConfig()
			c.AllowRepositoryImages = false
			return c
		}, buildProfileSpec},
		{"runner without inject", func() Config {
			c := profileConfig()
			claude := c.Runners["claude"]
			claude.Inject = nil
			c.Runners["claude"] = claude
			return c
		}, remediateProfileSpec},
		{"no pinned image", profileConfig, func() Spec {
			s := remediateProfileSpec()
			s.RunnerImage, s.RunnerSearchPath, s.RunnerImageManifest = "", "", ""
			return s
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, spec := tt.cfg(), tt.spec()
			plain := spec
			plain.NetworkProfile = nil
			if got, want := marshal(t, buildJobForTest(t, cfg, spec)), marshal(t, buildJobForTest(t, cfg, plain)); got != want {
				t.Errorf("a dropped profile changed the Job:\n--- want\n%s\n--- got\n%s", want, got)
			}
			if _, ref := createWithRef(t, cfg, spec); ref.NetworkProfile != "" {
				t.Errorf("read-back network profile = %q, want empty", ref.NetworkProfile)
			}
		})
	}
}

// TestCreateReportsNetworkProfile: Create returns the profile read off the
// Job it created, and off one it adopted, whatever the retry asked for.
func TestCreateReportsNetworkProfile(t *testing.T) {
	_, ref := createWithRef(t, profileConfig(), buildProfileSpec())
	want := v1alpha1.RunnerImageRef{Image: repoImage, Source: v1alpha1.RunnerImageSourceRepository,
		Manifest: runnerimage.AgentYAMLPath, NetworkProfile: "go"}
	if ref != want {
		t.Errorf("Create returned %+v, want %+v", ref, want)
	}

	cs := fake.NewClientset()
	name, first, err := New(cs, profileConfig(), nil).Create(context.Background(), remediateProfileSpec())
	if err != nil {
		t.Fatal(err)
	}
	off := profileConfig()
	off.NetworkProfiles = false
	again, ref2, err := New(cs, off, nil).Create(context.Background(), remediateProfileSpec())
	if err != nil {
		t.Fatal(err)
	}
	if again != name || ref2 != first || ref2.NetworkProfile != "go" {
		t.Errorf("adopting Create returned %q %+v, want %q %+v", again, ref2, name, first)
	}

	// And the other way: a Job created without a profile reads back none,
	// even when the adopting retry asks for one.
	cs = fake.NewClientset()
	if _, _, err := New(cs, off, nil).Create(context.Background(), remediateProfileSpec()); err != nil {
		t.Fatal(err)
	}
	if _, ref3, err := New(cs, profileConfig(), nil).Create(context.Background(), remediateProfileSpec()); err != nil ||
		ref3.NetworkProfile != "" {
		t.Errorf("adopting a profile-less Job returned %+v, %v; want no profile", ref3, err)
	}
}

// TestNetworkProfileSecretUnchanged: the label is on the Job and its pod,
// never on the per-Job Secret.
func TestNetworkProfileSecretUnchanged(t *testing.T) {
	spec := buildProfileSpec()
	plain := spec
	plain.NetworkProfile = nil
	name := NameFor(spec.Finding, spec.Kind, int32(spec.Attempt))
	if got, want := marshal(t, buildSecret(name, "maul-agents", spec)),
		marshal(t, buildSecret(name, "maul-agents", plain)); got != want {
		t.Errorf("the profile changed the Secret:\n--- want\n%s\n--- got\n%s", want, got)
	}
}

func TestNetworkProfileOf(t *testing.T) {
	job := buildJobForTest(t, profileConfig(), buildProfileSpec())
	if got := networkProfileOf(job); got != "go" {
		t.Errorf("networkProfileOf = %q, want go", got)
	}
	job.Labels[netprofile.LabelKey("npm")] = "true"
	if got := networkProfileOf(job); got != "" {
		t.Errorf("networkProfileOf with two labels = %q, want empty", got)
	}
	delete(job.Labels, netprofile.LabelKey("npm"))
	job.Labels[netprofile.LabelKey("go")] = "false"
	if got := networkProfileOf(job); got != "" {
		t.Errorf("networkProfileOf with a false label = %q, want empty", got)
	}
}

// TestGoProfileNamesNotReserved: no go name a profile owns is reserved, so
// an image whose ENV sets GOPROXY=off, GOMODCACHE or GOTOOLCHAIN (every
// scaffolded image does) still resolves; MAUL_NETWORK_PROFILE alone is,
// and no profile name is blanked on a repository-image Job without one.
func TestGoProfileNamesNotReserved(t *testing.T) {
	reserved := ReservedEnvNames()
	for _, name := range goProfileNames {
		if name == netprofile.EnvProfile {
			if !slices.Contains(reserved, name) || !slices.Contains(PerJobEnvNames(), name) {
				t.Errorf("%s is not a reserved per-Job name", name)
			}
			continue
		}
		if slices.Contains(reserved, name) {
			t.Errorf("%s is reserved; an image's ENV may set it", name)
		}
		if slices.Contains(scrubEnv, name) {
			t.Errorf("%s is scrubbed on every repository-image Job", name)
		}
	}
	for _, e := range InjectedEnv(repoSearchPath) {
		if slices.Contains(goProfileNames, e.Name) {
			t.Errorf("InjectedEnv sets %s", e.Name)
		}
	}
}

// TestPropertyNetworkProfileOnlyWhereAllowed: over phase × kind × image ×
// repository images × inject × the profile kill switch × profile × DNS
// mode, a Job carries the profile label and the go environment iff its
// (phase, kind) is (remediate, remediation) or (build, intent), it runs the
// repository's image, profiles are on and a profile was asked for; a
// profile asked for on any other pair is an error, never a Job. A profile
// adds no credential, no SecretKeyRef and no reserved name but
// MAUL_NETWORK_PROFILE (and, with tunnel hosts, MAUL_NETWORK_ROUTES and
// MAUL_NETWORK_HOSTS), and the proxy variables stay blanked on every Job
// except a tunnel-profile Job, where exactly HTTPS_PROXY, https_proxy,
// NO_PROXY and no_proxy carry the controller's values and the plain-http
// and ALL_PROXY names stay blank (step 3, §4.3). The profile is drawn from
// a go, a tunnel-only and a go-plus-tunnel profile. Seeded so the gate is
// deterministic.
func TestPropertyNetworkProfileOnlyWhereAllowed(t *testing.T) {
	rng := rand.New(rand.NewSource(20261014))
	r := clusterResolver()
	r.answers[proxyHost] = []string{"10.100.50.9"}
	tunnels := 0
	for i := range 2000 {
		pc := drawProfileCase(rng, r)
		spec := pc.spec
		if pc.want && len(spec.NetworkProfile.Hosts) > 0 {
			tunnels++
		}
		job, err := New(fake.NewClientset(), pc.cfg, nil).build(context.Background(),
			NameFor(spec.Finding, spec.Kind, int32(spec.Attempt)), spec)
		if spec.NetworkProfile != nil && !pc.pair {
			if !errors.Is(err, ErrNetworkProfileRefused) {
				t.Fatalf("iteration %d: %s/%s with a profile: err = %v, want a refusal", i, spec.Phase, spec.Kind, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("iteration %d: build: %v", i, err)
		}
		if msg := pc.check(t, job); msg != "" {
			t.Fatalf("iteration %d: %s/%s %+v: %s", i, spec.Phase, spec.Kind, pc, msg)
		}
	}
	if tunnels == 0 {
		t.Error("no draw built a tunnel-profile Job")
	}
}

// profileCase is one draw of TestPropertyNetworkProfileOnlyWhereAllowed.
type profileCase struct {
	cfg  Config
	spec Spec
	// pair is an allowed (Phase, Kind); want is the oracle: the Job
	// carries the profile.
	pair, want bool
}

func drawProfileCase(rng *rand.Rand, r Resolver) profileCase {
	phases := []string{"investigate", "remediate", "plan", "build"}
	kinds := []string{"investigation", "remediation", "intent"}
	coin := func() bool { return rng.Intn(2) == 0 }
	cfg := profileConfig()
	cfg.AllowRepositoryImages, cfg.NetworkProfiles = coin(), coin()
	inject := coin()
	if !inject {
		claude := cfg.Runners["claude"]
		claude.Inject = nil
		cfg.Runners["claude"] = claude
	}
	if coin() {
		cfg.DNS, cfg.Resolver = DNSNone, r
	}
	spec := injectedSpec()
	spec.Phase, spec.Kind = phases[rng.Intn(len(phases))], kinds[rng.Intn(len(kinds))]
	pinned := coin()
	if !pinned {
		spec.RunnerImage, spec.RunnerSearchPath, spec.RunnerImageManifest = "", "", ""
	}
	set := coin()
	if set {
		spec.NetworkProfile = []func() *NetworkProfile{goProfile, tunnelProfile, goTunnelProfile}[rng.Intn(3)]()
	}
	pair := spec.Phase == "remediate" && spec.Kind == "remediation" || spec.Phase == "build" && spec.Kind == "intent"
	injected := cfg.AllowRepositoryImages && pinned && inject
	return profileCase{cfg: cfg, spec: spec, pair: pair, want: pair && injected && cfg.NetworkProfiles && set}
}

// shape is the drawn profile's name, whether it has the Go route and
// whether it has a tunnel; the go profile's when none was drawn.
func (pc profileCase) shape() (name string, goRoute, tunnel bool) {
	if p := pc.spec.NetworkProfile; p != nil {
		return p.Name, p.Route == netprofile.RouteGo, len(p.Hosts) > 0
	}
	return "go", true, false
}

// check judges a built Job against the oracle, returning what is wrong.
func (pc profileCase) check(t *testing.T, job *batchv1.Job) string {
	t.Helper()
	name, goRoute, tunnel := pc.shape()
	key := netprofile.LabelKey(name)
	_, labelled := job.Labels[key]
	_, podLabelled := job.Spec.Template.Labels[key]
	agent := agentOf(t, job.Spec.Template.Spec)
	env := envMap(agent)
	_, hasProfileEnv := env[netprofile.EnvProfile]
	hasGoProxy := env["GOPROXY"].Value != ""
	if labelled != pc.want || podLabelled != pc.want || hasProfileEnv != pc.want || hasGoProxy != (pc.want && goRoute) {
		return fmt.Sprintf("label=%v pod=%v env=%v goproxy=%v, want %v (go route %v)",
			labelled, podLabelled, hasProfileEnv, hasGoProxy, pc.want, goRoute)
	}
	if got := networkProfileOf(job); got != map[bool]string{true: name}[pc.want] {
		return fmt.Sprintf("read-back %q", got)
	}
	if !pc.want {
		return ""
	}
	plain := pc.spec
	plain.NetworkProfile = nil
	base := envMap(agentOf(t, buildJobForTest(t, pc.cfg, plain).Spec.Template.Spec))
	perProfile := []string{netprofile.EnvProfile}
	if tunnel {
		perProfile = append(perProfile, netprofile.EnvRoutes, netprofile.EnvHosts)
	}
	for _, e := range agent.Env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			return "a SecretKeyRef on " + e.Name
		}
		if _, before := base[e.Name]; !before && reservedEnv[e.Name] && !slices.Contains(perProfile, e.Name) {
			return "the profile added reserved name " + e.Name
		}
	}
	return checkProxyEnv(env, pc.spec.NetworkProfile, tunnel)
}

// checkProxyEnv judges the proxy variables: blank, except on a tunnel Job,
// where the https names carry the profile's proxy and the no-proxy names
// the broker and the proxy.
func checkProxyEnv(env map[string]corev1.EnvVar, p *NetworkProfile, tunnel bool) string {
	for _, name := range proxyEnv {
		e, ok := env[name]
		switch {
		case tunnel && (name == "HTTPS_PROXY" || name == "https_proxy"):
			if e.Value != p.ProxyURL {
				return fmt.Sprintf("%s = %+v, want the profile's proxy", name, e)
			}
		case tunnel && (name == "NO_PROXY" || name == "no_proxy"):
			if !strings.Contains(e.Value, brokerHost) || !strings.Contains(e.Value, proxyHost) {
				return fmt.Sprintf("%s = %+v, want the broker and the proxy in it", name, e)
			}
		case !ok || e.Value != "":
			return fmt.Sprintf("%s = %+v, want blanked", name, e)
		}
	}
	return ""
}
