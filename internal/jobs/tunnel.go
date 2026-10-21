// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/devthenet-labs/maul/internal/netprofile"
)

// noProxyFixed are the NO_PROXY entries every tunnel Job carries beside its
// endpoints: loopback, and the cluster's own suffixes, so nothing a pod
// dials inside the cluster ever reaches the egress proxy as a CONNECT.
var noProxyFixed = []string{"localhost", "127.0.0.1", "::1", ".svc", ".cluster.local"}

// tunnelRefusals are why a profile's tunnel does not hold together: a
// tunnel route without hosts or with a baked module cache, hosts on a
// profile with no route, too many hosts, a host outside the grammar, on the
// tunnel deny table, listed twice, an upstream of the profile's own Go
// route or a host of one of its registry grants, and client presets that
// are unknown, repeated, without hosts, or beside a grant that configures
// the same package manager. netprofile.Validate refuses all of these in
// the operator's profile set already; the Job refuses them again so a
// controller can never hand one through.
func tunnelRefusals(p *NetworkProfile) []error {
	var errs []error
	refuse := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	switch {
	case p.Route == netprofile.RouteTunnel && len(p.Hosts) == 0:
		refuse("a tunnel profile must list hosts; it would open nothing")
	case p.Route == "" && len(p.Hosts) > 0:
		refuse("hosts need route %q or %q", netprofile.RouteTunnel, netprofile.RouteGo)
	}
	if p.Route == netprofile.RouteTunnel && p.BakedModCache != "" {
		refuse("a tunnel profile has no Go route to serve the baked module cache %q after", p.BakedModCache)
	}
	if len(p.Hosts) > netprofile.MaxTunnelHosts {
		refuse("%d tunnel hosts; at most %d", len(p.Hosts), netprofile.MaxTunnelHosts)
	}
	errs = append(errs, tunnelHostRefusals(p)...)
	return append(errs, clientRefusals(p)...)
}

// tunnelHostRefusals are why one of the profile's hosts may not be
// tunnelled to.
func tunnelHostRefusals(p *NetworkProfile) []error {
	var errs []error
	refuse := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	seen := map[string]bool{}
	for _, h := range p.Hosts {
		if !netprofile.ValidHost(h) {
			refuse("tunnel host %q is not an exact lowercase host name", h)
			continue
		}
		if seen[h] {
			refuse("tunnel host %s is listed twice", h)
		}
		seen[h] = true
		if err := netprofile.DeniedTunnelHost(h, netprofile.DenyContext{}); err != nil {
			refuse("tunnel host %s: %v", h, err)
		}
		if p.Route == netprofile.RouteGo && slices.Contains(netprofile.GoRouteHosts, h) {
			refuse("tunnel host %s is an upstream of the profile's Go route", h)
		}
		if g, ok := grantWithHost(p, h); ok {
			refuse("tunnel host %s is a host of the %s grant; the tunnel would bypass its package policy", h, g)
		}
	}
	return errs
}

// clientRefusals are why the profile's client presets may not be applied:
// unknown, repeated, without hosts, or beside a grant that sets the same
// package manager's index, which the preset must never override.
func clientRefusals(p *NetworkProfile) []error {
	var errs []error
	refuse := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if len(p.Clients) > 0 && len(p.Hosts) == 0 {
		refuse("client presets configure the tunnel; a profile without hosts takes none")
	}
	seen := map[string]bool{}
	for _, c := range p.Clients {
		if !netprofile.ValidClient(c) {
			refuse("client %q is not one of %s", c, strings.Join(netprofile.Clients, ", "))
			continue
		}
		if seen[c] {
			refuse("client %s is listed twice", c)
		}
		seen[c] = true
		if eco, ok := netprofile.ClientEcosystem(c); ok {
			if g, ok := grantFor(p, eco); ok {
				refuse("client %s and the %s grant %s both configure the same package manager", c, eco, g.Name)
			}
		}
	}
	return errs
}

// grantWithHost is the name of the profile's grant that h is a host of,
// exactly or under one of its *.suffix patterns.
func grantWithHost(p *NetworkProfile, h string) (string, bool) {
	for _, g := range p.Registries {
		for _, gh := range g.Hosts {
			if suffix, pattern := strings.CutPrefix(gh, "*."); pattern && strings.HasSuffix(h, "."+suffix) ||
				!pattern && h == gh {
				return g.Name, true
			}
		}
	}
	return "", false
}

// TunnelEnv is the tunnel half of a profile Job's agent environment (§4.2
// of the step-3 spec), or nothing for a profile without hosts: the proxy
// for https in both cases, NO_PROXY in both cases, the profile's routes and
// hosts for agent-runner's prompt, then each listed client's preset in the
// fixed netprofile.Clients order. endpointURLs are every URL the pod dials
// inside the cluster (the artifact server, the broker routes, the egress
// proxy itself); each URL's host, IP literals included, goes in NO_PROXY
// with loopback and the cluster suffixes, so the model traffic and the
// artifact fetch never reach the proxy. The plain-http and ALL_PROXY
// names are not set: injectEnv's blanks stay, so plain http goes direct
// and the NetworkPolicy drops it.
//
// Every value is non-empty and every name appears once. It is pure, and
// exported so the live client fixtures run the environment that ships.
func TunnelEnv(p NetworkProfile, endpointURLs []string) ([]corev1.EnvVar, error) {
	if len(p.Hosts) == 0 {
		return nil, nil
	}
	if err := validProxyURL(p.ProxyURL); err != nil {
		return nil, fmt.Errorf("jobs: tunnel env: %w", err)
	}
	proxy, _ := url.Parse(p.ProxyURL)
	hosts := slices.Sorted(slices.Values(p.Hosts))
	for _, h := range hosts {
		if !netprofile.ValidHost(h) {
			return nil, fmt.Errorf("jobs: tunnel env: host %q is not an exact lowercase host name", h)
		}
	}
	endpoints, err := noProxyHosts(endpointURLs)
	if err != nil {
		return nil, err
	}
	for _, e := range endpoints {
		if _, found := slices.BinarySearch(hosts, e); found {
			// The endpoint would have to go through the tunnel, which
			// carries only public hosts: refuse rather than drop it.
			return nil, fmt.Errorf("jobs: tunnel env: endpoint host %s is also a tunnel host", e)
		}
	}
	noProxy := strings.Join(sortedUnique(slices.Concat(endpoints, noProxyFixed)), ",")
	routes := netprofile.RouteTunnel
	if p.Route == netprofile.RouteGo {
		routes = netprofile.RouteGo + "," + netprofile.RouteTunnel
	}
	env := []corev1.EnvVar{
		{Name: "HTTPS_PROXY", Value: p.ProxyURL},
		{Name: "https_proxy", Value: p.ProxyURL},
		{Name: "NO_PROXY", Value: noProxy},
		{Name: "no_proxy", Value: noProxy},
		{Name: netprofile.EnvRoutes, Value: routes},
		{Name: netprofile.EnvHosts, Value: strings.Join(hosts, ",")},
	}
	for _, c := range p.Clients {
		if !netprofile.ValidClient(c) {
			return nil, fmt.Errorf("jobs: tunnel env: client %q is not one of %s", c,
				strings.Join(netprofile.Clients, ", "))
		}
	}
	for _, c := range netprofile.Clients {
		if slices.Contains(p.Clients, c) {
			env = append(env, clientPreset(c, p.ProxyURL, proxy.Hostname(), proxy.Port(), noProxy)...)
		}
	}
	return env, nil
}

// clientPreset is one client's environment (§4.2's preset table): proxy is
// the profile's proxy URL, host and port its parts, noProxy NO_PROXY's
// value.
func clientPreset(c, proxy, host, port, noProxy string) []corev1.EnvVar {
	switch c {
	case netprofile.ClientNPM:
		// npm reads env above a project .npmrc; pnpm 11 reads only its own
		// pnpm_config_* names; audit off keeps the upload small.
		return []corev1.EnvVar{
			{Name: "npm_config_https_proxy", Value: proxy},
			{Name: "npm_config_noproxy", Value: noProxy},
			{Name: "npm_config_update_notifier", Value: "false"},
			{Name: "npm_config_fund", Value: "false"},
			{Name: "npm_config_audit", Value: "false"},
			{Name: "pnpm_config_https_proxy", Value: proxy},
			{Name: "pnpm_config_noproxy", Value: noProxy},
			{Name: "pnpm_config_update_notifier", Value: "false"},
			{Name: "COREPACK_ENABLE_DOWNLOAD_PROMPT", Value: "0"},
		}
	case netprofile.ClientYarn:
		// Yarn Berry does not reliably read HTTPS_PROXY.
		return []corev1.EnvVar{
			{Name: "YARN_HTTPS_PROXY", Value: proxy},
			{Name: "YARN_ENABLE_TELEMETRY", Value: "0"},
		}
	case netprofile.ClientPip:
		// PIP_PROXY beats a pip config file.
		return []corev1.EnvVar{
			{Name: "PIP_PROXY", Value: proxy},
			{Name: "PIP_DISABLE_PIP_VERSION_CHECK", Value: "1"},
			{Name: "PIP_NO_INPUT", Value: "1"},
		}
	case netprofile.ClientUV:
		// uv's interpreter downloads come from GitHub, which is denied.
		return []corev1.EnvVar{{Name: "UV_PYTHON_DOWNLOADS", Value: "never"}}
	case netprofile.ClientNuGet:
		// Certificate revocation is checked over plain http, which the
		// tunnel cannot carry.
		return []corev1.EnvVar{
			{Name: "NUGET_CERT_REVOCATION_MODE", Value: "offline"},
			{Name: "DOTNET_CLI_TELEMETRY_OPTOUT", Value: "1"},
			{Name: "DOTNET_NOLOGO", Value: "1"},
			{Name: "DOTNET_CLI_WORKLOAD_UPDATE_NOTIFY_DISABLE", Value: "1"},
		}
	case netprofile.ClientCargo:
		// A git config http.proxy would win over HTTPS_PROXY; the git
		// index protocol would clone from GitHub.
		return []corev1.EnvVar{
			{Name: "CARGO_HTTP_PROXY", Value: proxy},
			{Name: "CARGO_REGISTRIES_CRATES_IO_PROTOCOL", Value: "sparse"},
		}
	case netprofile.ClientJVM:
		// The JVM ignores HTTPS_PROXY; JAVA_TOOL_OPTIONS reaches every JVM,
		// the Gradle daemon included. No proxy auth: the port is the
		// identity.
		return []corev1.EnvVar{{Name: "JAVA_TOOL_OPTIONS", Value: fmt.Sprintf(
			"-Dhttps.proxyHost=%s -Dhttps.proxyPort=%s -Dhttp.nonProxyHosts=%s", host, port, jvmNonProxyHosts(noProxy))}}
	}
	return nil
}

// jvmNonProxyHosts is NO_PROXY in the JVM's http.nonProxyHosts form:
// "|"-separated, a leading-dot suffix written as a "*." wildcard.
func jvmNonProxyHosts(noProxy string) string {
	entries := strings.Split(noProxy, ",")
	for i, e := range entries {
		if strings.HasPrefix(e, ".") {
			entries[i] = "*" + e
		}
	}
	return strings.Join(entries, "|")
}

// noProxyHosts are the hosts urls dial, lowercased, as NO_PROXY lists them:
// unlike endpointHosts, IP literals are kept (IPv6 without brackets), since
// a broker given by address must bypass the proxy too. A URL with no host
// is an error, as in endpointHosts.
func noProxyHosts(urls []string) ([]string, error) {
	var out []string
	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("jobs: agent endpoint %q names no host for NO_PROXY", raw)
		}
		host := strings.ToLower(u.Hostname())
		if strings.ContainsAny(host, ", \t") {
			return nil, fmt.Errorf("jobs: agent endpoint host %q cannot be listed in NO_PROXY", host)
		}
		out = append(out, host)
	}
	return out, nil
}

// sortedUnique is ss sorted with duplicates removed.
func sortedUnique(ss []string) []string {
	set := map[string]bool{}
	for _, s := range ss {
		set[s] = true
	}
	return slices.Sorted(maps.Keys(set))
}
