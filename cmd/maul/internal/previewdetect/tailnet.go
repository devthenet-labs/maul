// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewdetect

import (
	"slices"
	"strings"

	"github.com/devthenet-labs/maul/cmd/maul/internal/previewsetup"
	"github.com/devthenet-labs/maul/cmd/maul/internal/tailnet"
)

// ApplyTailnet lays the tailscale recipe's device, as the caller's
// Tailscale client found it (m, from tailnet.Find for a's hostname and
// tag), over a: --tailscale-address is filled only while unanswered and
// not given, as Apply fills. It returns what it reports (the device, the
// exact DNS records, a recorded address that disagrees) and what it
// filled. Pure; idempotent.
func ApplyTailnet(m tailnet.Match, a *previewsetup.Answers, given Given) (report, filled []string) {
	if a.Edge != previewsetup.EdgeTailscale {
		return nil, nil
	}
	if !m.Found {
		return []string{"tailnet device: not found: " + m.Why + "; once it is, `maul check previews` prints " +
			"the DNS records to create"}, nil
	}
	d := m.Device
	name := d.DNSName
	if name == "" {
		name = d.HostName
	}
	report = append(report, "tailnet device: "+name+" ("+strings.Join(d.Tags, ", ")+"): "+strings.Join(d.IPv4, ", "))
	switch {
	case len(a.Tailscale.Addresses) == 0 && !given("tailscale-address"):
		a.Tailscale.Addresses = slices.Clone(d.IPv4)
		filled = append(filled, "--tailscale-address "+strings.Join(d.IPv4, ",")+" (the tailnet device "+name+")")
	case !slices.Equal(a.Tailscale.Addresses, d.IPv4):
		report = append(report, "warning: --tailscale-address is "+strings.Join(a.Tailscale.Addresses, ", ")+
			", but the tailnet device "+name+" is at "+strings.Join(d.IPv4, ", ")+": give --tailscale-address "+
			"again if it was recreated")
	}
	for _, r := range previewsetup.DNSRecords(a.HostSuffix, a.SignInHost, d.IPv4) {
		report = append(report, "DNS record to create: "+r)
	}
	return report, filled
}
