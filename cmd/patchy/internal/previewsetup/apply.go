// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ApplyFieldManager is the server-side apply field manager `setup previews
// --apply` writes the generated manifests as.
const ApplyFieldManager = "patchy-setup-previews"

// DefaultApplyTimeout bounds each wait --apply makes: a Helm install, the
// Certificate becoming Ready, cloudflared rolling out.
const DefaultApplyTimeout = 10 * time.Minute

// ApplyInput is what --apply needs beyond the answers, each named by the
// operator and never guessed (setup-previews-easy (d)): the values the
// patchy release already runs with, and the chart version to upgrade it
// to.
type ApplyInput struct {
	// Dir is the output directory the generated files are in.
	Dir string
	// ValuesFiles are the operator's own values files for the patchy
	// release, in order; the generated values file is layered after them.
	ValuesFiles []string
	// ChartVersion is the patchy chart version the release is upgraded to.
	ChartVersion string
	// Timeout bounds each wait.
	Timeout time.Duration
}

// StepKind is what an apply step does.
type StepKind string

// The apply steps.
const (
	// StepPreflight checks that helm runs and the patchy release is
	// installed, before anything is changed.
	StepPreflight StepKind = "preflight"
	// StepApply server-side applies the manifests in Files, in order.
	StepApply StepKind = "apply"
	// StepNamespace creates Namespace unless it exists.
	StepNamespace StepKind = "namespace"
	// StepRequireSecret requires the Secret Name in Namespace to exist,
	// reading its metadata only.
	StepRequireSecret StepKind = "require-secret"
	// StepHelm upgrades (or installs) a Helm release.
	StepHelm StepKind = "helm"
	// StepWaitCertificate waits for a cert-manager Certificate to be Ready.
	StepWaitCertificate StepKind = "wait-certificate"
	// StepWaitDeployment waits for a Deployment to roll out.
	StepWaitDeployment StepKind = "wait-deployment"
	// StepCheck runs the checks `patchy check previews` runs.
	StepCheck StepKind = "check"
)

// Step is one in-cluster step of --apply, in the order NEXT-STEPS.md lists
// them.
type Step struct {
	Kind  StepKind
	Title string
	// Files are the manifests a StepApply applies, as paths under the
	// output directory, in order.
	Files []string
	// Secret says Files hold a secret: an error names no content.
	Secret bool
	// SkipUnlessNamespace makes a StepApply a skip, not a failure, when
	// that namespace does not exist in the cluster (Dex running elsewhere).
	SkipUnlessNamespace string
	// Namespace and Name are what a namespace, require-secret, wait or
	// check step reads.
	Namespace, Name string
	// Helm is the release a StepHelm upgrades, or the one a StepPreflight
	// requires to be installed.
	Helm *HelmRelease
	// Timeout bounds a Helm or wait step.
	Timeout time.Duration
}

// HelmRelease is one `helm upgrade`.
type HelmRelease struct {
	Release, Chart, Repo, Version, Namespace string
	// Values are the values files, in order.
	Values []string
	// Install lets the upgrade install a release that is not there yet
	// (an upstream chart); the patchy release must already be installed.
	Install bool
	// CreateNamespace creates the release's namespace with it.
	CreateNamespace bool
}

// HelmArgs is the helm command line (without the binary, the kubeconfig
// or the context) that upgrades h and waits up to timeout: what --apply
// runs and what --apply --dry-run prints, from one place.
func HelmArgs(h HelmRelease, timeout time.Duration) []string {
	args := []string{"upgrade"}
	if h.Install {
		args = append(args, "--install")
	}
	args = append(args, h.Release, h.Chart)
	if h.Repo != "" {
		args = append(args, "--repo", h.Repo)
	}
	args = append(args, "--version", h.Version, "-n", h.Namespace)
	if h.CreateNamespace {
		args = append(args, "--create-namespace")
	}
	for _, v := range h.Values {
		args = append(args, "-f", v)
	}
	return append(args, "--wait", "--timeout", timeout.String())
}

// ErrApplyInput reports --apply input the plan refuses.
var ErrApplyInput = errors.New("--apply")

// NormalizeChartVersion is the chart version a --chart-version names, its
// leading v dropped, or "" when it is no release version.
func NormalizeChartVersion(v string) string { return ChartVersion(v) }

// ApplySteps plans --apply for a, whose files are files, written under
// in.Dir: the in-cluster steps of NEXT-STEPS.md, in order, ending with the
// checks. Dex's configuration, DNS records, the tailnet policy, preview
// nodes and image pulls are left out: ApplyManual names them. Pure: it
// reads nothing, and the same input gives the same steps.
func ApplySteps(a Answers, files []File, in ApplyInput) ([]Step, error) {
	if len(in.ValuesFiles) == 0 {
		return nil, fmt.Errorf("%w needs --values: the values file(s) release %s already runs with, which "+
			"the upgrade layers %s after", ErrApplyInput, a.Release, ValuesFile)
	}
	version := NormalizeChartVersion(in.ChartVersion)
	if version == "" {
		return nil, fmt.Errorf("%w needs --chart-version: the patchy chart version to upgrade release %s to, "+
			"such as 0.12.41 (%q is not one)", ErrApplyInput, a.Release, in.ChartVersion)
	}
	if in.Timeout <= 0 {
		return nil, fmt.Errorf("%w: the wait timeout must be positive", ErrApplyInput)
	}
	has := map[string]bool{}
	for _, f := range files {
		has[f.Path] = true
	}
	for _, need := range []string{ValuesFile, RelayDexSecretFile, DexClientSecretFile} {
		if !has[need] {
			return nil, fmt.Errorf("%w: the plan has no %s", ErrApplyInput, need)
		}
	}
	path := func(rel string) string { return filepath.Join(in.Dir, filepath.FromSlash(rel)) }
	patchy := HelmRelease{Release: a.Release, Chart: ChartRef, Version: version, Namespace: a.Namespace,
		Values: append(slices.Clone(in.ValuesFiles), path(ValuesFile))}
	steps := []Step{{Kind: StepPreflight, Helm: &patchy,
		Title: fmt.Sprintf("Check that helm runs and release %s is installed in namespace %s", a.Release,
			a.Namespace)}}

	// A Secret the operator creates is required before anything changes.
	if a.WritesTailscaleOAuth() && !has[TailscaleOAuthFile] {
		op := TailscaleOperator
		steps = append(steps, Step{Kind: StepRequireSecret, Namespace: op.Namespace, Name: tailscaleOAuthSecret,
			Title: "Require the Tailscale operator's OAuth client, Secret " + tailscaleOAuthSecret +
				" in namespace " + op.Namespace + " (its metadata only)"})
	}

	relay := Step{Kind: StepApply, Secret: true, Files: []string{path(RelayDexSecretFile)},
		Title: "Apply the relay's Dex client secret in namespace " + a.Namespace}
	if has[DNSSecretFile] {
		relay.Files = append(relay.Files, path(DNSSecretFile))
		relay.Title = "Apply the relay's Dex client secret and the DNS-01 credential in namespace " + a.Namespace
	}
	steps = append(steps, relay, Step{Kind: StepApply, Secret: true, Files: []string{path(DexClientSecretFile)},
		SkipUnlessNamespace: a.Dex.Namespace,
		Title: "Apply Dex's copy of the client secret in namespace " + a.Dex.Namespace + " (skipped when no " +
			"such namespace is here: Dex runs elsewhere)"})
	if has[TailscaleOAuthFile] {
		ns := TailscaleOperator.Namespace
		steps = append(steps,
			Step{Kind: StepNamespace, Namespace: ns, Title: "Create namespace " + ns + " unless it exists"},
			Step{Kind: StepApply, Secret: true, Files: []string{path(TailscaleOAuthFile)},
				Title: "Apply the Tailscale operator's OAuth client, Secret " + tailscaleOAuthSecret + " in namespace " +
					ns})
	}
	steps = append(steps, tlsSteps(a, has, path, in.Timeout)...)
	if a.Edge == EdgeTailscale && !a.Tailscale.OperatorInstalled {
		steps = append(steps, upstreamStep(TailscaleOperator, path, in.Timeout))
	}
	if a.Edge == EdgeCloudflareTunnel {
		ns := a.CloudflaredNamespace()
		steps = append(steps,
			Step{Kind: StepApply, Secret: true, Files: []string{path(CloudflaredFile), path(CloudflaredSecretFile)},
				Title: "Apply cloudflared and the tunnel credentials in namespace " + ns},
			Step{Kind: StepWaitDeployment, Namespace: ns, Name: cloudflaredName, Timeout: in.Timeout,
				Title: "Wait for deployment/" + cloudflaredName + " in namespace " + ns + " to roll out"})
	}
	return append(steps,
		Step{Kind: StepHelm, Helm: &patchy, Timeout: in.Timeout,
			Title: fmt.Sprintf("Upgrade release %s to chart %s with your values, then %s", a.Release, version,
				ValuesFile)},
		Step{Kind: StepCheck, Namespace: a.Namespace, Title: "Run the checks of patchy check previews"}), nil
}

// tlsSteps are the certificate steps: cert-manager when the recipe
// installs it, the Issuer and Certificate it writes (waiting for the
// Certificate), or the sign-in route on a Gateway API edge.
func tlsSteps(a Answers, has map[string]bool, path func(string) string, timeout time.Duration) []Step {
	var steps []Step
	if a.GatewayTerminatesTLS() && !a.Certificate.CertManagerInstalled {
		steps = append(steps, upstreamStep(CertManager, path, timeout))
	}
	if has[IssuerFile] {
		kind, name := a.Issuer()
		steps = append(steps, Step{Kind: StepApply, Files: []string{path(IssuerFile)},
			Title: "Apply the " + kind + " " + name})
	}
	if has[CertificateFile] {
		steps = append(steps,
			Step{Kind: StepApply, Files: []string{path(CertificateFile)},
				Title: "Apply the Certificate " + CertificateName + " in namespace " + a.Namespace},
			Step{Kind: StepWaitCertificate, Namespace: a.Namespace, Name: CertificateName, Timeout: timeout,
				Title: "Wait for certificate/" + CertificateName + " to be Ready"})
	}
	if has[SignInRouteFile] {
		steps = append(steps, Step{Kind: StepApply, Files: []string{path(SignInRouteFile)},
			Title: "Apply the HTTPRoute " + SignInRouteName + " in namespace " + a.Namespace})
	}
	return steps
}

// upstreamStep installs or upgrades a pinned upstream chart from the values
// the recipe wrote for it.
func upstreamStep(p Pin, path func(string) string, timeout time.Duration) Step {
	return Step{Kind: StepHelm, Timeout: timeout,
		Title: "Install or upgrade " + p.Chart + " " + p.Version + " (release " + p.Release + " in namespace " +
			p.Namespace + ")",
		Helm: &HelmRelease{Release: p.Release, Chart: p.Chart, Repo: p.Repo, Version: p.Version,
			Namespace: p.Namespace, Values: []string{path(p.ValuesFile)}, Install: true, CreateNamespace: true}}
}

// Commands is the step as the commands that do the same by hand: what
// --apply --dry-run prints. --apply itself talks to the API server (a
// server-side apply as ApplyFieldManager) and runs helm as HelmArgs says.
func (s Step) Commands() []string {
	switch s.Kind {
	case StepPreflight:
		return []string{"helm version --short", "helm status " + shellWords(s.Helm.Release, "-n", s.Helm.Namespace)}
	case StepApply:
		args := []string{"--server-side", "--force-conflicts", "--field-manager=" + ApplyFieldManager}
		for _, f := range s.Files {
			args = append(args, "-f", f)
		}
		return []string{"kubectl apply " + shellWords(args...)}
	case StepNamespace:
		return []string{"kubectl create namespace " + shellWords(s.Namespace) +
			" --dry-run=client -o yaml | kubectl apply -f -"}
	case StepRequireSecret:
		return []string{"kubectl -n " + shellWords(s.Namespace, "get", "secret", s.Name) + " -o name"}
	case StepHelm:
		return []string{"helm " + shellWords(HelmArgs(*s.Helm, s.Timeout)...)}
	case StepWaitCertificate:
		return []string{"kubectl -n " + shellWords(s.Namespace, "wait", "certificate/"+s.Name,
			"--for=condition=Ready", "--timeout="+s.Timeout.String())}
	case StepWaitDeployment:
		return []string{"kubectl -n " + shellWords(s.Namespace, "rollout", "status", "deployment/"+s.Name,
			"--timeout="+s.Timeout.String())}
	case StepCheck:
		return []string{"patchy check previews -n " + shellWords(s.Namespace)}
	}
	return nil
}

// ApplyManual names what --apply leaves to the operator, each with the
// NEXT-STEPS.md step that says how: before, what must be done before it
// runs (the tailnet policy owns the tag the operator's devices carry);
// after, what is done once it has.
func ApplyManual(a Answers) (before, after []string) {
	if a.Edge == EdgeTailscale {
		before = append(before, "the tailnet policy, saved in the admin console (NEXT-STEPS.md step 6)")
		if !a.Tailscale.OperatorInstalled {
			before = append(before, "the operator's OAuth client, given with --tailscale-oauth-dir or created as "+
				"Secret "+tailscaleOAuthSecret+" in namespace "+TailscaleOperator.Namespace+" (step 6)")
		}
	}
	after = []string{
		"preview nodes, labelled and tainted (NEXT-STEPS.md step 2)",
		"preview image pulls on those nodes (step 3)",
		"Dex: its static client " + a.Dex.ClientID + " and the secret's environment variable, then roll Dex " +
			"(step 4)",
	}
	if a.Edge == EdgeCloudflareTunnel {
		after = append(after, "the two proxied DNS records and the Cache Rule in Cloudflare (step 7)")
	} else {
		after = append(after, "the two DNS records (step 7)")
	}
	return before, after
}

// RenderApplyPlan is what --apply --dry-run prints: what must be done
// first, each step with the commands that do it by hand, then what is
// left to the operator.
func RenderApplyPlan(a Answers, steps []Step) string {
	var b strings.Builder
	before, after := ApplyManual(a)
	if len(before) > 0 {
		b.WriteString("Before --apply, as NEXT-STEPS.md says:\n")
		for _, m := range before {
			b.WriteString("  - " + m + "\n")
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "--apply would run these %d steps against the cluster your kubeconfig names, in order, "+
		"stopping at the first failure:\n", len(steps))
	for i, s := range steps {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, s.Title)
		for _, c := range s.Commands() {
			fmt.Fprintf(&b, "     %s\n", c)
		}
	}
	b.WriteString("\nManifests are server-side applied as " + ApplyFieldManager + "; a Secret's contents are " +
		"never printed.\n\nLeft to you after it, as NEXT-STEPS.md says:\n")
	for _, m := range after {
		b.WriteString("  - " + m + "\n")
	}
	return b.String()
}

// shellWords joins words for a shell, quoting any that need it.
func shellWords(words ...string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shellQuote(w)
	}
	return strings.Join(quoted, " ")
}

// shellQuote quotes w for a POSIX shell when it holds anything beyond
// the characters that never need it.
func shellQuote(w string) string {
	if w != "" && strings.IndexFunc(w, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && !strings.ContainsRune("-_./:=@,+%", r)
	}) < 0 {
		return w
	}
	return "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
}
