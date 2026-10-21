// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/kube-openapi/pkg/validation/validate"
	"sigs.k8s.io/yaml"
)

// TestLivePins is the CI pin check (`mise run preview-pins`; opt-in, it
// needs the network, helm and docker): every pinned chart version exists
// in its repository, the values the recipes write for it render with that
// chart (so its schema accepts them), every Issuer and Certificate the
// golden trees hold is valid against the cert-manager CRDs at the pinned
// version, unknown fields included, and the pinned cloudflared image runs
// and accepts the config the cloudflare-tunnel recipe writes.
func TestLivePins(t *testing.T) {
	if os.Getenv("MAUL_LIVE_PINS") == "" {
		t.Skip("set MAUL_LIVE_PINS=1 (mise run preview-pins): needs the network and helm")
	}
	if _, err := exec.LookPath("helm"); err != nil {
		t.Fatal("helm not on PATH")
	}
	charts := map[string]string{}
	for _, p := range Pins() {
		charts[p.Chart] = pullChart(t, p)
	}

	liveCertManager(t, charts[CertManager.Chart])
	liveTailscaleOperator(t, charts[TailscaleOperator.Chart])
	liveCloudflared(t)
}

// liveCloudflared: the pinned cloudflared image exists at its tag and
// digest and is the release its tag says, and that cloudflared accepts
// every golden tree's config and routes as the recipe promised: each
// preview host and the sign-in host to the gateway, anything else to the
// 404. It runs the image with docker, offline (`ingress validate` and
// `ingress rule` never contact Cloudflare).
func liveCloudflared(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("docker not on PATH: the cloudflared pin is checked by running it")
	}
	img := Cloudflared.Ref()
	out, err := exec.Command("docker", "run", "--rm", img, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("image pin %s does not run: %v\n%s", img, err, out)
	}
	if !strings.Contains(string(out), "cloudflared version "+Cloudflared.Tag+" ") {
		t.Errorf("image pin %s is not release %s: %s", img, Cloudflared.Tag, out)
	}
	checked := 0
	for name, a := range goldenCases() {
		if a.Edge != EdgeCloudflareTunnel {
			continue
		}
		a.Normalize()
		cfg, err := cloudflaredConfig(a)
		if err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), cfg, 0o644); err != nil {
			t.Fatal(err)
		}
		run := func(args ...string) string {
			t.Helper()
			full := append([]string{"run", "--rm", "-v", dir + ":/etc/cloudflared/config:ro", img, "tunnel",
				"--config", "/etc/cloudflared/config/config.yaml", "ingress"}, args...)
			out, err := exec.Command("docker", full...).CombinedOutput()
			if err != nil {
				t.Fatalf("%s: cloudflared ingress %v: %v\n%s", name, args, err, out)
			}
			return string(out)
		}
		run("validate")
		origin := "service: http://" + a.GatewayService() + "." + a.Namespace + ".svc.cluster.local:80"
		for url, want := range map[string]string{
			"https://pr-1." + a.HostSuffix + "/x": "Matched rule #0",
			"https://" + a.SignInHost + "/start":  "Matched rule #1",
			"https://" + a.HostSuffix + "/":       "Matched rule #2",
			"https://elsewhere.example.invalid/":  "Matched rule #2",
		} {
			got := run("rule", url)
			if !strings.Contains(got, want) || (want != "Matched rule #2") != strings.Contains(got, origin) {
				t.Errorf("%s: cloudflared routes %s as\n%s\nwant %s", name, url, got, want)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no golden tree is a cloudflare-tunnel recipe")
	}
}

// liveCertManager: cert-manager at the pin renders with the recipe's
// values, keeps its CRDs, and accepts every golden Issuer and Certificate.
func liveCertManager(t *testing.T, chart string) {
	t.Helper()
	cmValues, err := certManagerValues()
	if err != nil {
		t.Fatal(err)
	}
	objs := helmTemplate(t, CertManager, chart, cmValues)
	crds := map[string]*structuralValidator{}
	for _, o := range objs {
		if o.GetKind() != "CustomResourceDefinition" {
			continue
		}
		if o.GetAnnotations()["helm.sh/resource-policy"] != "keep" {
			t.Errorf("CRD %s is not kept on uninstall: crds.keep did not take", o.GetName())
		}
		crds[o.GetName()] = newStructuralValidator(t, o)
	}
	for _, name := range []string{"issuers.cert-manager.io", "clusterissuers.cert-manager.io",
		"certificates.cert-manager.io"} {
		if crds[name] == nil {
			t.Fatalf("cert-manager %s renders no CRD %s with crds.enabled", CertManager.Version, name)
		}
	}
	checked := 0
	for name, a := range goldenCases() {
		files := byPath(planWith(t, a, goldenSecrets(name)))
		for path, crd := range map[string]string{IssuerFile: "issuers.cert-manager.io",
			CertificateFile: "certificates.cert-manager.io"} {
			data, ok := files[path]
			if !ok {
				continue
			}
			if path == IssuerFile && a.AmbientIssuer() {
				crd = "clusterissuers.cert-manager.io"
			}
			if errs := crds[crd].validate(t, []byte(data)); len(errs) > 0 {
				t.Errorf("%s/%s is not valid against cert-manager %s: %v", name, path, CertManager.Version, errs)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no golden tree holds an Issuer or a Certificate")
	}
}

// liveTailscaleOperator: the Tailscale operator at the pin renders with
// the recipe's values, without its IngressClass, and mounts the OAuth
// Secret NEXT-STEPS.md has the operator create.
func liveTailscaleOperator(t *testing.T, chart string) {
	t.Helper()
	tsValues, err := tailscaleOperatorValues()
	if err != nil {
		t.Fatal(err)
	}
	var oauth bool
	for _, o := range helmTemplate(t, TailscaleOperator, chart, tsValues) {
		if o.GetKind() == "IngressClass" {
			t.Errorf("the Tailscale operator renders IngressClass %s, which the recipe turned off", o.GetName())
		}
		if o.GetKind() == "Deployment" {
			vols, _, _ := unstructured.NestedSlice(o.Object, "spec", "template", "spec", "volumes")
			for _, v := range vols {
				name, _, _ := unstructured.NestedString(v.(map[string]any), "secret", "secretName")
				oauth = oauth || name == tailscaleOAuthSecret
			}
		}
	}
	if !oauth {
		t.Errorf("the Tailscale operator %s does not mount Secret %s, which NEXT-STEPS.md has the operator create",
			TailscaleOperator.Version, tailscaleOAuthSecret)
	}
}

// pullChart downloads p's chart at its pinned version and returns its
// directory: the pin exists, or the test fails.
func pullChart(t *testing.T, p Pin) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("helm", "pull", p.Chart, "--repo", p.Repo, "--version", p.Version, "--untar",
		"--untardir", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pin %s %s does not exist at %s: %v\n%s", p.Chart, p.Version, p.Repo, err, out)
	}
	return filepath.Join(dir, p.Chart)
}

// helmTemplate renders the chart in dir with values, as NEXT-STEPS.md
// installs it.
func helmTemplate(t *testing.T, p Pin, dir string, values []byte) []*unstructured.Unstructured {
	t.Helper()
	f := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(f, values, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("helm", "template", p.Release, dir, "-n", p.Namespace, "-f", f)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s refuses the recipe's values: %v\n%s", p.Chart, p.Version, err, stderr.String())
	}
	var objs []*unstructured.Unstructured
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(out), 4096)
	for {
		var m map[string]any
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode the %s render: %v", p.Chart, err)
		}
		if m != nil {
			objs = append(objs, &unstructured.Unstructured{Object: m})
		}
	}
	return objs
}

// structuralValidator is one CRD version's schema, as the API server
// would hold an object to it: its OpenAPI schema (types, required fields,
// enums, patterns; the CEL rules are left out, which keeps CEL out of the
// module's dependencies), and the fields it would prune.
type structuralValidator struct {
	schema    *validate.SchemaValidator
	structure *structuralschema.Structural
}

func newStructuralValidator(t *testing.T, o *unstructured.Unstructured) *structuralValidator {
	t.Helper()
	var crd apiextensionsv1.CustomResourceDefinition
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &crd); err != nil {
		t.Fatalf("CRD %s: %v", o.GetName(), err)
	}
	for _, v := range crd.Spec.Versions {
		if v.Name != "v1" || v.Schema == nil {
			continue
		}
		raw, err := json.Marshal(v.Schema.OpenAPIV3Schema)
		if err != nil {
			t.Fatal(err)
		}
		var openapi spec.Schema
		if err := json.Unmarshal(raw, &openapi); err != nil {
			t.Fatal(err)
		}
		internal := &apiextensions.JSONSchemaProps{}
		if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
			v.Schema.OpenAPIV3Schema, internal, nil); err != nil {
			t.Fatal(err)
		}
		structure, err := structuralschema.NewStructural(internal)
		if err != nil {
			t.Fatal(err)
		}
		return &structuralValidator{schema: validate.NewSchemaValidator(&openapi, nil, "", strfmt.Default),
			structure: structure}
	}
	t.Fatalf("CRD %s serves no v1 schema", o.GetName())
	return nil
}

// validate reports what the API server would refuse in a manifest, and
// every field it would silently drop (a misspelt solver setting, say).
func (v *structuralValidator) validate(t *testing.T, data []byte) []string {
	t.Helper()
	var obj map[string]any
	if err := yaml.Unmarshal(data, &obj); err != nil {
		t.Fatal(err)
	}
	res := v.schema.Validate(obj)
	out := make([]string, 0, len(res.Errors))
	for _, e := range res.Errors {
		out = append(out, e.Error())
	}
	pruned := pruning.PruneWithOptions(obj, v.structure, true,
		structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
	for _, p := range pruned {
		out = append(out, "unknown field "+p)
	}
	return out
}
