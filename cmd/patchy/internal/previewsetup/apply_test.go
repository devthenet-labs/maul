// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/quick"
	"time"
)

// testApplyInput is the --apply input the golden plans are rendered with.
func testApplyInput() ApplyInput {
	return ApplyInput{Dir: "patchy-previews", ValuesFiles: []string{"values/patchy.yaml"}, ChartVersion: "v0.12.41",
		Timeout: DefaultApplyTimeout}
}

// applySteps plans --apply for golden case name.
func applySteps(t *testing.T, name string) []Step {
	t.Helper()
	a := goldenCases()[name]
	a.Normalize()
	steps, err := ApplySteps(a, planWith(t, a, goldenSecrets(name)), testApplyInput())
	if err != nil {
		t.Fatalf("ApplySteps: %v", err)
	}
	return steps
}

// TestApplyPlanGolden pins what --apply --dry-run prints for every golden
// tree.
func TestApplyPlanGolden(t *testing.T) {
	for name, a := range goldenCases() {
		t.Run(name, func(t *testing.T) {
			a.Normalize()
			got := RenderApplyPlan(a, applySteps(t, name))
			path := filepath.Join("testdata", "apply-plan", name+".txt")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path) //nolint:gosec // the test's own golden file
			if err != nil {
				t.Fatalf("read %s (run with -update to create): %v", path, err)
			}
			if string(want) != got {
				t.Errorf("%s differs (run with -update to accept):\n%s", path,
					unifiedDiff(path, want, []byte(got)))
			}
			for _, s := range []string{sentinel.value, string(dnsSentinel.value), string(tunnelSentinel),
				string(tailscaleOAuthSentinel.secret)} {
				if strings.Contains(got, s) {
					t.Errorf("the plan carries a secret")
				}
			}
		})
	}
}

// index is the position of the first step matching ok, or -1.
func index(steps []Step, ok func(Step) bool) int { return slices.IndexFunc(steps, ok) }

// applies matches the apply step holding the file rel.
func applies(rel string) func(Step) bool {
	return func(s Step) bool {
		return s.Kind == StepApply && slices.ContainsFunc(s.Files, func(f string) bool {
			return strings.HasSuffix(filepath.ToSlash(f), "/"+rel)
		})
	}
}

// helm matches the helm step of release.
func helm(release string) func(Step) bool {
	return func(s Step) bool { return s.Kind == StepHelm && s.Helm.Release == release }
}

// TestApplyStepsOrder holds every plan to NEXT-STEPS.md's order: nothing
// changed before the preflight, every Secret before what reads it,
// cert-manager before its Issuer, the Issuer before the Certificate and
// its wait, the operator's OAuth client before the operator, patchy's
// upgrade after the edge, and the checks last; every generated manifest
// is applied exactly once.
func TestApplyStepsOrder(t *testing.T) {
	for name := range goldenCases() {
		t.Run(name, func(t *testing.T) {
			steps := applySteps(t, name)
			if steps[0].Kind != StepPreflight || steps[len(steps)-1].Kind != StepCheck {
				t.Fatalf("first %s, last %s", steps[0].Kind, steps[len(steps)-1].Kind)
			}
			patchy := index(steps, helm(goldenCases()[name].Release))
			if patchy != len(steps)-2 {
				t.Errorf("patchy's upgrade is step %d of %d", patchy+1, len(steps))
			}
			files := goldenFiles(t, name)
			checkTLSOrder(t, steps, files, patchy)
			checkOperatorOrder(t, steps, patchy)
			checkAppliedOnce(t, steps, files)
		})
	}
}

// before fails t unless step x comes before step y.
func before(t *testing.T, x, y int, what string) {
	t.Helper()
	if x < 0 || y < 0 || x >= y {
		t.Errorf("%s: steps %d and %d", what, x+1, y+1)
	}
}

// checkTLSOrder: the Secrets, cert-manager, the Issuer, the Certificate
// and its wait, in that order, before patchy's upgrade.
func checkTLSOrder(t *testing.T, steps []Step, files map[string]bool, patchy int) {
	t.Helper()
	before(t, index(steps, applies(RelayDexSecretFile)), patchy, "relay Secret before patchy")
	if files[DNSSecretFile] {
		before(t, index(steps, applies(DNSSecretFile)), index(steps, applies(IssuerFile)), "DNS before Issuer")
	}
	if cm := index(steps, helm(CertManager.Release)); cm >= 0 && files[IssuerFile] {
		before(t, cm, index(steps, applies(IssuerFile)), "cert-manager before Issuer")
	}
	if !files[CertificateFile] {
		return
	}
	if files[IssuerFile] {
		before(t, index(steps, applies(IssuerFile)), index(steps, applies(CertificateFile)),
			"Issuer before Certificate")
	}
	wait := index(steps, func(s Step) bool { return s.Kind == StepWaitCertificate })
	before(t, index(steps, applies(CertificateFile)), wait, "Certificate before its wait")
	before(t, wait, patchy, "Certificate Ready before patchy")
}

// checkOperatorOrder: the operator's OAuth client (written, or required
// before anything changes) before the operator, and it before patchy.
func checkOperatorOrder(t *testing.T, steps []Step, patchy int) {
	t.Helper()
	op := index(steps, helm(TailscaleOperator.Release))
	if op < 0 {
		return
	}
	oauth := index(steps, applies(TailscaleOAuthFile))
	if oauth < 0 {
		oauth = index(steps, func(s Step) bool { return s.Kind == StepRequireSecret })
		if oauth != 1 {
			t.Error("the operator's Secret is required only after something changed")
		}
	}
	before(t, oauth, op, "OAuth client before the operator")
	before(t, op, patchy, "operator before patchy")
}

// checkAppliedOnce: every manifest and Secret file is applied exactly
// once, and nothing else is.
func checkAppliedOnce(t *testing.T, steps []Step, files map[string]bool) {
	t.Helper()
	applied := map[string]int{}
	for _, s := range steps {
		for _, f := range s.Files {
			rel, err := filepath.Rel("patchy-previews", f)
			if err != nil {
				t.Fatal(err)
			}
			applied[filepath.ToSlash(rel)]++
		}
	}
	for f := range files {
		manifest := strings.HasPrefix(f, "manifests/") || strings.HasPrefix(f, "secrets/")
		if manifest && applied[f] != 1 {
			t.Errorf("%s applied %d times", f, applied[f])
		}
		if !manifest && applied[f] != 0 {
			t.Errorf("%s is no manifest but is applied", f)
		}
	}
}

// goldenFiles is the set of paths golden case name writes.
func goldenFiles(t *testing.T, name string) map[string]bool {
	t.Helper()
	m := map[string]bool{}
	for _, f := range planWith(t, goldenCases()[name], goldenSecrets(name)) {
		m[f.Path] = true
	}
	return m
}

// TestApplyStepsRecipes pins which upstream releases each recipe installs
// and which waits it makes.
func TestApplyStepsRecipes(t *testing.T) {
	for name, want := range map[string][]string{
		"existing-alb-eks":         {"preflight", "apply", "apply", "helm patchy", "check"},
		"existing-traefik-k3s":     {"preflight", "apply", "apply", "apply", "wait-certificate", "helm patchy", "check"},
		"existing-gateway-api-aks": {"preflight", "apply", "apply", "apply", "helm patchy", "check"},
		"tailscale-manual-k3s":     {"preflight", "apply", "apply", "apply", "wait-certificate", "helm patchy", "check"},
		"gateway-tls-digitalocean-k3s": {"preflight", "apply", "apply", "helm cert-manager", "apply", "apply",
			"wait-certificate", "helm patchy", "check"},
		"tailscale-route53-k3d": {"preflight", "apply", "apply", "namespace", "apply", "helm cert-manager", "apply",
			"apply", "wait-certificate", "helm tailscale-operator", "helm patchy", "check"},
		"tailscale-shared-domain-k3d": {"preflight", "require-secret", "apply", "apply", "helm cert-manager", "apply",
			"apply", "wait-certificate", "helm tailscale-operator", "helm patchy", "check"},
		"cloudflare-tunnel-k3s": {"preflight", "apply", "apply", "apply", "wait-deployment", "helm patchy", "check"},
	} {
		var got []string
		for _, s := range applySteps(t, name) {
			k := string(s.Kind)
			if s.Kind == StepHelm {
				k += " " + s.Helm.Release
			}
			got = append(got, k)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: steps %v, want %v", name, got, want)
		}
	}
}

// TestApplyStepsInput: --apply refuses to guess the values or the chart
// version.
func TestApplyStepsInput(t *testing.T) {
	a := goldenCases()["existing-alb-eks"]
	a.Normalize()
	files := planWith(t, a, goldenSecrets("existing-alb-eks"))
	for name, mutate := range map[string]func(*ApplyInput){
		"no values":        func(in *ApplyInput) { in.ValuesFiles = nil },
		"no chart version": func(in *ApplyInput) { in.ChartVersion = "" },
		"not a version":    func(in *ApplyInput) { in.ChartVersion = "latest" },
		"no timeout":       func(in *ApplyInput) { in.Timeout = 0 },
	} {
		in := testApplyInput()
		mutate(&in)
		if _, err := ApplySteps(a, files, in); !errors.Is(err, ErrApplyInput) {
			t.Errorf("%s: err = %v, want ErrApplyInput", name, err)
		}
	}
	if _, err := ApplySteps(a, files[:1], testApplyInput()); !errors.Is(err, ErrApplyInput) {
		t.Errorf("a plan without its Secrets: err = %v", err)
	}
	steps, err := ApplySteps(a, files, testApplyInput())
	if err != nil {
		t.Fatal(err)
	}
	if h := steps[len(steps)-2].Helm; h.Version != "0.12.41" || h.Install ||
		!slices.Equal(h.Values, []string{"values/patchy.yaml", filepath.Join("patchy-previews", ValuesFile)}) {
		t.Errorf("patchy's upgrade = %+v", h)
	}
}

// TestHelmArgs pins the helm command line --apply runs.
func TestHelmArgs(t *testing.T) {
	got := HelmArgs(HelmRelease{Release: "cert-manager", Chart: "cert-manager", Repo: "https://charts.jetstack.io",
		Version: "v1.21.2", Namespace: "cert-manager", Values: []string{"a.yaml", "b c.yaml"}, Install: true,
		CreateNamespace: true}, 90*time.Second)
	want := []string{"upgrade", "--install", "cert-manager", "cert-manager", "--repo", "https://charts.jetstack.io",
		"--version", "v1.21.2", "-n", "cert-manager", "--create-namespace", "-f", "a.yaml", "-f", "b c.yaml",
		"--wait", "--timeout", "1m30s"}
	if !slices.Equal(got, want) {
		t.Errorf("HelmArgs = %q", got)
	}
	got = HelmArgs(HelmRelease{Release: "patchy", Chart: ChartRef, Version: "0.12.41", Namespace: "patchy",
		Values: []string{"v.yaml"}}, time.Minute)
	want = []string{"upgrade", "patchy", ChartRef, "--version", "0.12.41", "-n", "patchy", "-f", "v.yaml", "--wait",
		"--timeout", "1m0s"}
	if !slices.Equal(got, want) {
		t.Errorf("HelmArgs = %q", got)
	}
}

// unquoteShell reads back one word shellQuote wrote: bare, or single
// quotes with '\” for each quote inside. ok is false for anything else.
func unquoteShell(s string) (string, bool) {
	if !strings.HasPrefix(s, "'") {
		return s, s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`;&|<>()*?[]#~!{}")
	}
	var b strings.Builder
	for s != "" {
		if !strings.HasPrefix(s, "'") {
			return "", false
		}
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", false
		}
		b.WriteString(s[1 : 1+end])
		s = s[2+end:]
		if strings.HasPrefix(s, `\'`) {
			b.WriteByte('\'')
			s = s[2:]
		}
	}
	return b.String(), true
}

// TestShellQuoteProperty: every quoted word reads back as itself, as one
// word, so a printed command names exactly the paths --apply uses.
func TestShellQuoteProperty(t *testing.T) {
	prop := func(w string) bool {
		got, ok := unquoteShell(shellQuote(w))
		return ok && got == w
	}
	if err := quick.Check(prop, quickConfig(2000)); err != nil {
		t.Error(err)
	}
	for _, w := range []string{"", "'", "a b", "it's", `\'`, "$HOME", "~/v.yaml", "-f", "a=b,c"} {
		if !prop(w) {
			t.Errorf("%q quotes as %q", w, shellQuote(w))
		}
	}
}

// TestApplyPlanConnection: with --kubeconfig and --context given, every
// command the plan prints names them, kubectl's and patchy's as
// --context and helm's as --kube-context, and the header names the
// cluster; without them, no command carries a connection flag.
func TestApplyPlanConnection(t *testing.T) {
	conn := Connection{Kubeconfig: "/home/me/my kube.yaml", Context: "staging"}
	for name, a := range goldenCases() {
		t.Run(name, func(t *testing.T) {
			a.Normalize()
			in := testApplyInput()
			in.Connection = conn
			steps, err := ApplySteps(a, planWith(t, a, goldenSecrets(name)), in)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range steps {
				for _, c := range s.Commands() {
					for _, part := range strings.Split(c, " | ") {
						var want string
						switch {
						case strings.HasPrefix(part, "kubectl "):
							want = "kubectl --kubeconfig '/home/me/my kube.yaml' --context staging "
						case strings.HasPrefix(part, "helm "):
							want = "helm --kubeconfig '/home/me/my kube.yaml' --kube-context staging "
						case strings.HasPrefix(part, "patchy "):
							if !strings.HasSuffix(part, " --kubeconfig '/home/me/my kube.yaml' --context staging") {
								t.Errorf("%q lacks the connection", part)
							}
							continue
						default:
							t.Errorf("unexpected command %q", part)
						}
						if !strings.HasPrefix(part, want) {
							t.Errorf("%q lacks the connection", part)
						}
					}
				}
			}
			plan := RenderApplyPlan(a, steps)
			if !strings.Contains(plan, "against context staging of kubeconfig '/home/me/my kube.yaml'") {
				t.Errorf("the header does not name the cluster:\n%s", plan)
			}
			if strings.Contains(RenderApplyPlan(a, applySteps(t, name)), "--context") {
				t.Error("a plan with no connection flags carries one")
			}
		})
	}
}

// TestApplyStepsFreshSecret: a Dex client secret drawn anew this run
// marks both of its files as never to replace a Secret in the cluster,
// and the plan says so; one read back from disk marks nothing.
func TestApplyStepsFreshSecret(t *testing.T) {
	const name = "tailscale-route53-k3d"
	a := goldenCases()[name]
	a.Normalize()
	in := testApplyInput()
	in.FreshDexSecret = true
	steps, err := ApplySteps(a, planWith(t, a, goldenSecrets(name)), in)
	if err != nil {
		t.Fatal(err)
	}
	var guarded []string
	for _, s := range steps {
		for _, f := range s.NoReplace {
			if !slices.Contains(s.Files, f) {
				t.Errorf("%s is guarded but not applied by its step", f)
			}
			guarded = append(guarded, filepath.ToSlash(f))
		}
	}
	want := []string{"patchy-previews/" + RelayDexSecretFile, "patchy-previews/" + DexClientSecretFile}
	if !slices.Equal(guarded, want) {
		t.Errorf("guarded %v, want %v", guarded, want)
	}
	if !strings.Contains(RenderApplyPlan(a, steps), "refuses to go on when the Dex client secret") {
		t.Error("the plan does not say it refuses")
	}
	for _, s := range applySteps(t, name) {
		if len(s.NoReplace) > 0 {
			t.Errorf("a secret read back guards %v", s.NoReplace)
		}
	}
}
