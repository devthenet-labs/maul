// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"errors"
	"math/rand"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
)

func TestDeniedHostRefuses(t *testing.T) {
	ctx := DenyContext{Hosts: []string{"ghe.example.com", "llm-gateway.example.net"}}
	for _, h := range []string{
		// forges
		"github.com", "api.github.com", "GitHub.com", "github.com.", "objects.githubusercontent.com",
		"raw.githubusercontent.com", "ghe.example.com", "uploads.ghe.example.com",
		// model APIs, every form the broker builds
		"api.anthropic.com", "anthropic.com", "console.anthropic.com", "api.openai.com",
		"x.openai.azure.com", "myresource.services.ai.azure.com", "cognitiveservices.azure.com",
		"x.cognitiveservices.azure.com", "bedrock-runtime.us-east-1.amazonaws.com",
		"bedrock.eu-west-2.amazonaws.com", "bedrock-runtime-fips.us-gov-west-1.amazonaws.com",
		"vpce-0abc.bedrock-runtime.us-east-1.vpce.amazonaws.com", "us-east5-aiplatform.googleapis.com",
		"europe-west4-aiplatform.googleapis.com", "aiplatform.googleapis.com", "x.aiplatform.googleapis.com",
		"generativelanguage.googleapis.com", "llm-gateway.example.net",
		// the Kubernetes API
		"kubernetes", "kubernetes.default", "kubernetes.default.svc", "kubernetes.default.svc.cluster.local",
		// in-cluster names without internal
		"nexus.tools.svc", "nexus.tools.svc.cluster.local",
		// IP literals outside internal CIDRs
		"10.20.30.40", "1.1.1.1", "169.254.169.254", "127.0.0.1", "[::1]", "::1",
		"",
	} {
		if err := DeniedHost(h, ctx); err == nil || !errors.Is(err, ErrDeniedHost) {
			t.Errorf("DeniedHost(%q) = %v, want a refusal", h, err)
		}
	}
}

func TestDeniedHostAdmits(t *testing.T) {
	for _, h := range []string{
		"acme.jfrog.io", "registry.npmjs.org", "files.pythonhosted.org", "pypi.org",
		"maul-gate-377946145366.d.codeartifact.us-east-1.amazonaws.com",
		"bucket.s3.us-east-1.amazonaws.com", "bedrock-runtime.s3.us-east-1.amazonaws.com",
		"storage.googleapis.com", "notgithub.com", "githubx.com", "anthropic.com.evil.example",
		"nexus.corp.internal", "kubernetes-dashboard.example.com",
	} {
		if err := DeniedHost(h, DenyContext{}); err != nil {
			t.Errorf("DeniedHost(%q) = %v, want admitted", h, err)
		}
	}
	internal := DenyContext{Internal: true, InternalCIDRs: []netip.Prefix{netip.MustParsePrefix("10.20.30.0/24")}}
	for _, h := range []string{"nexus.tools.svc", "nexus.tools.svc.cluster.local", "10.20.30.40"} {
		if err := DeniedHost(h, internal); err != nil {
			t.Errorf("DeniedHost(%q, internal) = %v, want admitted", h, err)
		}
	}
	for _, h := range []string{"10.20.31.1", "kubernetes.default.svc", "169.254.169.254"} {
		if err := DeniedHost(h, internal); err == nil {
			t.Errorf("DeniedHost(%q, internal) admitted it", h)
		}
	}
	// An always-denied address stays denied even when an internal CIDR
	// lists it.
	imds := DenyContext{Internal: true, InternalCIDRs: []netip.Prefix{netip.MustParsePrefix("169.254.169.0/24")}}
	if err := DeniedHost("169.254.169.254", imds); err == nil {
		t.Error("DeniedHost admitted IMDS through an internal CIDR")
	}
}

func TestDeniedPatternExamples(t *testing.T) {
	refused := []string{
		"*.amazonaws.com", "*.googleapis.com", "*.githubusercontent.com", "*.azure.com", "*.s3.amazonaws.com",
		"*.cloudfront.net", "*.blob.core.windows.net", "*.core.windows.net", "*.storage.googleapis.com",
		"*.us-east-1.amazonaws.com",           // admits bedrock-runtime.us-east-1.amazonaws.com
		"*.us-east-1.vpce.amazonaws.com",      // admits the Bedrock VPC endpoint form
		"*.example.com",                       // two labels
		"*.com", "*.", "*", "cdn.example.com", // not a pattern
		"*.objects.githubusercontent.com", // below a denied suffix
		"*.api.anthropic.com",
		"*.Example.co.uk", "*.a..b.c", "*.-a.b.c",
		"*.tools.svc.cluster.local",
		"*.1.2.3.4",
	}
	for _, p := range refused {
		if err := DeniedPattern(p, DenyContext{}); err == nil {
			t.Errorf("DeniedPattern(%q) admitted it", p)
		}
	}
	admitted := []string{
		"*.s3.us-east-1.amazonaws.com", "*.acct.blob.core.windows.net", "*.cdn.acme.example.com",
		"*.d.codeartifact.us-east-1.amazonaws.com", "*.files.example.org",
	}
	for _, p := range admitted {
		if err := DeniedPattern(p, DenyContext{}); err != nil {
			t.Errorf("DeniedPattern(%q) = %v, want admitted", p, err)
		}
	}
	// A context host is denied with its subtree, in both directions.
	ctx := DenyContext{Hosts: []string{"ghe.acme.example.com"}}
	for _, p := range []string{"*.acme.example.com", "*.x.ghe.acme.example.com"} {
		if err := DeniedPattern(p, ctx); err == nil {
			t.Errorf("DeniedPattern(%q) with a forge host below or above it admitted it", p)
		}
	}
}

// hostLabels is the pool generated hosts and patterns are drawn from: the
// labels of every table entry and of the rules' fixed positions, so a
// generated pattern lands on the table's edges often.
var hostLabels = []string{
	"github", "githubusercontent", "objects", "anthropic", "api", "openai", "azure", "services", "ai",
	"cognitiveservices", "aiplatform", "us-east5-aiplatform", "googleapis", "generativelanguage", "amazonaws",
	"bedrock", "bedrock-runtime", "s3", "us-east-1", "vpce", "vpce-0a", "com", "net", "io", "example", "acme",
	"cdn", "kubernetes", "default", "svc", "cluster", "local", "blob", "core", "windows", "cloudfront", "x",
}

// genHost is a 1-6 label host from the pool, ending in a plausible TLD
// most of the time.
type genHost string

func (genHost) Generate(r *rand.Rand, _ int) reflect.Value {
	n := 1 + r.Intn(6)
	labels := make([]string, n)
	for i := range labels {
		labels[i] = hostLabels[r.Intn(len(hostLabels))]
	}
	if r.Intn(3) > 0 {
		labels = append(labels, []string{"com", "net", "io"}[r.Intn(3)])
	}
	return reflect.ValueOf(genHost(strings.Join(labels, ".")))
}

// TestPropertyDeniedPatternSound: a pattern DeniedPattern admits matches
// no name DeniedHost refuses. Each admitted pattern is checked against
// names built by putting 1-2 pool labels in front of its suffix (every
// name it matches has that shape), plus the pool's whole host set.
func TestPropertyDeniedPatternSound(t *testing.T) {
	admittedCount := 0
	prop := func(suffix genHost, front1, front2 uint8) bool {
		pattern := "*." + string(suffix)
		if DeniedPattern(pattern, DenyContext{}) != nil {
			return true
		}
		admittedCount++
		for _, f := range []string{
			hostLabels[int(front1)%len(hostLabels)],
			hostLabels[int(front1)%len(hostLabels)] + "." + hostLabels[int(front2)%len(hostLabels)],
			"bedrock-runtime", "us-east5-aiplatform", "vpce-0a.bedrock", "api", "kubernetes.default",
		} {
			name := f + "." + string(suffix)
			if err := DeniedHost(name, DenyContext{}); err != nil {
				t.Logf("%s is admitted but matches %s: %v", pattern, name, err)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(20000)); err != nil {
		t.Fatal(err)
	}
	if admittedCount < 100 {
		t.Fatalf("only %d generated patterns were admitted; the generator no longer reaches the admitted side",
			admittedCount)
	}
}

// TestPropertyDeniedHostCanonical: case and one trailing dot never change
// DeniedHost's verdict.
func TestPropertyDeniedHostCanonical(t *testing.T) {
	prop := func(h genHost) bool {
		a := DeniedHost(string(h), DenyContext{}) == nil
		b := DeniedHost(strings.ToUpper(string(h))+".", DenyContext{}) == nil
		return a == b
	}
	if err := quick.Check(prop, quickConfig(5000)); err != nil {
		t.Fatal(err)
	}
}

func TestAlwaysDeniedCIDRs(t *testing.T) {
	for _, a := range []string{"0.1.2.3", "127.0.0.1", "169.254.169.254", "169.254.170.23", "224.0.0.1",
		"255.255.255.255"} {
		addr := netip.MustParseAddr(a)
		found := false
		for _, p := range AlwaysDeniedCIDRs {
			found = found || p.Contains(addr)
		}
		if !found {
			t.Errorf("%s is not in AlwaysDeniedCIDRs", a)
		}
	}
}
