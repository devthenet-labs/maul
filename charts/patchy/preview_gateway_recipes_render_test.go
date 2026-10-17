// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// The names docs/configuration/preview-gateway.md's recipes use.
const (
	recipeDoc        = "../../docs/configuration/preview-gateway.md"
	recipeSignInHost = "preview-auth.patchy.acme.dev"
	recipeSuffix     = "preview.acme-apps.dev"
	recipeGateway    = "patchy-preview-gateway"
)

// recipeSection is one "### " section of the doc's platform recipes: its
// chart values block (the yaml block that starts with previewAuth:) and
// every other yaml block, parsed as manifests or cloudflared configuration.
type recipeSection struct {
	title  string
	values string
	others []map[string]any
}

// TestPreviewGatewayRecipesRouteTheSignInHost renders every platform recipe
// in docs/configuration/preview-gateway.md that carries chart values, with the
// doc's own host names, and holds that the sign-in host reaches the relay or
// the gateway: by a classed relay Ingress (with TLS off the edge ALB), an
// HTTPRoute the chart renders or the recipe shows (on a Gateway listener the
// recipe shows for that host, with a certificate), or a cloudflared rule. A
// recipe that routes only *.<hostSuffix> leaves sign-in unreachable off EKS.
func TestPreviewGatewayRecipesRouteTheSignInHost(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	sections := recipeSections(t)
	// The recipes with chart values; k3d is the Traefik recipe plus an
	// address, and the wildcard certificate is no recipe.
	want := []string{"EKS: an ALB Ingress", "ingress-nginx", "Traefik", "Gateway API", "Cloudflare Tunnel"}
	var got []string
	for _, sec := range sections {
		if sec.values != "" {
			got = append(got, sec.title)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("recipes with chart values = %v, want %v", got, want)
	}
	for _, sec := range sections {
		if sec.values == "" {
			continue
		}
		t.Run(sec.title, func(t *testing.T) {
			values := filepath.Join(t.TempDir(), "recipe.yaml")
			if err := os.WriteFile(values, []byte(sec.values), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{
				"-f", "../../hack/testdata/chart-render/preview-foundation.yaml",
				"-f", "../../hack/testdata/chart-render/preview-auth.yaml",
				"-f", values,
				"--set", "previewAuth.host=" + recipeSignInHost,
				"--set", "preview.hostSuffix=" + recipeSuffix,
			}
			edge := ""
			if strings.HasPrefix(sec.title, "EKS") {
				edge = "patchy-edge"
				args = append(args, "--set", "edgeIngressClass.create=true",
					"--set", "edgeIngressClass.loadBalancerName=patchy-edge")
			}
			objs := renderObjects(t, args)
			if why := signInRoute(objs, sec.others, edge); why != "" {
				t.Logf("routed: %s", why)
				return
			}
			t.Errorf("recipe %q leaves the sign-in host %s unrouted: no classed relay Ingress with its TLS, no HTTPRoute "+
				"on a listener for it, no cloudflared rule", sec.title, recipeSignInHost)
		})
	}
}

// signInRoute names what routes the sign-in host, or "" for nothing.
func signInRoute(rendered []*unstructured.Unstructured, others []map[string]any, edgeClass string) string {
	for _, o := range rendered {
		switch o.GetKind() {
		case "Ingress":
			class, _, _ := unstructured.NestedString(o.Object, "spec", "ingressClassName")
			if class == "" || !ingressHasHost(o.Object, recipeSignInHost) {
				continue
			}
			if class == edgeClass || ingressTLSCovers(o.Object, recipeSignInHost) {
				return "Ingress " + o.GetName() + " (class " + class + ")"
			}
		case "HTTPRoute":
			if routeHasHost(o.Object, recipeSignInHost) && routeOnSignInListener(o.Object, others) {
				return "the chart's HTTPRoute " + o.GetName()
			}
		}
	}
	for _, o := range others {
		if o["kind"] == "HTTPRoute" && routeHasHost(o, recipeSignInHost) && routeOnSignInListener(o, others) &&
			routeBacksOnto(o, recipeGateway) {
			return "the recipe's HTTPRoute"
		}
		// cloudflared's config.yaml: an ingress list of hostname/service rules.
		if rules, ok := o["ingress"].([]any); ok {
			for _, r := range rules {
				m, _ := r.(map[string]any)
				svc, _ := m["service"].(string)
				if m["hostname"] == recipeSignInHost && strings.Contains(svc, recipeGateway+".") {
					return "a cloudflared rule"
				}
			}
		}
	}
	return ""
}

func ingressHasHost(obj map[string]any, host string) bool {
	rules, _, _ := unstructured.NestedSlice(obj, "spec", "rules")
	for _, r := range rules {
		if m, ok := r.(map[string]any); ok && m["host"] == host {
			return true
		}
	}
	return false
}

func ingressTLSCovers(obj map[string]any, host string) bool {
	tls, _, _ := unstructured.NestedSlice(obj, "spec", "tls")
	for _, e := range tls {
		m, _ := e.(map[string]any)
		hosts, _ := m["hosts"].([]any)
		if slices.Contains(hosts, any(host)) && m["secretName"] != nil {
			return true
		}
	}
	return false
}

func routeHasHost(obj map[string]any, host string) bool {
	names, _, _ := unstructured.NestedSlice(obj, "spec", "hostnames")
	return slices.Contains(names, any(host))
}

func routeBacksOnto(obj map[string]any, service string) bool {
	rules, _, _ := unstructured.NestedSlice(obj, "spec", "rules")
	for _, r := range rules {
		refs, _, _ := unstructured.NestedSlice(r.(map[string]any), "backendRefs")
		for _, ref := range refs {
			if m, ok := ref.(map[string]any); ok && m["name"] == service {
				return true
			}
		}
	}
	return false
}

// routeOnSignInListener: one of the route's parentRefs names a listener of a
// Gateway the recipe shows whose hostname is the sign-in host and which
// carries a certificate.
func routeOnSignInListener(route map[string]any, others []map[string]any) bool {
	parents, _, _ := unstructured.NestedSlice(route, "spec", "parentRefs")
	for _, p := range parents {
		ref, _ := p.(map[string]any)
		for _, g := range others {
			if g["kind"] != "Gateway" {
				continue
			}
			name, _, _ := unstructured.NestedString(g, "metadata", "name")
			if name != ref["name"] {
				continue
			}
			listeners, _, _ := unstructured.NestedSlice(g, "spec", "listeners")
			for _, l := range listeners {
				m, _ := l.(map[string]any)
				certs, _, _ := unstructured.NestedSlice(m, "tls", "certificateRefs")
				if m["name"] == ref["sectionName"] && m["hostname"] == recipeSignInHost && len(certs) > 0 {
					return true
				}
			}
		}
	}
	return false
}

// recipeSections splits the doc's "## Platform recipes" into its "### "
// sections and their yaml blocks.
func recipeSections(t *testing.T) []recipeSection {
	t.Helper()
	raw, err := os.ReadFile(recipeDoc)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, "\n## Platform recipes\n")
	if start < 0 {
		t.Fatal("no Platform recipes section")
	}
	body := doc[start+1:]
	if end := strings.Index(body[3:], "\n## "); end >= 0 {
		body = body[:end+3]
	}
	parts := strings.Split(body, "\n### ")[1:]
	out := make([]recipeSection, 0, len(parts))
	for _, part := range parts {
		title, text, _ := strings.Cut(part, "\n")
		sec := recipeSection{title: title}
		for _, block := range yamlBlocks(text) {
			if isValuesBlock(block) {
				sec.values = block
				continue
			}
			dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(block), 4096)
			for {
				var m map[string]any
				if err := dec.Decode(&m); err != nil {
					break
				}
				if m != nil {
					sec.others = append(sec.others, m)
				}
			}
		}
		out = append(out, sec)
	}
	return out
}

// yamlBlocks returns the bodies of the text's ```yaml fences.
func yamlBlocks(text string) []string {
	var blocks []string
	for {
		i := strings.Index(text, "```yaml\n")
		if i < 0 {
			return blocks
		}
		text = text[i+len("```yaml\n"):]
		j := strings.Index(text, "```")
		if j < 0 {
			return blocks
		}
		blocks = append(blocks, text[:j])
		text = text[j+3:]
	}
}

// isValuesBlock: the block's first non-comment line is previewAuth:.
func isValuesBlock(block string) bool {
	for line := range strings.Lines(block) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line == "previewAuth:"
	}
	return false
}

// renderObjects renders the chart with args and returns every object.
func renderObjects(t *testing.T, args []string) []*unstructured.Unstructured {
	t.Helper()
	all := append([]string{"template", "patchy", ".", "--namespace", "patchy"}, args...)
	output, err := exec.CommandContext(t.Context(), "helm", all...).CombinedOutput()
	if err != nil {
		t.Fatalf("render chart: %v\n%s", err, output)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	var objs []*unstructured.Unstructured
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		if u.Object != nil {
			objs = append(objs, u)
		}
	}
	return objs
}
