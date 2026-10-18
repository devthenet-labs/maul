// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"slices"
	"testing"
)

// TestDNSOptionSpecsAgreeWithValidate: the specs the wizard prompts from
// are the solver's own options: the same keys in the same order, a
// required one's absence refused by Validate, and Valid agreeing with
// Validate on each value.
func TestDNSOptionSpecsAgreeWithValidate(t *testing.T) {
	base := func(dns string, opts map[string]string) Answers {
		a := Defaults()
		a.Platform, a.Edge = PlatformK3s, EdgeGatewayTLS
		a.HostSuffix, a.SignInHost = "acme-previews.dev", "preview-auth.patchy.acme.dev"
		a.Dex.Issuer, a.Viewers.Teams, a.Registry = "https://dex.patchy.acme.dev", []string{"acme:qa"}, "ghcr.io"
		a.Certificate.DNS, a.Certificate.DNSOptions = dns, opts
		return a
	}
	valid := map[string]map[string]string{
		"route53": {"region": "us-east-1"}, "google": {"project": "acme-dns-1"},
		"azure":   {"subscriptionID": "6ff42ae2-765d-4adf-8112-31c55c1551ef", "resourceGroupName": "dns"},
		"rfc2136": {"nameserver": "10.0.0.53:53", "tsigKeyName": "acme-key"},
	}
	for _, dns := range DNSProviders() {
		specs := DNSOptionSpecs(dns)
		keys := make([]string, 0, len(specs))
		for _, s := range specs {
			keys = append(keys, s.Key)
		}
		if !slices.Equal(keys, DNSOptionKeys(dns)) {
			t.Errorf("%s: spec keys %v, option keys %v", dns, keys, DNSOptionKeys(dns))
		}
		for _, s := range specs {
			if !s.Required {
				continue
			}
			if v := valid[dns][s.Key]; v == "" || !s.Valid(v) {
				t.Errorf("%s %s: no valid example %q", dns, s.Key, v)
			}
			opts := map[string]string{}
			for k, v := range valid[dns] {
				if k != s.Key {
					opts[k] = v
				}
			}
			if base(dns, opts).Validate() == nil {
				t.Errorf("%s: Validate accepts no %s, which the spec says is required", dns, s.Key)
			}
			opts[s.Key] = "!not valid!"
			if s.Valid("!not valid!") || base(dns, opts).Validate() == nil {
				t.Errorf("%s %s: an invalid value is accepted", dns, s.Key)
			}
		}
		if dns != DNSManual {
			if err := base(dns, valid[dns]).Validate(); err != nil {
				t.Errorf("%s with its required options: %v", dns, err)
			}
		}
	}
}
