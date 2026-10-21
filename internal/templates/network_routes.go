// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"slices"
	"strings"

	"github.com/devthenet-labs/maul/internal/netprofile"
)

// NetworkRoutes are the dependency routes an operator network profile
// opens beyond its name: the public Go module route and its registry
// grants (step 2). With no grants a profile is step 1's, and every text
// that names it is exactly what it was before grants existed; with grants,
// the prompts, the plan's dependency advice and the pull request bodies
// say which ecosystems the run could fetch, through which registry and
// under which policy. With tunnel hosts (step 3) they also say which hosts
// packages can be downloaded from over HTTPS, and a profile whose route is
// the tunnel alone is never described as a Go route.
type NetworkRoutes struct {
	// PublicGo is the public Go module route (route: go). In the pod, which
	// learns only the ecosystems, it is set for a listed go with no go
	// grant; the prompt reads Go the same either way.
	PublicGo bool `json:"publicGo,omitempty"`
	// Grants are the profile's registry grants, at most one per ecosystem.
	Grants []netprofile.PodGrant `json:"grants,omitempty"`
	// TunnelHosts are the profile's tunnel hosts, sorted: the exact public
	// names the egress proxy opens an HTTPS tunnel to. None means no
	// tunnel.
	TunnelHosts []string `json:"tunnelHosts,omitempty"`
}

// RoutesOf is a profile's routes as the job controllers hold the profile.
func RoutesOf(p netprofile.Profile) NetworkRoutes {
	r := NetworkRoutes{PublicGo: p.Route == netprofile.RouteGo}
	for _, g := range p.Registries {
		r.Grants = append(r.Grants, netprofile.PodGrantOf(g))
	}
	if len(p.Hosts) > 0 {
		r.TunnelHosts = slices.Sorted(slices.Values(p.Hosts))
	}
	return r
}

// BeyondGo reports routes that are more than step 1's Go route alone:
// registry grants or tunnel hosts. Every text that names a profile is
// step 1's, word for word, exactly when it is false.
func (r NetworkRoutes) BeyondGo() bool { return len(r.Grants) > 0 || len(r.TunnelHosts) > 0 }

// WithTunnel is the routes as a pod reads them once it has a tunnel
// (netprofile.ParsePodTunnel): the tunnel's hosts and, for a pod told no
// ecosystems (a profile without grants), the Go route exactly when the
// routes list it. A pod with grants already learned Go from its
// ecosystems. No hosts leaves the routes as they were.
func (r NetworkRoutes) WithTunnel(t netprofile.PodTunnel, ecosystems []string) NetworkRoutes {
	if len(t.Hosts) == 0 {
		return r
	}
	r.TunnelHosts = slices.Clone(t.Hosts)
	if len(ecosystems) == 0 {
		r.PublicGo = t.GoRoute
	}
	return r
}

// PodRoutes is the routes as a pod reads them: its ecosystems and grants.
func PodRoutes(ecosystems []string, grants []netprofile.PodGrant) NetworkRoutes {
	r := NetworkRoutes{Grants: grants}
	if _, ok := r.grant(netprofile.EcosystemGo); !ok {
		for _, e := range ecosystems {
			r.PublicGo = r.PublicGo || e == netprofile.EcosystemGo
		}
	}
	return r
}

// grant is the routes' grant of the ecosystem.
func (r NetworkRoutes) grant(ecosystem string) (netprofile.PodGrant, bool) {
	for _, g := range r.Grants {
		if g.Ecosystem == ecosystem {
			return g, true
		}
	}
	return netprofile.PodGrant{}, false
}

// hasGo reports a Go route of either kind.
func (r NetworkRoutes) hasGo() bool {
	_, ok := r.grant(netprofile.EcosystemGo)
	return r.PublicGo || ok
}

// ecosystemNames are the ecosystems' display names.
var ecosystemNames = map[string]string{
	netprofile.EcosystemGo: "Go", netprofile.EcosystemNPM: "npm", netprofile.EcosystemPyPI: "PyPI",
}

// list joins items as prose: "a", "a and b", "a, b and c".
func list(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// ecosystems names the routes' ecosystems in the fixed order.
func (r NetworkRoutes) ecosystems() string {
	var names []string
	for _, e := range netprofile.Ecosystems {
		_, granted := r.grant(e)
		if granted || e == netprofile.EcosystemGo && r.PublicGo {
			names = append(names, ecosystemNames[e])
		}
	}
	return list(names)
}

// hostList is the tunnel's hosts as inline code, in prose. Each is a
// validated host name, so code() shows it as it is.
func (r NetworkRoutes) hostList() string {
	items := make([]string, 0, len(r.TunnelHosts))
	for _, h := range r.TunnelHosts {
		items = append(items, code(h))
	}
	return list(items)
}

// packageList is a grant's allowlist as inline code, with the count of the
// entries the pod was not told by name.
func packageList(g netprofile.PodGrant) string {
	items := make([]string, 0, len(g.Packages))
	for _, p := range g.Packages {
		items = append(items, code(p))
	}
	out := strings.Join(items, ", ")
	if g.MorePackages > 0 {
		out += fmt.Sprintf(" and %d more", g.MorePackages)
	}
	return out
}

// promptWidth is the column the prompt templates wrap at.
const promptWidth = 120

// wrap fills words into lines of at most promptWidth columns, the first
// starting at first and every other at indent, as the prompt templates are
// hand-wrapped. A word longer than a line stands alone.
func wrap(text, first, indent string) string {
	var b strings.Builder
	line := first
	empty := true
	for _, w := range strings.Fields(text) {
		if !empty && len(line)+1+len(w) > promptWidth {
			b.WriteString(line + "\n")
			line, empty = indent, true
		}
		if !empty {
			line += " "
		}
		line += w
		empty = false
	}
	b.WriteString(line)
	return b.String()
}

// networkRoutesPrompt is the paragraph a remediate or build prompt carries
// for a profile with registry grants or tunnel hosts, in place of step 1's
// Go paragraph: an opening sentence naming the route and its ecosystems,
// one bullet per ecosystem and one for the tunnel's hosts, and the shared
// closing. needs names what a refused dependency blocks ("plan" or "fix").
// It starts a line of its own, indented as the prompts' bullet
// continuations are.
func networkRoutesPrompt(profile string, r NetworkRoutes, needs string) string {
	opening := "Packages can be fetched through the operator's dependency route " + code(profile)
	switch eco := r.ecosystems(); {
	case eco != "" && len(r.TunnelHosts) > 0:
		opening += " for: " + eco + ", and downloaded over HTTPS from the hosts below."
	case eco != "":
		opening += " for: " + eco + "."
	default:
		opening = "Packages can be downloaded over HTTPS from the hosts below, through the operator's " +
			"dependency route " + code(profile) + "."
	}
	parts := []string{wrap(opening, "  ", "  ")}
	if r.hasGo() {
		parts = append(parts, wrap(goRoute(r), "  - ", "    "))
	}
	if g, ok := r.grant(netprofile.EcosystemNPM); ok {
		parts = append(parts, wrap(npmRoute(g), "  - ", "    "))
	}
	if g, ok := r.grant(netprofile.EcosystemPyPI); ok {
		parts = append(parts, wrap(pipRoute(g), "  - ", "    "))
	}
	if len(r.TunnelHosts) > 0 {
		parts = append(parts, wrap(tunnelRoute(r, needs), "  - ", "    "))
	}
	closing := "Anything else on the network is unreachable. A refused package answers `403`"
	if r.hasGo() {
		closing += " (`410` for Go)"
	}
	closing += " with a reason; report it rather than working around it. A reviewer will check every new dependency."
	if len(r.TunnelHosts) > 0 {
		// The tunnel bullet already says what to report when a dependency
		// is refused; repeating it here would read as a second instruction.
		closing += " Stop once any route refuses a dependency the " + needs + " needs."
	} else {
		closing += " If a dependency the " + needs + " needs is refused, stop and report it."
	}
	parts = append(parts, wrap(closing, "  ", "  "))
	return strings.Join(parts, "\n")
}

// refusedGuide is the build prompt's paragraph on the report's refused
// lines, for routes beyond step 1's Go route alone (BeyondGo): there a
// refusal reaches the agent as the proxy's own line (a tunnel's status line,
// a registry route's body). On step 1's Go route the go command reports a
// refusal as a missing file, never with the proxy's line, so that prompt
// says nothing of refused and stays step 1's word for word. The example is
// a tunnel's refusal when the routes have hosts, else a registry grant's.
func refusedGuide(r NetworkRoutes) string {
	example := "`403 egress-proxy: <host> is not in profile <profile>'s hosts`"
	if len(r.TunnelHosts) == 0 {
		example = "`403 egress-proxy: package <name> is not allowed by the grant of registry <registry>`"
	}
	return wrap("`refused` is only for a build that could not be done because the operator's egress proxy "+
		"refused a dependency the plan needs: one `- \"...\"` item per refusal, at most 5, each the proxy's "+
		"exact line with `egress-proxy:` in it (for example "+example+"). maul does not retry a build the "+
		"proxy refused by its policy, and shows these lines to the operator, who can change it. Leave "+
		"`refused` out for any other failure, for a refusal that says to retry later, for a refusal you got "+
		"past, and for one you only inferred without the proxy's line, which belongs in `reason`.", "", "")
}

// goRoute is the Go bullet: step 1's Go text, and with a go registry where
// modules come from first, its policy and its private modules.
func goRoute(r NetworkRoutes) string {
	text := "**Go**: `go get`, `go mod tidy` and `go mod download` work for tagged releases of module paths " +
		"the operator allows, and modules already in this image still build. A module outside the operator's " +
		"allowlist, or a version that is not a tagged release, fails as not found or refused, often as a " +
		"missing file under the image's module cache rather than with a reason: report it, do not work around " +
		"it. Name explicit tagged versions (`go get <module>@vX.Y.Z`) or `@latest`; branch and commit versions " +
		"are refused."
	if g, ok := r.grant(netprofile.EcosystemGo); ok {
		text += " Modules come from the operator's registry " + code(g.Registry) + " first"
		if g.AnyPackage {
			text += ", which decides which modules exist"
		} else {
			text += ", for modules under " + packageList(g)
		}
		text += "."
		if g.AllowPseudoVersions {
			text += " Through it, branch and commit (pseudo-) versions of those modules can be fetched too."
		}
		if len(g.PrivateModules) > 0 {
			private := make([]string, 0, len(g.PrivateModules))
			for _, m := range g.PrivateModules {
				private = append(private, code(m))
			}
			text += " Private modules under " + list(private) + " are not checked against the public " +
				"checksum database."
		}
		text += " A `410` naming `checksum_not_public` means the operator has not said whether that module " +
			"may be checked publicly; report it."
	}
	return text + " Do not change `GOPROXY`, `GOSUMDB`, `GONOSUMDB`, `GOPRIVATE`, `GOFLAGS` or `GOTOOLCHAIN`, " +
		"and never disable checksum verification. Commit `go.mod` and `go.sum` with the change."
}

// npmRoute is the npm bullet.
func npmRoute(g netprofile.PodGrant) string {
	text := "**npm**: `npm install <pkg>@<version>` and `npm ci` work through the registry " + code(g.Registry) +
		" the operator configured; "
	if g.AnyPackage {
		text += "which packages exist is the registry's decision."
	} else {
		text += "packages the operator allows: " + packageList(g) + "."
	}
	if g.AllowPrerelease {
		text += " Prerelease versions can be fetched."
	} else {
		text += " Only release versions (x.y.z) can be fetched."
	}
	if g.Scripts {
		text += " Install scripts run, with no network: a native build that tries to download headers or " +
			"prebuilt binaries will fail; report it."
	} else {
		text += " Install scripts do not run."
	}
	return text + " A `403` saying the registry refused a package is the registry's own policy. " +
		"`package-lock.json` keeps the registry's own URLs; commit it with `package.json`. Do not change the npm " +
		"registry settings, add an `.npmrc`, or use yarn or pnpm; scoped registries in `.npmrc` are not reachable."
}

// pipRoute is the pip bullet.
func pipRoute(g netprofile.PodGrant) string {
	text := "**pip**: `pip install` and `pip download` work through the operator's index " + code(g.Registry) +
		" for "
	if g.AnyPackage {
		text += "the registry's packages"
	} else {
		text += "the projects the operator allows: " + packageList(g) + ";"
	}
	if g.Sdists {
		text += " wheels and source builds."
		if !g.AnyPackage {
			text += " With source builds, the build backend (setuptools, hatchling, …) must itself be allowed."
		}
	} else {
		text += " wheels only (no source builds)."
	}
	return text + " Install into a virtualenv under `/tmp` or the work tree. Do not pass `--index-url`, " +
		"`--extra-index-url` or `--trusted-host`, and do not write them into requirements files; with " +
		"`pip-compile`, pass `--no-emit-index-url`. Pin with hashes where the repository does."
}

// tunnelRoute is the tunnel's bullet (§4.6 of the step-3 spec): the hosts,
// what is refused, what not to change, and how to read the proxy's verdict
// when a package manager does not print it, so the host and the proxy's
// line reach the run's report, the record that outlives the pod. The probe
// is fixed text; only the validated hosts vary.
func tunnelRoute(r NetworkRoutes, needs string) string {
	return "**HTTPS downloads**: packages can be downloaded over HTTPS from these hosts only, through the " +
		"operator's proxy, which is already configured in this environment: " + r.hostList() + ". Any other " +
		"host, plain http, and git dependencies are refused (`403 egress-proxy: …`). Do not change " +
		"`HTTPS_PROXY`, `NO_PROXY` or a package manager's proxy or registry settings, and never disable TLS " +
		"verification. Never publish or upload a package. Commit the lockfile with the change. If a download " +
		"fails, find out whether the proxy refused it: run " +
		"`curl -sSv -x \"$HTTPS_PROXY\" https://<host>/ 2>&1 | grep -i egress-proxy` (or, without curl, " +
		"`python3 -c` with `http.client.HTTPSConnection(proxy_host, proxy_port).set_tunnel(\"<host>\", 443)`), " +
		"because some package managers do not print the proxy's reason. If a dependency the " + needs + " needs " +
		"is refused, put the host and the proxy's exact line in your final message and in your report."
}

// routeSummaries describe each route in a phrase, for the plan advice:
// each grant with its registry and policy, and the public Go route.
func (r NetworkRoutes) routeSummaries() []string {
	phrase := func(g netprofile.PodGrant, kind string) string {
		if g.AnyPackage {
			return "any " + kind + " " + code(g.Registry) + " serves"
		}
		return "the " + kind + "s the operator allows from " + code(g.Registry)
	}
	var out []string
	if g, ok := r.grant(netprofile.EcosystemGo); ok {
		out = append(out, phrase(g, "Go module"))
	}
	if r.PublicGo {
		out = append(out, "tagged releases of public Go module paths the operator allows")
	}
	if g, ok := r.grant(netprofile.EcosystemNPM); ok {
		out = append(out, phrase(g, "npm package"))
	}
	if g, ok := r.grant(netprofile.EcosystemPyPI); ok {
		out = append(out, phrase(g, "PyPI project"))
	}
	if len(r.TunnelHosts) > 0 {
		out = append(out, "any package downloaded over HTTPS from "+r.hostList())
	}
	return out
}

// routeList is the pull request line's parenthesis: each ecosystem, with
// the registry it came through ("Go, npm via `ca-npm`").
func (r NetworkRoutes) routeList() string {
	var out []string
	if g, ok := r.grant(netprofile.EcosystemGo); ok {
		item := "Go via " + code(g.Registry)
		if r.PublicGo {
			item += " and the public route"
		}
		out = append(out, item)
	} else if r.PublicGo {
		out = append(out, "Go")
	}
	for _, e := range []string{netprofile.EcosystemNPM, netprofile.EcosystemPyPI} {
		if g, ok := r.grant(e); ok {
			out = append(out, ecosystemNames[e]+" via "+code(g.Registry))
		}
	}
	if len(r.TunnelHosts) > 0 {
		out = append(out, "HTTPS downloads from "+r.hostList())
	}
	return strings.Join(out, ", ")
}
