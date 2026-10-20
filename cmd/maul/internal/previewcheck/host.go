// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/devthenet-labs/maul/cmd/maul/internal/checkreport"
)

// The placeholder Ingress the chart keeps in slot 0 so the preview load
// balancer, and so its address, outlives every Preview
// (charts/maul/templates/preview-foundation.yaml). Its status address is
// the load balancer the wildcard record must point at.
const (
	PlaceholderNamespace = "maul-preview-0"
	PlaceholderName      = "maul-preview-placeholder"
)

// tlsTimeout bounds the preview TLS handshake. The preview load balancer
// drops connections from outside its inbound CIDRs, so a timeout is the
// expected answer from anywhere else and must not hang the run.
const tlsTimeout = 10 * time.Second

// previewHost checks the preview host: <project>-0.<suffix> resolves to the
// placeholder Ingress's load balancer, and serves a certificate trusted for
// it. No Intent is numbered 0, so no Preview owns that name.
func (c *Checker) previewHost(ctx context.Context, suffix string) {
	if suffix == "" {
		c.add(CheckPreviewDNS, checkreport.Skip, "preview-controller's host suffix is unknown")
		c.add(CheckPreviewTLS, checkreport.Skip, "preview-controller's host suffix is unknown")
		return
	}
	host := c.cfg.Project + "-0." + suffix
	addrs, err := c.cfg.Resolver.LookupHost(ctx, host)
	var dnsErr *net.DNSError
	switch {
	case err != nil && errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		c.add(CheckPreviewDNS, checkreport.Fail, "%s does not resolve (%v): a wildcard record *.%s must point "+
			"at the preview load balancer", host, err, suffix)
		c.add(CheckPreviewTLS, checkreport.Skip, "%s does not resolve", host)
		return
	case err != nil:
		// A timeout, a SERVFAIL or the run's own deadline says nothing
		// about the record: only NXDOMAIN is evidence that it is missing.
		c.add(CheckPreviewDNS, checkreport.Skip, "cannot look %s up (%v), which does not say whether the "+
			"record exists; run this again", host, err)
		c.add(CheckPreviewTLS, checkreport.Skip, "%s could not be looked up", host)
		return
	}
	slices.Sort(addrs)
	switch lb := c.loadBalancer(ctx); {
	case lb.why != "" && lb.status == checkreport.Fail:
		c.add(CheckPreviewDNS, checkreport.Fail, "%s resolves to %s, but there is no preview load balancer for "+
			"it to point at: %s", host, strings.Join(addrs, ", "), lb.why)
	case lb.why != "":
		c.add(CheckPreviewDNS, checkreport.Skip, "%s resolves to %s, but it cannot be compared with the preview "+
			"load balancer: %s", host, strings.Join(addrs, ", "), lb.why)
	case slices.ContainsFunc(addrs, func(a string) bool { return slices.Contains(lb.addrs, a) }):
		c.add(CheckPreviewDNS, checkreport.Pass, "%s resolves to the preview load balancer %s (%s)", host,
			lb.name, strings.Join(addrs, ", "))
	default:
		c.add(CheckPreviewDNS, checkreport.Fail, "%s resolves to %s, but the preview load balancer %s is at %s: "+
			"the wildcard record *.%s must point at it", host, strings.Join(addrs, ", "), lb.name,
			strings.Join(lb.addrs, ", "), suffix)
	}

	c.previewTLS(ctx, host, "*."+suffix, "the preview load balancer admits only the chart's preview.inboundCIDRs")
}

// previewTLS checks host serves a certificate trusted for it. cover is the
// name the certificate must cover (*.<suffix>, or the sign-in host itself),
// and admits says who answers only some addresses, for a timeout's reason.
func (c *Checker) previewTLS(ctx context.Context, host, cover, admits string) {
	dctx, cancel := context.WithTimeout(ctx, tlsTimeout)
	state, err := c.cfg.DialTLS(dctx, host)
	cancel()
	var verify *tls.CertificateVerificationError
	var hostname x509.HostnameError
	switch {
	case err == nil && len(state.PeerCertificates) > 0:
		leaf := state.PeerCertificates[0]
		c.add(CheckPreviewTLS, checkreport.Pass, "%s serves a trusted certificate for %s (issued by %s, valid "+
			"until %s)", host, strings.Join(leaf.DNSNames, ", "), leaf.Issuer.CommonName,
			leaf.NotAfter.UTC().Format(time.DateOnly))
	case err == nil:
		c.add(CheckPreviewTLS, checkreport.Fail, "%s completed a handshake without a certificate", host)
	case timedOut(err):
		c.add(CheckPreviewTLS, checkreport.Skip, "no answer from %s:443 within %s: %s, so this is expected "+
			"from any other address; run it from an admitted one", host, tlsTimeout, admits)
	case errors.As(err, &verify) || errors.As(err, &hostname):
		c.add(CheckPreviewTLS, checkreport.Fail, "%s serves a certificate that is not trusted for it (it must "+
			"cover %s): %v", host, cover, err)
	default:
		c.add(CheckPreviewTLS, checkreport.Fail, "%s:443: %v", host, err)
	}
}

// previewLB is the preview load balancer as the placeholder Ingress reports
// it: its name and addresses, or why it cannot be compared against.
type previewLB struct {
	name  string
	addrs []string
	// why is set when it cannot be compared against, and status then says
	// what that makes the DNS check: Skip when it could not be found out,
	// Fail when the Ingress shows there is no load balancer.
	why    string
	status checkreport.Status
}

// loadBalancer reads the placeholder Ingress's load balancer.
func (c *Checker) loadBalancer(ctx context.Context) previewLB {
	var ing networkingv1.Ingress
	key := types.NamespacedName{Namespace: PlaceholderNamespace, Name: PlaceholderName}
	if err := c.cfg.Reader.Get(ctx, key, &ing); err != nil {
		return previewLB{status: checkreport.Skip, why: fmt.Sprintf("cannot read the placeholder Ingress %s: %v",
			key, err)}
	}
	for _, in := range ing.Status.LoadBalancer.Ingress {
		switch {
		case in.Hostname != "":
			addrs, err := c.cfg.Resolver.LookupHost(ctx, in.Hostname)
			if err != nil {
				return previewLB{status: checkreport.Skip, why: fmt.Sprintf("its address %s does not resolve: %v",
					in.Hostname, err)}
			}
			return previewLB{name: in.Hostname, addrs: addrs}
		case in.IP != "":
			return previewLB{name: in.IP, addrs: []string{in.IP}}
		}
	}
	return previewLB{status: checkreport.Fail, why: fmt.Sprintf("the placeholder Ingress %s has no load balancer "+
		"address, so the preview load balancer is not provisioned (the Ingress's events say why; one created "+
		"minutes ago may still be provisioning)", key)}
}
