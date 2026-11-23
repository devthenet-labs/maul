// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/devthenet-labs/maul/internal/projectsettings"
)

// projectSettingsArgs renders the chart with the Project settings page on,
// every bound at its default.
var projectSettingsArgs = []string{"-f", "../../hack/testdata/chart-render/status-project-settings.yaml"}

// The names the page's objects render under (release maul).
const (
	settingsRole   = "maul-status-server-project-settings"
	settingsPolicy = "maul-project-settings"
	settingsSA     = "system:serviceaccount:maul:maul-status-server"
)

// needHelm skips a test where helm is not on PATH.
func needHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
}

// valuesFile writes a values file for one render and returns its -f args.
func valuesFile(t *testing.T, body string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"-f", path}
}

// renderFails renders with args, wanting a failure; it returns helm's
// output.
func renderFails(t *testing.T, args []string) string {
	t.Helper()
	all := append([]string{"template", "maul", ".", "--namespace", "maul"}, args...)
	out, err := exec.CommandContext(t.Context(), "helm", all...).CombinedOutput()
	if err == nil {
		t.Fatalf("helm template %v rendered, want a failure", args)
	}
	return string(out)
}

// statusServerConfig is the status server's ConfigMap data in a render.
func statusServerConfig(t *testing.T, objs []*unstructured.Unstructured) map[string]string {
	t.Helper()
	cm := findObject(objs, "ConfigMap", "maul-status-server-config")
	if cm == nil {
		t.Fatal("the render has no maul-status-server-config")
	}
	data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
	return data
}

// renderedPolicy is the policy the status server's flag carries in a
// render, read as the server reads it.
func renderedPolicy(t *testing.T, objs []*unstructured.Unstructured) projectsettings.Policy {
	t.Helper()
	raw, ok := statusServerConfig(t, objs)["MAUL_PROJECT_SETTINGS_POLICY"]
	if !ok {
		t.Fatal("the status server's ConfigMap has no MAUL_PROJECT_SETTINGS_POLICY")
	}
	pol, err := projectsettings.ParsePolicy([]byte(raw))
	if err != nil {
		t.Fatalf("the status server would refuse the rendered policy %s: %v", raw, err)
	}
	return pol
}

// settingsPolicyText is every expression and message of the settings admission
// policy, one per line.
func settingsPolicyText(t *testing.T, objs []*unstructured.Unstructured) string {
	t.Helper()
	vap := findObject(objs, "ValidatingAdmissionPolicy", settingsPolicy)
	if vap == nil {
		t.Fatalf("the render has no ValidatingAdmissionPolicy %s", settingsPolicy)
	}
	var b strings.Builder
	for _, key := range []string{"variables", "validations", "matchConditions"} {
		list, _, _ := unstructured.NestedSlice(vap.Object, "spec", key)
		for _, item := range list {
			m, _ := item.(map[string]any)
			for _, f := range []string{"expression", "message"} {
				if s, ok := m[f].(string); ok {
					b.WriteString(s + "\n")
				}
			}
		}
	}
	return b.String()
}

// TestProjectSettingsOffRendersNothing: the page is off by default, and
// with the intents views alone; then nothing of it renders: no grant, no
// policy, no key in the server's ConfigMap.
func TestProjectSettingsOffRendersNothing(t *testing.T) {
	needHelm(t)
	for name, args := range map[string][]string{
		"default":       nil,
		"intents views": {"-f", "../../hack/testdata/chart-render/status-intents.yaml"},
		"status server off": append(slices.Clone(projectSettingsArgs),
			"--set", "statusServer.enabled=false"),
	} {
		t.Run(name, func(t *testing.T) {
			objs := renderObjects(t, args)
			for _, u := range objs {
				if strings.Contains(u.GetName(), "project-settings") {
					t.Errorf("off, the render has %s %s", u.GetKind(), u.GetName())
				}
			}
			if name == "status server off" {
				return
			}
			for k := range statusServerConfig(t, objs) {
				if strings.HasPrefix(k, "MAUL_PROJECT_SETTINGS") {
					t.Errorf("off, the status server's ConfigMap has %s", k)
				}
			}
		})
	}
}

// TestProjectSettingsGrant: on, the status server gains exactly one write,
// patch on projects (never update, never projects/status), named by
// resourceNames when the operator lists Projects, and the page is told the
// same list; the policy matches that ServiceAccount, whatever it is named.
func TestProjectSettingsGrant(t *testing.T) {
	needHelm(t)
	objs := renderObjects(t, projectSettingsArgs)
	role := checkSettingsGrant(t, objs)
	data := statusServerConfig(t, objs)
	if data["MAUL_PROJECT_SETTINGS_ENABLED"] != "true" {
		t.Errorf("MAUL_PROJECT_SETTINGS_ENABLED = %q, want true", data["MAUL_PROJECT_SETTINGS_ENABLED"])
	}
	if v, ok := data["MAUL_PROJECT_SETTINGS_PROJECTS"]; ok {
		t.Errorf("with no projects listed, MAUL_PROJECT_SETTINGS_PROJECTS = %q, want none", v)
	}
	if !strings.Contains(settingsPolicyText(t, objs), "request.userInfo.username == '"+settingsSA+"'") {
		t.Errorf("the policy does not match the status server's ServiceAccount %s", settingsSA)
	}
	checkNoOtherProjectWrite(t, objs, role)
	t.Run("listed projects", func(t *testing.T) {
		objs := renderObjects(t, append(slices.Clone(projectSettingsArgs),
			"--set", "statusServer.projectSettings.projects={alpha,beta}"))
		rules, _, _ := unstructured.NestedSlice(findObject(objs, "Role", settingsRole).Object, "rules")
		want := []any{map[string]any{"apiGroups": []any{"maul.run"}, "resources": []any{"projects"},
			"resourceNames": []any{"alpha", "beta"}, "verbs": []any{"patch"}}}
		if !reflect.DeepEqual(rules, want) {
			t.Errorf("rules = %v, want %v", rules, want)
		}
		if got := statusServerConfig(t, objs)["MAUL_PROJECT_SETTINGS_PROJECTS"]; got != "alpha,beta" {
			t.Errorf("MAUL_PROJECT_SETTINGS_PROJECTS = %q, want alpha,beta", got)
		}
	})
	t.Run("renamed service account", func(t *testing.T) {
		objs := renderObjects(t, append(slices.Clone(projectSettingsArgs),
			"--set", "statusServer.serviceAccount.name=pages"))
		subjects, _, _ := unstructured.NestedSlice(findObject(objs, "RoleBinding", settingsRole).Object, "subjects")
		if name := subjects[0].(map[string]any)["name"]; name != "pages" {
			t.Errorf("the binding names %v, want pages", name)
		}
		renamed := "request.userInfo.username == 'system:serviceaccount:maul:pages'"
		if !strings.Contains(settingsPolicyText(t, objs), renamed) {
			t.Error("the policy does not follow the renamed ServiceAccount")
		}
	})
}

// checkSettingsGrant holds the page's Role and RoleBinding to exactly one
// rule, patch on projects, for the status server's ServiceAccount, and
// returns the Role.
func checkSettingsGrant(t *testing.T, objs []*unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	role := findObject(objs, "Role", settingsRole)
	if role == nil {
		t.Fatalf("on, the render has no Role %s", settingsRole)
	}
	if role.GetNamespace() != "maul" {
		t.Errorf("Role %s is in %q, want the release namespace", settingsRole, role.GetNamespace())
	}
	rules, _, _ := unstructured.NestedSlice(role.Object, "rules")
	want := []any{map[string]any{"apiGroups": []any{"maul.run"}, "resources": []any{"projects"},
		"verbs": []any{"patch"}}}
	if !reflect.DeepEqual(rules, want) {
		t.Errorf("Role %s rules = %v, want exactly %v", settingsRole, rules, want)
	}
	binding := findObject(objs, "RoleBinding", settingsRole)
	if binding == nil {
		t.Fatalf("on, the render has no RoleBinding %s", settingsRole)
	}
	ref, _, _ := unstructured.NestedString(binding.Object, "roleRef", "name")
	subjects, _, _ := unstructured.NestedSlice(binding.Object, "subjects")
	wantSubjects := []any{map[string]any{"kind": "ServiceAccount", "name": "maul-status-server", "namespace": "maul"}}
	if ref != settingsRole || !reflect.DeepEqual(subjects, wantSubjects) {
		t.Errorf("RoleBinding %s = %s %v, want %s %v", settingsRole, ref, subjects, settingsRole, wantSubjects)
	}
	return role
}

// checkNoOtherProjectWrite: no status-server role but the page's grants a
// write on projects or their status.
func checkNoOtherProjectWrite(t *testing.T, objs []*unstructured.Unstructured, role *unstructured.Unstructured) {
	t.Helper()
	for _, u := range objs {
		if (u.GetKind() != "Role" && u.GetKind() != "ClusterRole") || u == role {
			continue
		}
		rs, _, _ := unstructured.NestedSlice(u.Object, "rules")
		for _, r := range rs {
			m, _ := r.(map[string]any)
			res, _ := m["resources"].([]any)
			verbs, _ := m["verbs"].([]any)
			if strings.Contains(u.GetName(), "status-server") && slices.ContainsFunc(res, func(x any) bool {
				return x == "projects" || x == "projects/status"
			}) && slices.ContainsFunc(verbs, func(x any) bool { return x != "get" && x != "list" && x != "watch" }) {
				t.Errorf("%s %s grants %v on %v", u.GetKind(), u.GetName(), verbs, res)
			}
		}
	}
}

// TestProjectSettingsChartDefaultsAreDefaultPolicy: the chart's default
// bounds are projectsettings.DefaultPolicy (owner decision 3), as the
// status server reads them from its ConfigMap.
func TestProjectSettingsChartDefaultsAreDefaultPolicy(t *testing.T) {
	needHelm(t)
	got, want := renderedPolicy(t, renderObjects(t, projectSettingsArgs)), projectsettings.DefaultPolicy()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the chart's default policy is\n%+v\nwant DefaultPolicy\n%+v", got, want)
	}
}

// customPolicyValues sets every bound away from its default, from a values
// file (numbers there are floats to helm, unlike --set's), switches one
// switch on and locks two groups.
const customPolicyValues = `
statusServer:
    projectSettings:
        allow:
            approvers: true
            disableEvaluator: false
            emptyChecks: false
        locked: [stages, models]
        ceilings:
            maxCostMicroUSD: 30000000
            maxRevisions: 0
            maxCheckFixes: 12
            maxVerifyFixes: 4
            maxVerifications: 60
            maxActiveIntents: 3
        floors:
            maxEvaluateCostMicroUSD: 1500000
            evaluateMaxTurns: 45
            evaluateTimeout: 15m
            evaluateTokenBudget: 150000
            maxToolCalls: 0
            maxVerifications: 4
            handoffMax: 1h30m
`

// customPolicy is customPolicyValues as the status server must read it.
func customPolicy() projectsettings.Policy {
	return projectsettings.Policy{
		Allow:  projectsettings.ProjectSettingsAllow{Approvers: true},
		Locked: []projectsettings.Group{projectsettings.GroupStages, projectsettings.GroupModels},
		Ceilings: projectsettings.Ceilings{MaxCostMicroUSD: 30_000_000, MaxRevisions: 0, MaxCheckFixes: 12,
			MaxVerifyFixes: 4, MaxVerifications: 60, MaxActiveIntents: 3},
		Floors: projectsettings.Floors{MaxEvaluateCostMicroUSD: 1_500_000, EvaluateMaxTurns: 45,
			EvaluateTimeout: projectsettings.Duration(15 * time.Minute), EvaluateTokenBudget: 150_000,
			MaxToolCalls: 0, MaxVerifications: 4, HandoffMax: projectsettings.Duration(90 * time.Minute)},
	}
}

// scientific is a number helm wrote in scientific notation, which neither
// the server's JSON nor CEL reads as an integer.
var scientific = regexp.MustCompile(`[0-9]e[+-]?[0-9]`)

// TestProjectSettingsPolicyIsOneSource: the server's flag and the
// admission policy are rendered from the same values: every bound the
// server reads is one the policy enforces, in the same unit, and a lock,
// a switch or a zero floor shows in both.
func TestProjectSettingsPolicyIsOneSource(t *testing.T) {
	needHelm(t)
	args := append(slices.Clone(projectSettingsArgs), valuesFile(t, customPolicyValues)...)
	objs := renderObjects(t, args)
	pol := renderedPolicy(t, objs)
	if want := customPolicy(); !reflect.DeepEqual(pol, want) {
		t.Fatalf("the server reads\n%+v\nwant\n%+v", pol, want)
	}
	text := settingsPolicyText(t, objs)
	raw := statusServerConfig(t, objs)["MAUL_PROJECT_SETTINGS_POLICY"]
	if scientific.MatchString(text) || scientific.MatchString(raw) {
		t.Errorf("a bound is rendered in scientific notation:\n%s\n%s", raw, text)
	}
	checkBoundsSpelled(t, text, raw, pol)
	for _, g := range projectsettings.Groups() {
		if got, want := strings.Contains(text, "the operator locked "+string(g)), pol.Locks(g); got != want {
			t.Errorf("locked %s: in the policy %v, in the server's policy %v", g, got, want)
		}
	}
	// maxToolCalls has no floor (zero); allow.approvers drops the pin.
	if strings.Contains(text, "verification.maxToolCalls below the operator's floor") {
		t.Error("a zero floor is rendered")
	}
	if strings.Contains(text, "the operator has not let the page change the approvers") {
		t.Error("with allow.approvers the approver list is still pinned")
	}
	t.Run("set", func(t *testing.T) { checkSetRendersTheSame(t, text, raw) })
	t.Run("evaluator switch", func(t *testing.T) {
		// allow.disableEvaluator renders no floor at all, and none of the
		// evaluator-off or hand-off rules.
		objs := renderObjects(t, append(slices.Clone(args), "--set",
			"statusServer.projectSettings.allow.disableEvaluator=true",
			"--set", "statusServer.projectSettings.allow.emptyChecks=true"))
		text := settingsPolicyText(t, objs)
		for _, s := range []string{"below the operator's floor", "may not turn verification off",
			"may not move the hand-off earlier", "may not empty checks.fix"} {
			if strings.Contains(text, s) {
				t.Errorf("with the switches on the policy still has %q", s)
			}
		}
	})
}

// checkBoundsSpelled: each bound as the flag spells it, and the field the
// server reads it on: the policy compares that field with that very
// spelling.
func checkBoundsSpelled(t *testing.T, text, raw string, pol projectsettings.Policy) {
	t.Helper()
	flag := policyBlocks(t, []byte(raw))
	for _, kind := range []string{"ceilings", "floors"} {
		for _, key := range policyKeys(t, kind) {
			f, ok := fieldOfPolicyKey(t, kind, key)
			if !ok {
				continue
			}
			ref := celPath("object", f.ID)
			spelled := ""
			switch v := flag[kind][key].(type) {
			case float64:
				spelled = strconv.FormatInt(int64(v), 10)
			case string:
				spelled = "duration('" + v + "')"
			}
			switch _, has := pol.Floor(f.ID); {
			case kind == "ceilings":
				if want := ref + landingSuffix(f) + " <= " + spelled + " ||"; !strings.Contains(text, want) {
					t.Errorf("ceiling of %s: the policy lacks %q", f.ID, want)
				}
			case has:
				if want := " >= " + spelled + " ||"; !strings.Contains(text, want) || !strings.Contains(text, ref) {
					t.Errorf("floor of %s: the policy lacks %q", f.ID, want)
				}
			}
		}
	}
}

// checkSetRendersTheSame: customPolicyValues through --set (integers to
// helm, where a values file's are floats) render the same policy and flag.
func checkSetRendersTheSame(t *testing.T, text, raw string) {
	t.Helper()
	set := []string{"--set", "statusServer.projectSettings.ceilings.maxCostMicroUSD=30000000",
		"--set", "statusServer.projectSettings.ceilings.maxRevisions=0",
		"--set", "statusServer.projectSettings.ceilings.maxCheckFixes=12",
		"--set", "statusServer.projectSettings.ceilings.maxVerifyFixes=4",
		"--set", "statusServer.projectSettings.ceilings.maxVerifications=60",
		"--set", "statusServer.projectSettings.ceilings.maxActiveIntents=3",
		"--set", "statusServer.projectSettings.floors.maxEvaluateCostMicroUSD=1500000",
		"--set", "statusServer.projectSettings.floors.evaluateMaxTurns=45",
		"--set", "statusServer.projectSettings.floors.evaluateTimeout=15m",
		"--set", "statusServer.projectSettings.floors.evaluateTokenBudget=150000",
		"--set", "statusServer.projectSettings.floors.maxToolCalls=0",
		"--set", "statusServer.projectSettings.floors.maxVerifications=4",
		"--set", "statusServer.projectSettings.floors.handoffMax=1h30m",
		"--set", "statusServer.projectSettings.allow.approvers=true",
		"--set", "statusServer.projectSettings.locked={stages,models}"}
	other := renderObjects(t, append(slices.Clone(projectSettingsArgs), set...))
	if got := settingsPolicyText(t, other); got != text {
		t.Errorf("--set renders another policy:\n%s\nwant\n%s", got, text)
	}
	if got := statusServerConfig(t, other)["MAUL_PROJECT_SETTINGS_POLICY"]; got != raw {
		t.Errorf("--set renders another flag: %s, want %s", got, raw)
	}
}

// celPath is a field's optional path in CEL from root (object or
// oldObject): limits.maxRevisions is root.spec.?limits.?maxRevisions.
func celPath(root, id string) string {
	return root + ".spec.?" + strings.ReplaceAll(id, ".", ".?")
}

// landingSuffix is what the policy reads an unset field as: its schema or
// code default, for the fields that have one.
func landingSuffix(f projectsettings.Field) string {
	if !f.HasDefault {
		return ""
	}
	if f.Kind == projectsettings.KindSeconds {
		return ".orValue('" + strconv.FormatInt(f.Default, 10) + "s')"
	}
	return ".orValue(" + strconv.FormatInt(f.Default, 10) + ")"
}

// TestProjectSettingsLandingDefaultsAreTheAPIs: wherever the policy reads
// an unset limit, it reads it as the value it lands on (Field.Default, the
// v1alpha1 default), on the new object and on the old one alike.
func TestProjectSettingsLandingDefaultsAreTheAPIs(t *testing.T) {
	needHelm(t)
	text := settingsPolicyText(t, renderObjects(t, projectSettingsArgs))
	pol := projectsettings.DefaultPolicy()
	for _, f := range projectsettings.Fields() {
		_, ceiling := pol.Ceiling(f.ID)
		_, floor := pol.Floor(f.ID)
		if (!ceiling && !floor) || !f.HasDefault {
			continue
		}
		for _, root := range []string{"object", "oldObject"} {
			if want := celPath(root, f.ID) + landingSuffix(f); !strings.Contains(text, want) {
				t.Errorf("%s: the policy does not read %q", f.ID, want)
			}
		}
		// And never as anything else.
		for _, m := range regexp.MustCompile(regexp.QuoteMeta(celPath("object", f.ID))+`\.orValue\(([^)]*)\)`).
			FindAllStringSubmatch(text, -1) {
			if "orValue("+m[1]+")" != strings.TrimPrefix(landingSuffix(f), ".") {
				t.Errorf("%s: the policy reads unset as %s, want %s", f.ID, m[1], landingSuffix(f))
			}
		}
	}
}

// TestProjectSettingsRenderFailures: the page refuses to render without
// what it stands on: the intents views, the posture that names a person by
// their GitHub id, and the admission policy that is its containment; and
// a policy the server would refuse fails the render, not the pod.
func TestProjectSettingsRenderFailures(t *testing.T) {
	needHelm(t)
	on := []string{"--set", "statusServer.projectSettings.enabled=true"}
	oidc := func(scopes string) []string {
		return []string{"--set", "statusServer.intents.enabled=true",
			"--set-json", `statusServer.auth.config={"mode":"oidc",` +
				`"oidc":{"issuerURL":"https://sso.example.com","clientID":"maul-status","clientSecret":"placeholder",` +
				`"scopes":` + scopes + `,"claims":{"usernamePrefix":"github:","groupsPrefix":"github:"}}}`}
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"without the intents views", append(slices.Clone(on), "--set", "statusServer.auth.existingSecret=maul-status-auth"),
			"statusServer.projectSettings.enabled requires statusServer.intents.enabled"},
		{"without federated:id", append(slices.Clone(on), oidc(`["openid","email"]`)...),
			"statusServer.projectSettings.enabled requires federated:id in statusServer.auth.config.oidc.scopes"},
		{"without the admission policy", append(slices.Clone(projectSettingsArgs), "--set", "admissionPolicy.enabled=false"),
			"statusServer.projectSettings.enabled requires admissionPolicy.enabled"},
		// Chart.yaml's kubeVersion is stricter than the policy's 1.30, so
		// helm refuses the cluster before any template runs.
		{"on a cluster too old for the policy", append(slices.Clone(projectSettingsArgs), "--kube-version", "1.29.0"),
			"incompatible with Kubernetes v1.29.0"},
		{"a floor above its ceiling", append(slices.Clone(projectSettingsArgs),
			"--set", "statusServer.projectSettings.floors.maxVerifications=60"),
			"statusServer.projectSettings.floors.maxVerifications (60) is above ceilings.maxVerifications (50)"},
		{"an unknown lock", append(slices.Clone(projectSettingsArgs),
			"--set", "statusServer.projectSettings.locked={budget}"),
			"locked"},
		{"a missing ceiling", append(slices.Clone(projectSettingsArgs),
			"--set", "statusServer.projectSettings.ceilings.maxRevisions=null"), "maxRevisions"},
		{"a ceiling outside its field", append(slices.Clone(projectSettingsArgs),
			"--set", "statusServer.projectSettings.ceilings.maxRevisions=21"), "maxRevisions"},
		{"a floor outside its field", append(slices.Clone(projectSettingsArgs),
			"--set", "statusServer.projectSettings.floors.maxToolCalls=20"), "maxToolCalls"},
		{"a duration that is not one", append(slices.Clone(projectSettingsArgs),
			"--set", "statusServer.projectSettings.floors.handoffMax=soon"), "handoffMax"},
		{"a Project that is no name", append(slices.Clone(projectSettingsArgs),
			"--set", "statusServer.projectSettings.projects={Not_A_Name}"), "projects"},
		{"an unknown switch", append(slices.Clone(projectSettingsArgs),
			"--set", "statusServer.projectSettings.allow.everything=true"), "everything"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if out := renderFails(t, c.args); !strings.Contains(out, c.want) {
				t.Errorf("the render failed without %q:\n%s", c.want, out)
			}
		})
	}
	t.Run("with federated:id", func(t *testing.T) {
		objs := renderObjects(t, append(slices.Clone(on), oidc(`["openid","email","federated:id"]`)...))
		if findObject(objs, "ValidatingAdmissionPolicy", settingsPolicy) == nil {
			t.Error("an inline oidc posture with federated:id renders no policy")
		}
	})
}

// TestProjectSettingsSchemaRangesAreTheFields: the values schema bounds
// each ceiling and floor to the range of the field it is on (a floor may
// also be zero, no floor), as Policy.Validate does, so a bound the status
// server would refuse at startup fails the render instead. Which key is on
// which field is read from the Policy type itself, never listed here.
func TestProjectSettingsSchemaRangesAreTheFields(t *testing.T) {
	raw, err := os.ReadFile("values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	blocks := schema.Properties["statusServer"].Properties["projectSettings"].Properties
	for _, kind := range []string{"ceilings", "floors"} {
		var block struct {
			Required   []string                   `json:"required"`
			Additional *bool                      `json:"additionalProperties"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(blocks[kind], &block); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		keys := policyKeys(t, kind)
		slices.Sort(block.Required)
		if !slices.Equal(block.Required, keys) || block.Additional == nil || *block.Additional {
			t.Errorf("%s: required %v, additionalProperties %v; want every key of the Policy (%v) required and no other",
				kind, block.Required, block.Additional, keys)
		}
		for _, key := range keys {
			field, ok := fieldOfPolicyKey(t, kind, key)
			if !ok || field.Kind == projectsettings.KindSeconds {
				continue
			}
			checkSchemaBound(t, kind, key, block.Properties[key], field)
		}
	}
}

// checkSchemaBound holds one ceiling or floor's schema to its field's
// range.
func checkSchemaBound(t *testing.T, kind, key string, raw json.RawMessage, field projectsettings.Field) {
	t.Helper()
	var prop struct {
		Type    string `json:"type"`
		Minimum *int64 `json:"minimum"`
		Maximum *int64 `json:"maximum"`
		AnyOf   []struct {
			Const   *int64 `json:"const"`
			Minimum *int64 `json:"minimum"`
			Maximum *int64 `json:"maximum"`
		} `json:"anyOf"`
	}
	if err := json.Unmarshal(raw, &prop); err != nil {
		t.Fatalf("%s.%s: %v", kind, key, err)
	}
	lo, hi := prop.Minimum, prop.Maximum
	if kind == "floors" {
		if len(prop.AnyOf) != 2 || prop.AnyOf[0].Const == nil || *prop.AnyOf[0].Const != 0 {
			t.Errorf("floors.%s: want anyOf [{const: 0}, {minimum, maximum}], got %s", key, raw)
			return
		}
		lo, hi = prop.AnyOf[1].Minimum, prop.AnyOf[1].Maximum
	}
	if prop.Type != "integer" || lo == nil || hi == nil || *lo != field.Min || *hi != field.Max {
		t.Errorf("%s.%s (%s): schema %s, want an integer %d to %d", kind, key, field.ID, raw, field.Min, field.Max)
	}
}

// policyKeys are the JSON keys of one block of the Policy, sorted.
func policyKeys(t *testing.T, kind string) []string {
	t.Helper()
	raw, _ := json.Marshal(projectsettings.DefaultPolicy())
	doc := policyBlocks(t, raw)
	keys := make([]string, 0, len(doc[kind]))
	for k := range doc[kind] {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// fieldOfPolicyKey is the field a Policy key bounds, found by moving that
// one key and seeing which field's bound moves.
func fieldOfPolicyKey(t *testing.T, kind, key string) (projectsettings.Field, bool) {
	t.Helper()
	raw, _ := json.Marshal(projectsettings.DefaultPolicy())
	doc := policyBlocks(t, raw)
	switch v := doc[kind][key].(type) {
	case float64:
		doc[kind][key] = v + 1
	case string:
		doc[kind][key] = "77s"
	}
	whole := map[string]any{"locked": []string{}}
	for k, v := range doc {
		whole[k] = v
	}
	moved, _ := json.Marshal(whole)
	var pol projectsettings.Policy
	if err := json.Unmarshal(moved, &pol); err != nil {
		t.Fatal(err)
	}
	base := projectsettings.DefaultPolicy()
	for _, f := range projectsettings.Fields() {
		get := base.Ceiling
		getMoved := pol.Ceiling
		if kind == "floors" {
			get, getMoved = base.Floor, pol.Floor
		}
		a, _ := get(f.ID)
		b, _ := getMoved(f.ID)
		if a != b {
			return f, true
		}
	}
	t.Errorf("%s.%s bounds no field", kind, key)
	return projectsettings.Field{}, false
}

// policyBlocks are the object blocks of a policy's JSON (allow, ceilings,
// floors) by name.
func policyBlocks(t *testing.T, raw []byte) map[string]map[string]any {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for k, v := range top {
		var block map[string]any
		if json.Unmarshal(v, &block) == nil {
			out[k] = block
		}
	}
	return out
}
