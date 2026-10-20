// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/mod/semver"
	appsv1 "k8s.io/api/apps/v1"

	"github.com/devthenet-labs/maul/cmd/maul/internal/checkreport"
	"github.com/devthenet-labs/maul/cmd/maul/internal/previewsetup"
)

// CheckPreviewUpstreams compares the upstream components the preview
// recipes write values for with the versions `setup previews` pins
// (design §5.3): a pin is maul's recommendation, so an install older than
// it runs what maul no longer recommends.
const CheckPreviewUpstreams = "preview-upstreams"

// upstream is one pinned upstream component, found by the image its
// Deployment runs.
type upstream struct {
	// name is how a reason names it.
	name string
	// image is the image's repository path, without a registry: a mirror
	// that keeps the path under a prefix of its own is matched too.
	image string
	// pin is the version the recipes are written for.
	pin string
}

// upstreams are the pinned components: the two charts' controllers and the
// cloudflared image.
func upstreams() []upstream {
	return []upstream{
		{name: "cert-manager", image: "jetstack/cert-manager-controller", pin: previewsetup.CertManager.Version},
		{name: "the Tailscale operator", image: "tailscale/k8s-operator", pin: previewsetup.TailscaleOperator.Version},
		{name: "cloudflared", image: "cloudflare/cloudflared", pin: previewsetup.Cloudflared.Tag},
	}
}

// labelVersion is the chart label a Deployment may carry its version in.
const labelVersion = "app.kubernetes.io/version"

// installed is one Deployment running an upstream.
type installed struct {
	up    upstream
	where string
	// version is what it runs, "" when neither the image's tag nor the
	// Deployment's version label says.
	version string
}

// upstreamsCheck reports the pinned upstreams the cluster runs against
// their pins, reading Deployments (their images and labels) and nothing
// else. None running is no line: no recipe needs them all.
func (c *Checker) upstreamsCheck(ctx context.Context) {
	var list appsv1.DeploymentList
	if err := c.cfg.Reader.List(ctx, &list); err != nil {
		c.add(CheckPreviewUpstreams, checkreport.Skip, "cannot list the Deployments, so cert-manager, the "+
			"Tailscale operator and cloudflared cannot be compared with the versions maul pins: %v", err)
		return
	}
	found := findUpstreams(list.Items)
	if len(found) == 0 {
		return
	}
	v := judgeUpstreams(found)
	c.add(CheckPreviewUpstreams, v.status, "%s", v.reason)
}

// findUpstreams lists every Deployment running a pinned upstream, sorted
// by namespace and name, each upstream once per Deployment.
func findUpstreams(deps []appsv1.Deployment) []installed {
	var out []installed
	for i := range deps {
		d := &deps[i]
		for _, up := range upstreams() {
			for _, ctr := range d.Spec.Template.Spec.Containers {
				path, tag := parseImage(ctr.Image)
				if path != up.image && !strings.HasSuffix(path, "/"+up.image) {
					continue
				}
				version := tag
				if !validVersion(version) {
					version = d.Labels[labelVersion]
				}
				if !validVersion(version) {
					version = d.Spec.Template.Labels[labelVersion]
				}
				if !validVersion(version) {
					version = ""
				}
				out = append(out, installed{up: up, where: d.Namespace + "/" + d.Name, version: version})
				break
			}
		}
	}
	slices.SortStableFunc(out, func(a, b installed) int { return strings.Compare(a.where, b.where) })
	return out
}

// parseImage splits an image reference into its repository path, without
// a registry host, and its tag ("" when it has none, as a digest-only
// reference has none). Docker Hub's library/ prefix is dropped.
func parseImage(ref string) (path, tag string) {
	ref, _, _ = strings.Cut(strings.TrimSpace(ref), "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref, tag = ref[:i], ref[i+1:]
	}
	if first, rest, ok := strings.Cut(ref, "/"); ok &&
		(strings.ContainsAny(first, ".:") || first == "localhost") {
		ref = rest
	}
	return strings.TrimPrefix(ref, "library/"), tag
}

// canonical is a version as semver reads it: a leading v added.
func canonical(v string) string {
	if v == "" || strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}

// validVersion reports whether v is a version semver can compare.
func validVersion(v string) bool { return semver.IsValid(canonical(v)) }

// upstreamVerdict is preview-upstreams' line.
type upstreamVerdict struct {
	status checkreport.Status
	reason string
}

// judgeUpstreams is preview-upstreams' verdict, pure: any upstream older
// than its pin is a WARN (never a FAIL: it still works, it is just no
// longer what maul recommends), every one at or past it a PASS, and
// none whose version can be read a SKIP.
func judgeUpstreams(found []installed) upstreamVerdict {
	var older, current, unknown []string
	for _, f := range found {
		switch {
		case f.version == "":
			unknown = append(unknown, fmt.Sprintf("%s (%s) runs an image whose version neither its tag nor "+
				"its %s label says", f.up.name, f.where, labelVersion))
		case semver.Compare(canonical(f.version), canonical(f.up.pin)) < 0:
			older = append(older, fmt.Sprintf("%s %s (%s) is older than %s", f.up.name, f.version, f.where,
				f.up.pin))
		default:
			current = append(current, fmt.Sprintf("%s %s (%s)", f.up.name, f.version, f.where))
		}
	}
	const pinned = "the version this maul's preview recipes are written and tested for"
	switch {
	case len(older) > 0:
		reason := strings.Join(older, "; ") + ", " + pinned + ": upgrade it to the pinned version " +
			"(`maul setup previews` writes the command in NEXT-STEPS.md)"
		if len(current) > 0 {
			reason += "; " + strings.Join(current, ", ") + " at or past its pin"
		}
		return upstreamVerdict{checkreport.Warn, strings.Join(append([]string{reason}, unknown...), "; ")}
	case len(current) > 0:
		reason := strings.Join(current, ", ") + " at or past " + pinned
		return upstreamVerdict{checkreport.Pass, strings.Join(append([]string{reason}, unknown...), "; ")}
	}
	return upstreamVerdict{checkreport.Skip, strings.Join(unknown, "; ") + ", so it cannot be compared with " +
		pinned}
}
