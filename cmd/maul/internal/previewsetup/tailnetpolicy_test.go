// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
// comments dropped, then strict JSON (no field the recipe does not know,
// and no object key twice, which Tailscale's parser refuses where
// encoding/json would silently keep the last).
func parsePolicy(t *testing.T, hujson string) (tailnetPolicy, error) {
	t.Helper()
	var lines []string
	for l := range strings.SplitSeq(hujson, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "//") {
			lines = append(lines, l)
		}
	}
	body := strings.Join(lines, "\n")
	if err := noDuplicateKeys(body); err != nil {
		return tailnetPolicy{}, err
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var p tailnetPolicy
	err := dec.Decode(&p)
	return p, err
}

// noDuplicateKeys walks the JSON's tokens and refuses an object that holds
// a key twice.
func noDuplicateKeys(body string) error {
	dec := json.NewDecoder(strings.NewReader(body))
	type frame struct {
		object bool
		keys   map[string]bool
		key    bool // the next string token in this object is a key
	}
	var stack []*frame
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		var top *frame
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		if k, ok := tok.(string); ok && top != nil && top.object && top.key {
			if top.keys[k] {
				return fmt.Errorf("duplicate key %q", k)
			}
			top.keys[k] = true
			top.key = false
			continue
		}
		switch tok {
		case json.Delim('{'):
			stack = append(stack, &frame{object: true, keys: map[string]bool{}, key: true})
			continue
		case json.Delim('['):
			stack = append(stack, &frame{})
			continue
		case json.Delim('}'), json.Delim(']'):
			stack = stack[:len(stack)-1]
		}
		// A value ended (scalar or closed container): the enclosing
		// object's next token is a key again.
		if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].key = true
		}
	}
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
		!slices.Equal(p.TagOwners[TailscaleProxyTag], []string{TailscaleOperatorTag}):
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
	case !test(TailscaleProxyTag, "", tag+":443"):
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
		switch r.Intn(8) {
		case 0, 1:
		case 2:
			a.Tailscale.Tag = TailscaleProxyTag // the operator's own proxy tag, an obvious choice
		case 3:
			a.Tailscale.Tag = TailscaleOperatorTag
		default:
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

// TestTailnetPolicyRefusesProxyTag pins the counterexample review found:
// --tailscale-tag tag:k8s (the tag the operator gives every proxy) would
// write tagOwners with "tag:k8s" twice, which Tailscale refuses, grant
// viewers every proxy the operator creates, and make the "another tagged
// device" test a self-reference. The recipe refuses the tag, and the
// policy it would have written does not parse.
func TestTailnetPolicyRefusesProxyTag(t *testing.T) {
	a := tailscaleAnswers()
	a.Tailscale.Tag = TailscaleProxyTag
	if err := a.Validate(); err == nil || !strings.Contains(err.Error(), "every proxy it creates") {
		t.Errorf("Validate with --tailscale-tag %s = %v", TailscaleProxyTag, err)
	}
	if _, err := parsePolicy(t, tailscalePolicy(a)); err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Errorf("the policy for %s parsed: %v", TailscaleProxyTag, err)
	}
}

// TestNoDuplicateKeys: the strict parser's duplicate check sees a repeat
// at any depth and nothing else.
func TestNoDuplicateKeys(t *testing.T) {
	for in, dup := range map[string]bool{
		`{"a": 1, "b": {"a": [1, {"a": 2}]}}`: false,
		`{"a": 1, "a": 2}`:                    true,
		`{"x": {"k": [], "k": []}}`:           true,
		`[{"a": 1}, {"a": 1}]`:                false,
		`{"a": "a", "b": "b"}`:                false,
		`{"a": {}, "b": {}}`:                  false,
		`{"a": {}, "a": {}}`:                  true,
	} {
		if err := noDuplicateKeys(in); (err != nil) != dup {
			t.Errorf("noDuplicateKeys(%s) = %v, want duplicate %v", in, err, dup)
		}
	}
}
