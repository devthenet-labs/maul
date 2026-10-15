// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package chart_test

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// TestChartNetworkProfilesParse: the profile set the chart renders, from
// the on-fixture, is one netprofile.Parse accepts (what the egress proxy and
// both job controllers do at startup), it is the same string in all three
// ConfigMaps, and the defaults the chart applies are the package's.
func TestChartNetworkProfilesParse(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	args := append([]string{"template", "patchy", ".", "--namespace", "patchy"}, egressOnArgs...)
	output, err := exec.CommandContext(t.Context(), "helm", args...).Output()
	if err != nil {
		t.Fatalf("render chart: %v", err)
	}
	dec := yaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	got := map[string]string{}
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(u); err != nil {
			break
		}
		if u.GetKind() != "ConfigMap" {
			continue
		}
		if v, ok, _ := unstructured.NestedString(u.Object, "data", netprofile.EnvProfiles); ok {
			got[u.GetName()] = v
		}
	}
	want := []string{
		"patchy-egress-proxy-config", "patchy-intent-controller-config", "patchy-remediation-controller-config",
	}
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	if len(names) != len(want) {
		t.Fatalf("%s is in %v, want exactly %v", netprofile.EnvProfiles, names, want)
	}
	first := got[want[0]]
	for _, n := range want {
		if got[n] != first {
			t.Errorf("%s in %s differs from %s's:\n%s\n%s", netprofile.EnvProfiles, n, want[0], got[n], first)
		}
	}
	set, err := netprofile.Parse([]byte(first))
	if err != nil {
		t.Fatalf("netprofile.Parse refuses the chart's profiles: %v\n%s", err, first)
	}
	goProfile, ok := set.Get("go")
	if !ok || set.Len() != 2 {
		t.Fatalf("parsed %d profiles, want go and npm", set.Len())
	}
	if goProfile.Host != netprofile.DefaultHost || goProfile.BakedModCache != "/opt/go/pkg/mod" ||
		goProfile.AllowPseudoVersions {
		t.Errorf("go profile defaults = host %q, bakedModCache %q, allowPseudoVersions %v; want %q, /opt/go/pkg/mod, false",
			goProfile.Host, goProfile.BakedModCache, goProfile.AllowPseudoVersions, netprofile.DefaultHost)
	}
	if !reflect.DeepEqual([]string(goProfile.Modules), []string{"github.com/google/uuid", "golang.org/x"}) {
		t.Errorf("go profile modules = %v, want the trailing slash stripped", goProfile.Modules)
	}
	if p, reason := set.For("https://github.com/devthenet-labs/patchy-target"); p.Name != "go" || reason != "" {
		t.Error("the go profile does not match devthenet-labs/patchy-target")
	}
}
