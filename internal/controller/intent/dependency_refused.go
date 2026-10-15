// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"slices"
	"strings"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
	"github.com/bitwise-media-group/patchy/internal/report"
)

// maxRefusedQuoted bounds the proxy lines a dependency_refused detail
// quotes, so the detail stays well inside the run's 4096-byte bound.
const maxRefusedQuoted = 3

// dependencyRefused reads a not-built build report for a refusal no further
// attempt gets past: the report's refused lines (the structured signal the
// build prompt asks for, never its free-text reason) holding at least one of
// the egress proxy's policy refusals. It needs the network profile the run's
// Job carried (recorded at launch from the Job itself): a run with none had
// no proxy to be refused by, so a line it quotes is not one. The detail
// names the profile, quotes the policy lines (each bounded by the report
// schema) and says what the operator changes; transient refusals among the
// lines are left out, and a report holding only those is an ordinary
// not_built, retried as before.
func dependencyRefused(b *report.Build, profile string) (string, bool) {
	if profile == "" || b == nil || len(b.Refused) == 0 {
		return "", false
	}
	var (
		quoted  []string
		hosts   []string
		pkg     bool
		never   bool
		matched int
	)
	for _, line := range b.Refused {
		ref, ok := netprofile.PolicyRefusal(line)
		if !ok {
			continue
		}
		matched++
		if len(quoted) < maxRefusedQuoted {
			quoted = append(quoted, "`"+strings.ReplaceAll(line, "`", "'")+"`")
		}
		switch ref.Kind {
		case netprofile.RefusalHost:
			if !slices.Contains(hosts, ref.Host) {
				hosts = append(hosts, ref.Host)
			}
		case netprofile.RefusalPackage:
			pkg = true
		case netprofile.RefusalNever:
			never = true
		}
	}
	if matched == 0 {
		return "", false
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "the egress proxy refused a dependency the build needs, under network profile %s, and refuses "+
		"it the same way on every attempt under that profile, so patchy did not retry it. The proxy said: %s",
		profile, strings.Join(quoted, "; "))
	if more := matched - len(quoted); more > 0 {
		fmt.Fprintf(&sb, " (and %d more)", more)
	}
	var actions []string
	if len(hosts) > 0 {
		actions = append(actions, fmt.Sprintf("add %s to network profile %s's hosts", strings.Join(hosts, ", "),
			profile))
	}
	if pkg {
		actions = append(actions, fmt.Sprintf("allow the module or package in network profile %s's modules or its "+
			"registry grant (or in the registry's own policy)", profile))
	}
	if never {
		actions = append(actions, "give the plan another source for what the proxy never opens (a forge, model "+
			"API or control-plane host, an IP address, a port other than 443, plain http), since no profile admits it")
	}
	fmt.Fprintf(&sb, ". To build it, %s; then apply the trigger label again for a new plan, or, on a review round, "+
		"ask for the change again", strings.Join(actions, "; and "))
	return sb.String(), true
}
