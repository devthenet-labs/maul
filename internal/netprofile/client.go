// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import (
	"fmt"
	"slices"
	"strings"
)

// The tunnel client presets (step 3): each names a package manager whose
// proxy and noise settings the job controllers set on a tunnel Job when
// the operator lists it (agent.networkProfiles.<p>.clients). The
// environment each preset sets is the jobs package's; this package owns
// only the names and the hosts each client usually needs.
const (
	ClientNPM   = "npm"  // npm, yarn classic and pnpm
	ClientYarn  = "yarn" // Yarn Berry (2+)
	ClientPip   = "pip"
	ClientUV    = "uv"
	ClientNuGet = "nuget" // dotnet restore
	ClientCargo = "cargo"
	ClientJVM   = "jvm" // Maven and Gradle, through JAVA_TOOL_OPTIONS
)

// Clients is every client preset, in the fixed order the job environment
// applies them.
var Clients = []string{ClientNPM, ClientYarn, ClientPip, ClientUV, ClientNuGet, ClientCargo, ClientJVM}

// ValidClient reports whether c is a client preset.
func ValidClient(c string) bool { return slices.Contains(Clients, c) }

// clientHosts are the public hosts each client fetches from by default,
// for the startup warning when a profile lists the client without them
// (ClientWarnings). Any one host of a client's list satisfies it, except
// where the client needs them all; allOf marks those.
var clientHosts = map[string]struct {
	hosts []string
	allOf bool
}{
	ClientNPM:   {hosts: []string{"registry.npmjs.org"}},
	ClientYarn:  {hosts: []string{"registry.yarnpkg.com", "registry.npmjs.org"}},
	ClientPip:   {hosts: []string{"pypi.org", "files.pythonhosted.org"}, allOf: true},
	ClientUV:    {hosts: []string{"pypi.org", "files.pythonhosted.org"}, allOf: true},
	ClientNuGet: {hosts: []string{"api.nuget.org"}},
	ClientCargo: {hosts: []string{"index.crates.io", "static.crates.io"}, allOf: true},
	ClientJVM:   {hosts: []string{"repo.maven.apache.org"}},
}

// clientEcosystem is the registry ecosystem whose grant sets the same
// package manager's settings as the client preset, so the two cannot share
// a profile: an npm grant points npm at the proxy's /r/ route, the npm
// preset at the tunnel, and only one can win.
var clientEcosystem = map[string]string{
	ClientNPM:  EcosystemNPM,
	ClientYarn: EcosystemNPM,
	ClientPip:  EcosystemPyPI,
	ClientUV:   EcosystemPyPI,
}

// ClientWarnings are the startup warnings for a valid profile: one per
// listed client whose usual public hosts are not in the profile's hosts.
// They are warnings, not refusals, because a private mirror the operator
// listed instead is legitimate.
func (p Profile) ClientWarnings() []string {
	var out []string
	for _, c := range p.Clients {
		want, ok := clientHosts[c]
		if !ok {
			continue
		}
		var missing []string
		for _, h := range want.hosts {
			if !p.Hosts.Allows(h) {
				missing = append(missing, h)
			}
		}
		if len(missing) == 0 || (!want.allOf && len(missing) < len(want.hosts)) {
			continue
		}
		out = append(out, fmt.Sprintf("profile %s: client %s usually fetches from %s, which the profile's hosts "+
			"do not list; add it or point %s at a mirror you listed", p.Name, c, strings.Join(missing, " and "), c))
	}
	return out
}
