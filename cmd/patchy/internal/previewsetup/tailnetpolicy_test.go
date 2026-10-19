// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"encoding/json"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"
)

// tailnetPolicy is the part of a Tailscale policy file the recipe writes.
type tailnetPolicy struct {
	Groups    map[string][]string `json:"groups"`
	TagOwners map[string][]string `json:"tagOwners"`
	Grants    []struct {
		Src []string `json:"src"`
		Dst []string `json:"dst"`
		IP  []string `json:"ip"`
	} `json:"grants"`
	Tests []policyTest `json:"tests"`
}

// policyTest is one test a policy save runs.
type policyTest struct {
	Src    string   `json:"src"`
	Accept []string `json:"accept"`
	Deny   []string `json:"deny"`
}

// parsePolicy reads the HuJSON policy as Tailscale would: its line
// comments dropped, then strict JSON (no field the recipe does not know).
func parsePolicy(t *testing.T, hujson string) (tailnetPolicy, error) {
	t.Helper()
	var lines []string
	for l := range strings.SplitSeq(hujson, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "//") {
			lines = append(lines, l)
		}
	}
	dec := json.NewDecoder(strings.NewReader(strings.Join(lines, "\n")))
	dec.DisallowUnknownFields()
	var p tailnetPolicy
	err := dec.Decode(&p)
	return p, err
}

// judgePolicy says what is wrong with the policy for tag, "" when nothing:
// the operator owns its tags (and is owned by admins alone); exactly two
// grants, members to their own devices and the viewers to tag on tcp:443
// alone; nothing reaches a wildcard; and the tests a save runs pin both
// the owner's own access and the device's closure (port 80, another tag).
func judgePolicy(p tailnetPolicy, tag string) string {
	for _, judge := range []func(tailnetPolicy, string) string{judgeOwners, judgeGrants, judgeTests} {
		if why := judge(p, tag); why != "" {
			return why
		}
	}
	return ""
}

func judgeOwners(p tailnetPolicy, tag string) string {
	switch {
	case p.TagOwners[TailscaleOperatorTag] == nil || len(p.TagOwners[TailscaleOperatorTag]) != 0:
		return "the operator's tag is not admins' alone"
	case !slices.Equal(p.TagOwners[tag], []string{TailscaleOperatorTag}),
		!slices.Equal(p.TagOwners["tag:k8s"], []string{TailscaleOperatorTag}):
		return "the operator does not own the device tags"
	case !slices.Equal(p.Groups["group:preview-viewers"], []string{policyViewer}):
		return "the viewers' group is not the placeholder"
	}
	return ""
}

func judgeGrants(p tailnetPolicy, tag string) string {
	if len(p.Grants) != 2 {
		return "not exactly two grants"
	}
	self, viewers := p.Grants[0], p.Grants[1]
	switch {
	case !slices.Equal(self.Src, []string{"autogroup:member"}) || !slices.Equal(self.Dst, []string{"autogroup:self"}):
		return "members do not keep their own devices"
	case !slices.Equal(viewers.Src, []string{"group:preview-viewers"}) || !slices.Equal(viewers.Dst, []string{tag}) ||
		!slices.Equal(viewers.IP, []string{"tcp:443"}):
		return "the viewers' grant is not tag on tcp:443 alone"
	}
	for _, g := range p.Grants {
		if slices.Contains(g.Src, "*") || slices.Contains(g.Dst, "*") {
			return "a grant reaches a wildcard"
		}
	}
	return ""
}

func judgeTests(p tailnetPolicy, tag string) string {
	test := func(src, accept, deny string) bool {
		return slices.ContainsFunc(p.Tests, func(c policyTest) bool {
			return c.Src == src && (accept == "" || slices.Contains(c.Accept, accept)) &&
				(deny == "" || slices.Contains(c.Deny, deny))
		})
	}
	switch {
	case !test(policyOwner, policyOwner+":22", ""):
		return "no test that the owner keeps their own devices"
	case !test(policyViewer, tag+":443", tag+":80"):
		return "no test that a viewer reaches 443 and not 80"
	case !test("tag:k8s", "", tag+":443"):
		return "no test that another tagged device is refused"
	}
	return ""
}

// TestTailnetPolicyIsSafe: the desktop's policy parses and is safe.
func TestTailnetPolicyIsSafe(t *testing.T) {
	a := tailscaleAnswers()
	p, err := parsePolicy(t, tailscalePolicy(a))
	if err != nil {
		t.Fatalf("the policy does not parse: %v\n%s", err, tailscalePolicy(a))
	}
	if why := judgePolicy(p, a.TailscaleTag()); why != "" {
		t.Errorf("%s:\n%s", why, tailscalePolicy(a))
	}
	next := byPath(planWith(t, a, Secrets{DexClient: sentinel, DNS: dnsSentinel}))[NextStepsFile]
	for _, want := range []string{tailscalePolicy(a), "Replace `" + policyOwner + "`", `"ssh"`, "autogroup:internet"} {
		if !strings.Contains(next, want) {
			t.Errorf("NEXT-STEPS.md lacks %q", want)
		}
	}
}

// TestTailnetPolicyProperties (seeded): for every tag the recipe accepts,
// the policy parses and is safe, and no grant opens the device on any
// port but 443.
func TestTailnetPolicyProperties(t *testing.T) {
	cfg := quickConfig(500)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		a := tailscaleAnswers()
		if r.Intn(4) > 0 {
			a.Tailscale.Tag = "tag:" + randLabel(r)
		}
		args[0] = reflect.ValueOf(a)
	}
	prop := func(a Answers) bool {
		if a.Validate() != nil {
			return true // a tag the recipe refuses never reaches the policy
		}
		p, err := parsePolicy(t, tailscalePolicy(a))
		if err != nil {
			t.Logf("tag %s: %v", a.TailscaleTag(), err)
			return false
		}
		if why := judgePolicy(p, a.TailscaleTag()); why != "" {
			t.Logf("tag %s: %s", a.TailscaleTag(), why)
			return false
		}
		for _, g := range p.Grants {
			if slices.Contains(g.Dst, a.TailscaleTag()) && !slices.Equal(g.IP, []string{"tcp:443"}) {
				t.Logf("tag %s: a grant opens %v", a.TailscaleTag(), g.IP)
				return false
			}
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestJudgePolicyRefusesAllowAll: the judge is no rubber stamp; an
// allow-all grant left in, or a viewers' grant widened to every port, is
// caught.
func TestJudgePolicyRefusesAllowAll(t *testing.T) {
	a := tailscaleAnswers()
	tag := a.TailscaleTag()
	for name, edit := range map[string]func(string) string{
		"allow-all left in": func(s string) string {
			return strings.Replace(s, `"grants": [`, `"grants": [{"src": ["*"], "dst": ["*"], "ip": ["*"]},`, 1)
		},
		"every port": func(s string) string { return strings.Replace(s, `"tcp:443"`, `"*"`, 1) },
		"no port 80 test": func(s string) string {
			return strings.Replace(s, `, "deny": ["`+tag+`:80"]`, ``, 1)
		},
	} {
		p, err := parsePolicy(t, edit(tailscalePolicy(a)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if judgePolicy(p, tag) == "" {
			t.Errorf("%s: judged safe", name)
		}
	}
}
