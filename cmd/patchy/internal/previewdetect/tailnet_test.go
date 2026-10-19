// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewdetect

import (
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/quick"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/tailnet"
)

func tailnetAnswers() previewsetup.Answers {
	a := previewsetup.Defaults()
	a.Edge, a.HostSuffix, a.SignInHost = previewsetup.EdgeTailscale, "acme-previews.dev", "preview-auth.acme.dev"
	return a
}

var found = tailnet.Match{Found: true, Device: tailnet.Device{Name: "patchy-previews",
	DNSName: "patchy-previews.tail1234.ts.net", IPv4: []string{"100.101.12.58"}, Tags: []string{"tag:patchy-previews"}}}

func TestApplyTailnet(t *testing.T) {
	a := tailnetAnswers()
	report, filled := ApplyTailnet(found, &a, func(string) bool { return false })
	if !slices.Equal(a.Tailscale.Addresses, []string{"100.101.12.58"}) || len(filled) != 1 ||
		!strings.HasPrefix(filled[0], "--tailscale-address 100.101.12.58 (the tailnet device") {
		t.Errorf("filled %q, addresses %v", filled, a.Tailscale.Addresses)
	}
	want := []string{"DNS record to create: *.acme-previews.dev. A 100.101.12.58",
		"DNS record to create: preview-auth.acme.dev. A 100.101.12.58"}
	for _, w := range want {
		if !slices.Contains(report, w) {
			t.Errorf("report lacks %q: %q", w, report)
		}
	}

	// A flag given wins; a recorded address that disagrees is reported, never replaced.
	b := tailnetAnswers()
	b.Tailscale.Addresses = []string{"100.64.0.9"}
	report, filled = ApplyTailnet(found, &b, func(f string) bool { return f == "tailscale-address" })
	if len(filled) != 0 || !slices.Equal(b.Tailscale.Addresses, []string{"100.64.0.9"}) ||
		!slices.ContainsFunc(report, func(l string) bool { return strings.HasPrefix(l, "warning: --tailscale-address") }) {
		t.Errorf("a given address: filled %q, report %q", filled, report)
	}

	c := tailnetAnswers()
	report, filled = ApplyTailnet(tailnet.Match{Why: "no device named x"}, &c, func(string) bool { return false })
	if len(filled) != 0 || len(c.Tailscale.Addresses) != 0 || len(report) != 1 ||
		!strings.Contains(report[0], "no device named x") {
		t.Errorf("not found: filled %q, report %q", filled, report)
	}

	other := tailnetAnswers()
	other.Edge = previewsetup.EdgeGatewayTLS
	if report, filled := ApplyTailnet(found, &other, func(string) bool { return false }); report != nil ||
		filled != nil || other.Tailscale.Addresses != nil {
		t.Error("another edge took the tailnet device")
	}
}

// TestApplyTailnetProperties (seeded): ApplyTailnet fills only an
// unanswered, ungiven --tailscale-address, with exactly the device's
// addresses, never touches another answer, and is idempotent.
func TestApplyTailnetProperties(t *testing.T) {
	cfg := &quick.Config{MaxCount: 1000, Rand: rand.New(rand.NewSource(20261018))}
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		a := tailnetAnswers()
		if r.Intn(3) == 0 {
			a.Edge = previewsetup.EdgeExisting
		}
		if r.Intn(2) == 0 {
			a.Tailscale.Addresses = []string{"100.64.0.9"}
		}
		m := found
		if r.Intn(3) == 0 {
			m = tailnet.Match{Why: "not there"}
		}
		args[0] = reflect.ValueOf(m)
		args[1] = reflect.ValueOf(a)
		args[2] = reflect.ValueOf(r.Intn(2) == 0)
	}
	prop := func(m tailnet.Match, a previewsetup.Answers, flagGiven bool) bool {
		given := func(f string) bool { return flagGiven && f == "tailscale-address" }
		before := a
		before.Tailscale.Addresses = slices.Clone(a.Tailscale.Addresses)
		_, filled := ApplyTailnet(m, &a, given)
		open := len(before.Tailscale.Addresses) == 0 && !flagGiven && m.Found &&
			before.Edge == previewsetup.EdgeTailscale
		after := a
		after.Tailscale.Addresses = before.Tailscale.Addresses
		switch {
		case !reflect.DeepEqual(after, before):
			t.Logf("another answer changed")
			return false
		case open != (len(filled) == 1):
			t.Logf("open %v, filled %q", open, filled)
			return false
		case open && !slices.Equal(a.Tailscale.Addresses, m.Device.IPv4):
			t.Logf("filled %v, device %v", a.Tailscale.Addresses, m.Device.IPv4)
			return false
		case !open && !slices.Equal(a.Tailscale.Addresses, before.Tailscale.Addresses):
			t.Logf("an answered address changed")
			return false
		}
		again := a
		if _, f := ApplyTailnet(m, &again, given); len(f) != 0 {
			t.Logf("not idempotent: %q", f)
			return false
		}
		return true
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}
