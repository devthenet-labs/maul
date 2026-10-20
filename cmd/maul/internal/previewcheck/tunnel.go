// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/devthenet-labs/maul/cmd/maul/internal/checkreport"
	"github.com/devthenet-labs/maul/cmd/maul/internal/previewsetup"
)

// A Cloudflare Tunnel edge (design §2.3, the cloudflare-tunnel recipe R1):
// the gateway's exposure is a ClusterIP Service that cloudflared reaches
// from inside the cluster, so it publishes no address of its own. Every
// preview host and the sign-in host are proxied records, each a CNAME to
// <tunnel id>.cfargotunnel.com. Cloudflare answers a proxied record with
// its own anycast addresses and hides the CNAME, so from outside the DNS
// line can see one of two things: the CNAME itself (a resolver that shows
// it), or Cloudflare's addresses. Only the CNAME names a target: every
// proxied record answers with Cloudflare's addresses, whatever origin it
// routes to, so those are a SKIP, and preview-auth-host's request through
// that edge is what shows the record routes to this install's gateway.

// cfargotunnelSuffix is the zone every tunnel's CNAME target sits in.
const cfargotunnelSuffix = ".cfargotunnel.com"

// tunnelIDPattern is a tunnel ID: a lowercase UUID.
var tunnelIDPattern = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

// cloudflareRanges are the addresses Cloudflare's proxy answers from, as
// Cloudflare publishes them (https://www.cloudflare.com/ips/). A list that
// lags Cloudflare's makes a proxied record look like an unknown address,
// a WARN or SKIP, never a PASS.
var cloudflareRanges = func() []netip.Prefix {
	ranges := []string{
		"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22", "141.101.64.0/18",
		"108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20", "197.234.240.0/22", "198.41.128.0/17",
		"162.158.0.0/15", "104.16.0.0/13", "104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
		"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32", "2405:8100::/32",
		"2a06:98c0::/29", "2c0f:f248::/32",
	}
	out := make([]netip.Prefix, 0, len(ranges))
	for _, p := range ranges {
		out = append(out, netip.MustParsePrefix(p))
	}
	return out
}()

// cloudflareProxied reports whether every address is one of Cloudflare's
// proxy's.
func cloudflareProxied(addrs []string) bool {
	if len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			return false
		}
		ip = ip.Unmap()
		if !slices.ContainsFunc(cloudflareRanges, func(p netip.Prefix) bool { return p.Contains(ip) }) {
			return false
		}
	}
	return true
}

// tunnelEdge is a gateway exposure that publishes no address, which a
// Cloudflare Tunnel may serve.
type tunnelEdge struct {
	// id is the tunnel the cloudflare-tunnel recipe's cloudflared runs for
	// this install's host suffix, and where its configuration says so;
	// both empty when no such cloudflared was found.
	id, where string
	// lookup says why no cloudflared could be looked for, when it could not.
	lookup string
}

// cloudflaredConfig is the part of cloudflared's config.yaml the check
// reads: the tunnel and the hostnames it routes.
type cloudflaredConfig struct {
	Tunnel  string            `json:"tunnel"`
	Ingress []cloudflaredRule `json:"ingress"`
}

// cloudflaredRule is one of cloudflared's ingress rules.
type cloudflaredRule struct {
	Hostname string `json:"hostname"`
}

// findTunnel looks for the cloudflare-tunnel recipe's cloudflared, in
// whatever namespace it runs: its ConfigMap (never its credentials Secret)
// names the tunnel, and its ingress rules must route *.<suffix>.
func (c *Checker) findTunnel(ctx context.Context, suffix string) tunnelEdge {
	var list corev1.ConfigMapList
	if err := c.cfg.Reader.List(ctx, &list, client.MatchingLabels{
		LabelName: previewsetup.CloudflaredAppName, labelInstance: previewsetup.TunnelName,
	}); err != nil {
		return tunnelEdge{lookup: fmt.Sprintf("cannot list cloudflared's ConfigMaps: %v", err)}
	}
	items := list.Items
	slices.SortFunc(items, func(a, b corev1.ConfigMap) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	for _, cm := range items {
		var cfg cloudflaredConfig
		if err := yaml.Unmarshal([]byte(cm.Data[previewsetup.CloudflaredConfigKey]), &cfg); err != nil {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(cfg.Tunnel))
		if !tunnelIDPattern.MatchString(id) {
			continue
		}
		if slices.ContainsFunc(cfg.Ingress, func(r cloudflaredRule) bool {
			return strings.EqualFold(strings.TrimSpace(r.Hostname), "*."+suffix)
		}) {
			return tunnelEdge{id: id, where: "cloudflared in " + cm.Namespace + "/" + cm.Name}
		}
	}
	return tunnelEdge{}
}

// labelInstance is the recipe's instance label on cloudflared's objects.
const labelInstance = "app.kubernetes.io/instance"

// cnameResolver is the optional half of a Resolver that reports a host's
// canonical name; *net.Resolver has it.
type cnameResolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
}

// cname is host's CNAME target, "" when the resolver cannot say or the
// host is no alias.
func (c *Checker) cname(ctx context.Context, host string) string {
	r, ok := c.cfg.Resolver.(cnameResolver)
	if !ok {
		return ""
	}
	name, err := r.LookupCNAME(ctx, host)
	if err != nil {
		return ""
	}
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if name == strings.ToLower(strings.TrimSuffix(host, ".")) {
		return ""
	}
	return name
}

// tunnelDNSInput is what judgeTunnelDNS reads.
type tunnelDNSInput struct {
	// host is the name looked up, and what names its record in a reason.
	host, record string
	// tunnel is the recipe's tunnel, its id empty when none was found.
	tunnel tunnelEdge
	// cname is host's CNAME target, "" when none was seen.
	cname string
	// addrs are host's addresses, sorted.
	addrs []string
}

// judgeTunnelDNS is the DNS line for an exposure that publishes no
// address, pure. ok is false when nothing about the record or the cluster
// says a tunnel serves it, and the generic reason stands. Only a visible
// CNAME to the recipe's tunnel passes; a proxied record, whose target is
// hidden, is a SKIP; one pointing elsewhere, or at a
// tunnel not known to be this install's, is a WARN or a SKIP that says
// what to look at, never a PASS.
func judgeTunnelDNS(in tunnelDNSInput) (status checkreport.Status, reason string, ok bool) {
	got := strings.Join(in.addrs, ", ")
	route := ""
	if in.tunnel.id != "" {
		route = fmt.Sprintf("`cloudflared tunnel route dns --overwrite-dns %s '%s'`", in.tunnel.id, in.record)
	}
	if id, isTunnel := strings.CutSuffix(in.cname, cfargotunnelSuffix); isTunnel && tunnelIDPattern.MatchString(id) {
		switch in.tunnel.id {
		case id:
			return checkreport.Pass, fmt.Sprintf("%s is a CNAME to %s, Cloudflare Tunnel %s, which %s serves", in.host,
				in.cname, id, in.tunnel.where), true
		case "":
			return checkreport.Skip, fmt.Sprintf("%s is a CNAME to %s, Cloudflare Tunnel %s, but no cloudflared from "+
				"`maul setup previews --edge cloudflare-tunnel` routes this host suffix, so which tunnel serves this "+
				"install is unknown: check its ingress rules send the host to the preview gateway", in.host, in.cname,
				id), true
		default:
			return checkreport.Warn, fmt.Sprintf("%s is a CNAME to %s, but %s serves tunnel %s: point the record at "+
				"%s%s with %s", in.host, in.cname, in.tunnel.where, in.tunnel.id, in.tunnel.id, cfargotunnelSuffix,
				route), true
		}
	}
	switch {
	case in.cname != "" && in.tunnel.id != "":
		// A CNAME this resolver shows is the record's target, and it is not
		// the tunnel, whatever addresses it ends at.
		return checkreport.Warn, fmt.Sprintf("%s is a CNAME to %s, not to %s%s, the tunnel %s serves: route it "+
			"with %s", in.host, in.cname, in.tunnel.id, cfargotunnelSuffix, in.tunnel.where, route), true
	case cloudflareProxied(in.addrs) && in.tunnel.id != "":
		// Any proxied record answers with these addresses, whatever origin
		// it routes to, so they prove nothing about the tunnel.
		return checkreport.Skip, fmt.Sprintf("%s resolves to Cloudflare's proxy (%s): proxied; the target is "+
			"hidden, preview-auth-host proves routing to Cloudflare Tunnel %s, which %s serves (or check in the "+
			"Cloudflare dashboard that the record is a proxied CNAME to %s%s)", in.host, got, in.tunnel.id,
			in.tunnel.where, in.tunnel.id, cfargotunnelSuffix), true
	case cloudflareProxied(in.addrs):
		return checkreport.Skip, fmt.Sprintf("%s resolves to Cloudflare's proxy (%s), which hides where the record "+
			"routes, and no cloudflared from `maul setup previews --edge cloudflare-tunnel` routes this host "+
			"suffix: check in the Cloudflare dashboard that the record is a proxied CNAME to your tunnel", in.host,
			got), true
	case in.tunnel.id != "" && allPrivate(in.addrs):
		return checkreport.Skip, fmt.Sprintf("%s resolves to %s here, a private address, though %s serves Cloudflare "+
			"Tunnel %s: a split-horizon resolver may hold another record; run this where viewers resolve it", in.host,
			got, in.tunnel.where, in.tunnel.id), true
	case in.tunnel.id != "":
		return checkreport.Warn, fmt.Sprintf("%s resolves to %s, neither Cloudflare's proxy nor a CNAME to "+
			"%s%s, the tunnel %s serves: route it with %s", in.host, got, in.tunnel.id, cfargotunnelSuffix,
			in.tunnel.where, route), true
	}
	return "", "", false
}

// tunnelHint names the recipe's tunnel, when known, for a reason about a
// record that does not resolve.
func tunnelHint(t *tunnelEdge) string {
	if t == nil || t.id == "" {
		return ""
	}
	return fmt.Sprintf(", Cloudflare Tunnel %s (%s): a proxied CNAME to %s%s, which `cloudflared tunnel route dns` "+
		"creates", t.id, t.where, t.id, cfargotunnelSuffix)
}
