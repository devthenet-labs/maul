// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"fmt"
	"net/netip"
	"strings"
)

// DNSRecords are the records that point the preview wildcard and the
// sign-in host at addrs, one per name and address, as a zone file writes
// them (A for IPv4, AAAA for IPv6). An address that does not parse, or an
// empty name, gives no record.
func DNSRecords(suffix, signIn string, addrs []string) []string {
	var out []string
	for _, name := range []string{"*." + suffix, signIn} {
		if name == "" || name == "*." {
			continue
		}
		for _, a := range addrs {
			ip, err := netip.ParseAddr(strings.TrimSpace(a))
			if err != nil {
				continue
			}
			typ := "A"
			if !ip.Unmap().Is4() {
				typ = "AAAA"
			}
			out = append(out, fmt.Sprintf("%s. %s %s", name, typ, ip.Unmap()))
		}
	}
	return out
}
