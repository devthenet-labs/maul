// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"fmt"

	"sigs.k8s.io/yaml"
)

// The existing-edge recipe's names, the ones
// docs/configuration/preview-gateway.md's recipes use.
const (
	// CertificateName and CertificateSecret are the wildcard Certificate
	// and its Secret, covering *.<suffix> and the sign-in host.
	CertificateName   = "preview-wildcard"
	CertificateSecret = "preview-wildcard-tls"
	// SignInRouteName is the HTTPRoute that sends the sign-in host to the
	// gateway on a Gateway API edge.
	SignInRouteName = "preview-sign-in"

	CertificateFile = "manifests/certificate.yaml"
	SignInRouteFile = "manifests/sign-in-route.yaml"

	// forwardedHopsEdge is the proxies appending X-Forwarded-For in front
	// of the gateway behind one existing edge: one ALB, ingress controller
	// or Gateway (design §5.3: fixed per recipe, never asked).
	forwardedHopsEdge = 1
)

// IngressController is the controller an Ingress class names, as far as
// the wizard tunes for it: the class names the docs' recipes use.
func IngressController(class string) string {
	switch class {
	case "nginx", "traefik", "alb":
		return class
	}
	return ""
}

// existingRecipe is R4 (design §4.3): an edge the cluster already runs
// routes *.<suffix> and the sign-in host. On an Ingress class both the
// gateway's Ingress and the relay's (the sign-in host, G3) are classed and
// carry TLS; on a Gateway API parent the chart's HTTPRoute carries the
// previews and a manifest of the wizard's the sign-in host, both onto the
// gateway's Service. A cert-manager Certificate covers both names unless
// the edge holds the certificate.
func existingRecipe(a Answers, v values) ([]File, error) {
	gw := v.at("previewAuth", "gateway")
	gw["forwardedHops"] = forwardedHopsEdge
	var files []File
	if a.Existing.GatewayParent != "" {
		ns, name, section, err := ParseGatewayParent(a.Existing.GatewayParent)
		if err != nil {
			return nil, err
		}
		parent := map[string]any{"name": name, "namespace": ns}
		if section != "" {
			parent["sectionName"] = section
		}
		v.at("previewAuth", "gateway", "ingress")["enabled"] = false
		v.at("previewAuth", "gateway", "httpRoute")["parentRefs"] = []any{parent}
		route, err := signInRoute(a, ns, name)
		if err != nil {
			return nil, err
		}
		return append(files, File{Path: SignInRouteFile, Data: route}), nil
	}
	class := a.Existing.IngressClass
	gwIngress := v.at("previewAuth", "gateway", "ingress")
	gwIngress["enabled"] = true
	gwIngress["className"] = class
	relay := v.at("previewAuth", "ingress")
	relay["className"] = class
	gwAnn, relayAnn := ingressAnnotations(class)
	gwIngress["annotations"] = gwAnn
	relay["annotations"] = relayAnn
	if a.EdgeTLS() == TLSCertificate {
		gwIngress["tls"] = []any{map[string]any{"hosts": []string{"*." + a.HostSuffix},
			"secretName": CertificateSecret}}
		relay["tls"] = []any{map[string]any{"hosts": []string{a.SignInHost}, "secretName": CertificateSecret}}
		cert, err := certificate(a)
		if err != nil {
			return nil, err
		}
		files = append(files, File{Path: CertificateFile, Data: cert})
	} else {
		gwIngress["tls"] = []any{}
		relay["tls"] = []any{}
	}
	return files, nil
}

// ingressAnnotations are the gateway's and the relay's Ingress annotations
// for the controller a class names (the docs' recipes): ingress-nginx's
// body size and stream timeouts, Traefik's TLS entrypoint. Never a
// cert-manager annotation: the Certificate is explicit (G5).
func ingressAnnotations(class string) (gateway, relay map[string]any) {
	switch IngressController(class) {
	case "nginx":
		return map[string]any{
			"nginx.ingress.kubernetes.io/proxy-body-size":    "32m",
			"nginx.ingress.kubernetes.io/proxy-read-timeout": "3600",
			"nginx.ingress.kubernetes.io/proxy-send-timeout": "3600",
			"nginx.ingress.kubernetes.io/proxy-buffering":    "off",
		}, map[string]any{}
	case "traefik":
		ep := map[string]any{"traefik.ingress.kubernetes.io/router.entrypoints": "websecure"}
		return ep, map[string]any{"traefik.ingress.kubernetes.io/router.entrypoints": "websecure"}
	}
	return map[string]any{}, map[string]any{}
}

// certificate is the wildcard Certificate (design §4.3, the docs' "The
// wildcard certificate"): *.<suffix> and the sign-in host, ECDSA, a new
// key at every renewal.
func certificate(a Answers) ([]byte, error) {
	kind, name := a.Issuer()
	return manifest(map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Certificate",
		"metadata":   map[string]any{"name": CertificateName, "namespace": a.Namespace},
		"spec": map[string]any{
			"secretName": CertificateSecret,
			"issuerRef":  map[string]any{"kind": kind, "name": name},
			"dnsNames":   []string{"*." + a.HostSuffix, a.SignInHost},
			"privateKey": map[string]any{"algorithm": "ECDSA", "size": 256, "rotationPolicy": "Always"},
		},
	}, certificateComment(a)...)
}

// certificateComment says who serves the Certificate's Secret.
func certificateComment(a Answers) []string {
	who := "The wildcard certificate both Ingresses name"
	if a.GatewayTerminatesTLS() {
		who = "The wildcard certificate the gateway terminates TLS with"
	}
	return []string{who + ": every preview host and the sign-in host.",
		"Apply it once cert-manager runs and its issuer can answer DNS-01 for both names."}
}

// signInRoute sends the sign-in host to the gateway's Service on a Gateway
// API edge, which answers the relay's host as the relay does: the chart's
// HTTPRoute carries only *.<suffix> while the sign-in host is
// previewAuth.host. No sectionName, so it attaches to the Gateway's
// listener for that host.
func signInRoute(a Answers, ns, name string) ([]byte, error) {
	return manifest(map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata":   map[string]any{"name": SignInRouteName, "namespace": a.Namespace},
		"spec": map[string]any{
			"parentRefs": []any{map[string]any{"name": name, "namespace": ns}},
			"hostnames":  []string{a.SignInHost},
			"rules": []any{map[string]any{
				"backendRefs": []any{map[string]any{"name": a.GatewayService(), "port": 80}},
			}},
		},
	}, "The sign-in host's route onto the preview gateway, on the Gateway's HTTPS listener",
		"for that host.")
}

// manifest marshals obj under a comment, one line per entry.
func manifest(obj map[string]any, comment ...string) ([]byte, error) {
	body, err := yaml.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("render a manifest: %w", err)
	}
	var head []byte
	for _, c := range append([]string{"Generated by `maul setup previews`."}, comment...) {
		head = append(head, "# "+c+"\n"...)
	}
	return append(head, body...), nil
}
