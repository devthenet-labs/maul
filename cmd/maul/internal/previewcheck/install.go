// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/net/publicsuffix"

	"github.com/devthenet-labs/maul/cmd/maul/internal/checkreport"
	"github.com/devthenet-labs/maul/internal/previewauth"
)

// The install-level checks (`maul check previews`), beside the shared
// host and sign-in ones.
const (
	CheckPreviewConfig  = "preview-config"
	CheckPreviewViewers = "preview-viewers"
	CheckPreviewNodes   = "preview-nodes"
)

// ProbeProject is the Project an install-level run names: its probe host,
// <ProbeProject>-0.<suffix>, is previewauth.UnknownProbeLabel's, the host the
// relay's own background probe asks for, which no Preview owns.
var ProbeProject = strings.TrimSuffix(previewauth.UnknownProbeLabel, "-0")

// The settings the install-level checks read beyond the shared ones.
const (
	keyPreviewHostSuffix = "MAUL_PREVIEW_HOST_SUFFIX"
	keyAuthHostSuffix    = "MAUL_PREVIEW_AUTH_HOST_SUFFIX"
)

// sharedSiteAnnotation is the chart's previewAuth.sharedSiteAcknowledged on
// the relay's ConfigMap: the registrable domain the operator accepted the
// preview hosts sharing with sign-in. Recorded only; nothing but this check
// reads it.
const sharedSiteAnnotation = "maul.run/preview-shared-site-acknowledged"

// Report is an install-level run's outcome: every line is about the
// namespace's preview install as a whole.
type Report struct {
	Namespace string `json:"namespace"`
	Checks    []Line `json:"checks"`
}

// Lines renders the report as table lines, one cell (the check) each.
func (r Report) Lines() []checkreport.Line {
	out := make([]checkreport.Line, 0, len(r.Checks))
	for _, l := range r.Checks {
		out = append(out, checkreport.Line{Status: l.Status, Cells: []string{l.Name}, Reason: l.Reason})
	}
	return out
}

// Failed counts the checks that failed.
func (r Report) Failed() int { return checkreport.Failed(r.Lines()) }

// Install runs the install-level checks, before any Project exists: the
// preview configuration as a whole (preview-config), who may view previews
// (preview-viewers), the preview nodes (preview-nodes), the pinned upstreams'
// versions (preview-upstreams), then the shared host and sign-in checks
// against the probe host and every Ready Preview. cfg.Project and
// cfg.AllPreviews are set here. It reads ConfigMaps, the gateway's exposure,
// Previews, RoleBindings, ClusterRoleBindings, Nodes and Deployments; never a
// Secret.
func Install(ctx context.Context, cfg Config) Report {
	cfg.Project, cfg.AllPreviews = ProbeProject, true
	c := New(cfg)
	s := LoadSettings(ctx, cfg.Reader, cfg.Namespace)
	suffix := c.previewConfig(s)
	// The cluster-only checks run before the network ones, whose timeouts
	// (from outside an allowlist) could otherwise spend the run's deadline.
	c.viewers(ctx, s)
	c.nodes(ctx, s)
	c.upstreamsCheck(ctx)
	c.Host(ctx, s, suffix)
	c.Auth(ctx, s, suffix)
	if cfg.Isolation != nil {
		c.isolation(ctx, s)
	}
	return Report{Namespace: cfg.Namespace, Checks: c.Lines()}
}

// previewConfig judges the preview install's configuration as a whole and
// returns the host suffix the host checks probe under ("" when unknown):
// previews and sign-in are on and agree on the suffix, the preview gateway
// alone serves preview hosts (the legacy path serving too is a WARN, and
// serving without sign-in a FAIL), the sign-in host sits outside the suffix, and
// the suffix is a registrable domain of its own, shared with neither the
// sign-in host, the relay's issuer nor Dex (a WARN otherwise: those hosts'
// cookies would be same-site with every preview's; INFO once the operator
// has acknowledged that very domain, previewAuth.sharedSiteAcknowledged).
func (c *Checker) previewConfig(s Settings) string {
	switch {
	case s.Preview.Err != nil:
		c.add(CheckPreviewConfig, checkreport.Skip, "cannot read the controllers' settings: %v", s.Preview.Err)
		return ""
	case !s.Preview.Found():
		c.add(CheckPreviewConfig, checkreport.Fail, "no preview-controller ConfigMap in namespace %s: are "+
			"preview.enabled and previewController.enabled set in the maul chart?", c.cfg.Namespace)
		return ""
	}
	suffix := strings.TrimSpace(s.Preview.Data[keyPreviewHostSuffix])
	if suffix == "" {
		c.add(CheckPreviewConfig, checkreport.Fail, "%s sets no %s (preview.hostSuffix)", s.Preview.From,
			keyPreviewHostSuffix)
		return ""
	}
	if !s.Auth.Found() {
		c.add(CheckPreviewConfig, checkreport.Fail, "preview sign-in is off: there is no preview-auth ConfigMap in "+
			"namespace %s (previewAuth.enabled), so nothing signs viewers in to the previews at <label>.%s",
			c.cfg.Namespace, suffix)
		return suffix
	}
	v := judgeConfig(configInput{
		suffix:     suffix,
		authSuffix: strings.TrimSpace(s.Auth.Data[keyAuthHostSuffix]),
		issuer:     s.Auth.Data[keyAuthIssuer],
		signIn:     strings.TrimSpace(s.Auth.Data[keyAuthGatewaySignIn]),
		dexIssuer:  s.Auth.Data[keyAuthDexIssuer],
		gateway:    s.Auth.Bool(keyAuthGateway),
		legacy:     s.legacyIngress(),
		required:   s.Preview.Bool(keyAuthRequired),
		sharedSite: strings.TrimSpace(s.Auth.Annotations[sharedSiteAnnotation]),
	})
	c.add(CheckPreviewConfig, v.status, "%s", v.reason)
	return suffix
}

// configInput is what judgeConfig reads: the settings as the ConfigMaps
// hold them.
type configInput struct {
	suffix, authSuffix, issuer, signIn, dexIssuer string
	// gateway is the relay's gateway switch, legacy preview-controller's
	// legacy slot Ingress switch, and required whether that Ingress is put
	// behind sign-in (the legacy path's require stage).
	gateway, legacy, required bool
	// sharedSite is the registrable domain the operator acknowledged the
	// suffix sharing with sign-in ("" for none).
	sharedSite string
}

// legacyIngress is preview-controller's legacy slot Ingress switch, its
// flag's default (true) when unset or unreadable.
func (s Settings) legacyIngress() bool {
	v, ok := s.Preview.Data[keyPreviewLegacyIngress]
	if !ok {
		return true
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	return err != nil || b
}

// configVerdict is preview-config's line.
type configVerdict struct {
	status checkreport.Status
	reason string
}

// judgeConfig is preview-config's verdict, pure. A problem that breaks
// previews fails; one that leaves them working but weaker (the legacy path
// still serving, a registrable domain shared with a sign-in host) warns; a
// shared registrable domain the operator acknowledged is INFO; every
// finding is named, the worst deciding the status.
func judgeConfig(in configInput) configVerdict {
	var fails, warns, infos []string
	if in.authSuffix != "" && in.authSuffix != in.suffix {
		fails = append(fails, fmt.Sprintf("the relay's host suffix %s is not preview-controller's %s", in.authSuffix,
			in.suffix))
	}
	issuerHost := ""
	if previewauth.ValidIssuer(in.issuer) {
		issuerHost = strings.TrimPrefix(in.issuer, "https://")
	} else {
		fails = append(fails, fmt.Sprintf("the relay's issuer %q is not an https origin", in.issuer))
	}
	signIn := in.signIn
	if signIn == "" {
		signIn = issuerHost
	}
	switch {
	case !in.gateway && !in.legacy:
		fails = append(fails, "nothing serves preview hosts: the preview gateway is off "+
			"(previewAuth.gateway.enabled) and so is the legacy slot Ingress (preview.legacyIngress)")
	case in.legacy && !in.required:
		fails = append(fails, "the legacy slot Ingress serves preview hosts without sign-in "+
			"(previewAuth.stage permit): require it, or turn the legacy path off once the gateway serves")
	case !in.gateway:
		warns = append(warns, "the preview gateway is off (previewAuth.gateway.enabled), so the legacy load "+
			"balancer path serves preview hosts")
	case in.legacy:
		warns = append(warns, "both the preview gateway and the legacy slot Ingress serve preview hosts: once the "+
			"wildcard record points at the gateway's edge, set preview.legacyIngress false")
	}
	site := ""
	if signIn != "" {
		if _, err := previewauth.NewHosts(in.suffix, signIn); err != nil {
			fails = append(fails, strings.TrimPrefix(err.Error(), "previewauth: "))
		}
	}
	switch reg, err := publicsuffix.EffectiveTLDPlusOne(in.suffix); {
	case err != nil:
		fails = append(fails, fmt.Sprintf("the host suffix %s has no registrable domain of its own (%v): "+
			"previews must sit under a domain you register", in.suffix, err))
	default:
		site = reg
		w, i := judgeSharedSite(in, reg, sameSite(reg, signIn, issuerHost, hostOf(in.dexIssuer)))
		warns, infos = append(warns, w...), append(infos, i...)
	}
	switch {
	case len(fails) > 0:
		return configVerdict{checkreport.Fail, strings.Join(slices.Concat(fails, warns, infos), "; ")}
	case len(warns) > 0:
		return configVerdict{checkreport.Warn, strings.Join(slices.Concat(warns, infos), "; ")}
	case len(infos) > 0:
		return configVerdict{checkreport.Info, fmt.Sprintf("the preview gateway alone serves <label>.%s and "+
			"viewers sign in at %s; %s", in.suffix, signIn, strings.Join(infos, "; "))}
	}
	return configVerdict{checkreport.Pass, fmt.Sprintf("the preview gateway alone serves <label>.%s, viewers "+
		"sign in at %s, and %s is a registrable domain shared with no sign-in host", in.suffix, signIn, site)}
}

// judgeSharedSite is the shared-registrable-domain finding: shared lists
// the sign-in hosts on reg, the suffix's registrable domain. Sharing it
// warns unless in.sharedSite acknowledges reg itself, when it is INFO; an
// acknowledgement nothing needs is INFO too.
func judgeSharedSite(in configInput, reg string, shared []string) (warns, infos []string) {
	with := strings.Join(shared, ", ")
	switch {
	case len(shared) > 0 && in.sharedSite == reg:
		infos = append(infos, fmt.Sprintf("the host suffix %s shares its registrable domain %s with %s, "+
			"acknowledged (previewAuth.sharedSiteAcknowledged): preview code runs same-site with sign-in, so "+
			"no other app on %s may rely on parent-domain cookies or SameSite for CSRF", in.suffix, reg, with, reg))
	case len(shared) > 0 && in.sharedSite != "":
		warns = append(warns, fmt.Sprintf("the host suffix %s shares its registrable domain %s with %s, so "+
			"preview code runs same-site with sign-in, and previewAuth.sharedSiteAcknowledged names %s, not %s",
			in.suffix, reg, with, in.sharedSite, reg))
	case len(shared) > 0:
		warns = append(warns, fmt.Sprintf("the host suffix %s shares its registrable domain %s with %s, so "+
			"preview code runs same-site with sign-in: put previews on a domain of their own, or accept it "+
			"with previewAuth.sharedSiteAcknowledged: %s (maul setup previews --shared-domain acknowledged)",
			in.suffix, reg, with, reg))
	case in.sharedSite != "":
		infos = append(infos, fmt.Sprintf("previewAuth.sharedSiteAcknowledged names %s, but no sign-in host "+
			"shares the host suffix's registrable domain %s: the acknowledgement is unused", in.sharedSite, reg))
	}
	return warns, infos
}

// sameSite lists the hosts whose registrable domain is reg, each once.
func sameSite(reg string, hosts ...string) []string {
	var out []string
	for _, h := range hosts {
		if h == "" || slices.Contains(out, h) {
			continue
		}
		if r, err := publicsuffix.EffectiveTLDPlusOne(h); err == nil && r == reg {
			out = append(out, h)
		}
	}
	return out
}

// hostOf is an https URL's host name, "" when it is none.
func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
