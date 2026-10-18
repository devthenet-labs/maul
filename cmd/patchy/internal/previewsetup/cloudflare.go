// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/ghapp"
)

// The cloudflare-tunnel recipe's names (design §4.3 R1): cloudflared runs
// in a namespace of its own, never the release namespace, so the tunnel
// credentials (which can serve every name the tunnel routes) sit beside
// the one Deployment that mounts them and nothing of patchy's.
const (
	// DefaultCloudflaredNamespace is where cloudflared runs: a namespace
	// the recipe owns (its manifest creates it, and rolling back deletes
	// it), so never one an operator's own cloudflared may already use.
	DefaultCloudflaredNamespace = "patchy-cloudflared"
	// cloudflaredName names the Deployment, its ConfigMap, its
	// ServiceAccount and its NetworkPolicy.
	cloudflaredName = "cloudflared"
	// CloudflaredAppName is cloudflared's app.kubernetes.io/name label,
	// beside app.kubernetes.io/instance TunnelName: what `check previews`
	// finds the recipe's ConfigMap and Deployment by.
	CloudflaredAppName = cloudflaredName
	// CloudflaredConfigKey is the ConfigMap key holding config.yaml.
	CloudflaredConfigKey = "config.yaml"
	// CloudflaredSecret holds the tunnel credentials file under
	// CloudflaredSecretKey.
	CloudflaredSecret    = "cloudflared-credentials"
	CloudflaredSecretKey = "credentials.json"

	CloudflaredFile       = "manifests/cloudflared.yaml"
	CloudflaredSecretFile = "secrets/" + CloudflaredSecret + ".secret.yaml"

	// cloudflaredMetricsPort serves /ready, which the probes ask.
	cloudflaredMetricsPort = 2000
	// cloudflaredUser is the image's own non-root user (distroless nonroot).
	cloudflaredUser = 65532
	// gatewayPodPort is the gateway's listener on the relay's Pods (the
	// Service's port 80 targets it): what a NetworkPolicy judges, after
	// the Service's address is translated.
	gatewayPodPort = 8082
	// gatewayPodLabel is the label the chart puts on the gateway's Pods.
	gatewayPodLabel = "patchy.bitwisemedia.uk/preview-gateway"
	// cloudflaredConfigAnnotation carries the config's digest on the Pod
	// template, so a changed config (a new tunnel, a new host) rolls the
	// Deployment when the manifest is applied again.
	cloudflaredConfigAnnotation = "patchy.bitwisemedia.uk/cloudflared-config-sha256"
	// TunnelName is the name NEXT-STEPS.md creates the tunnel with.
	TunnelName = "patchy-previews"
)

// Cloudflare is the cloudflare-tunnel recipe's answers.
type Cloudflare struct {
	// TunnelID is the locally-managed tunnel's ID, read from its
	// credentials file (never a secret: it is the CNAME target
	// <TunnelID>.cfargotunnel.com every proxied record names).
	TunnelID string `json:"tunnelID,omitempty"`
	// Namespace is where cloudflared runs; empty is
	// DefaultCloudflaredNamespace.
	Namespace string `json:"namespace,omitempty"`
	// AdvancedCertificates says the zones holding both names have
	// Cloudflare's Advanced Certificate Manager, whose certificates may
	// cover names deeper than Universal SSL's first level.
	AdvancedCertificates bool `json:"advancedCertificates,omitempty"`
}

// CloudflaredNamespace is where cloudflared runs.
func (a Answers) CloudflaredNamespace() string {
	if a.Cloudflare.Namespace != "" {
		return a.Cloudflare.Namespace
	}
	return DefaultCloudflaredNamespace
}

// UniversalSSLCovers reports whether Cloudflare's free Universal SSL
// certificate covers host on a zone that is host's registrable domain:
// the apex and its first-level names only (design §12 [1]). A name deeper
// than that needs Advanced Certificate Manager.
func UniversalSSLCovers(host string) bool {
	site, err := Site(host)
	if err != nil {
		return false
	}
	if host == site {
		return true
	}
	rest, ok := strings.CutSuffix(host, "."+site)
	return ok && rest != "" && !strings.Contains(rest, ".")
}

// CheckCloudflareDepth holds both names the tunnel serves to what the
// edge's certificate covers (design §2.3). Every preview host is
// <label>.<suffix>, so Universal SSL covers previews only when the suffix
// is a registrable domain itself, and the sign-in host only at most one
// label below its own. acm (Advanced Certificate Manager on the zones)
// lifts the rule.
func CheckCloudflareDepth(suffix, signIn string, acm bool) error {
	if acm {
		return nil
	}
	var errs []error
	if !UniversalSSLCovers("preview." + suffix) {
		site, _ := Site(suffix)
		errs = append(errs, fmt.Errorf("--host-suffix %s is below its registrable domain %s, so every preview host "+
			"<label>.%s is a second-level name, which Cloudflare's free Universal SSL certificate does not cover: "+
			"use a domain of its own for previews (--host-suffix %s), or pass --cloudflare-acm if the zone has "+
			"Advanced Certificate Manager", suffix, site, suffix, site))
	}
	if !UniversalSSLCovers(signIn) {
		site, _ := Site(signIn)
		errs = append(errs, fmt.Errorf("--sign-in-host %s is more than one label below its registrable domain %s, "+
			"which Cloudflare's free Universal SSL certificate does not cover: use a first-level name (such as "+
			"preview-auth.%s), or pass --cloudflare-acm if the zone has Advanced Certificate Manager", signIn, site,
			site))
	}
	return errors.Join(errs...)
}

// validateCloudflare checks the cloudflare-tunnel recipe's answers, and
// refuses its answers on any other edge.
func (a Answers) validateCloudflare() []error {
	var errs []error
	c := a.Cloudflare
	if a.Edge != EdgeCloudflareTunnel {
		if c.AdvancedCertificates {
			errs = append(errs, errors.New("--cloudflare-acm is for --edge cloudflare-tunnel"))
		}
		if c.Namespace != "" {
			errs = append(errs, errors.New("--cloudflared-namespace is for --edge cloudflare-tunnel"))
		}
		return errs
	}
	if c.TunnelID != "" && !tunnelIDPattern.MatchString(c.TunnelID) {
		errs = append(errs, fmt.Errorf("the tunnel ID %q is not a lowercase UUID", c.TunnelID))
	}
	ns := a.CloudflaredNamespace()
	if v := validation.IsDNS1123Label(ns); len(v) > 0 {
		errs = append(errs, fmt.Errorf("--cloudflared-namespace %q is not a namespace name: %s", ns,
			strings.Join(v, "; ")))
	}
	if ns == a.Namespace {
		errs = append(errs, fmt.Errorf("--cloudflared-namespace %s is the release namespace: the tunnel "+
			"credentials serve every name the tunnel routes, so they never sit beside patchy's own Secrets", ns))
	} else if owner := sharedNamespace(a, ns); owner != "" {
		errs = append(errs, fmt.Errorf("--cloudflared-namespace %s is %s: the recipe's manifest relabels its "+
			"namespace for the restricted Pod Security level and rolling back deletes it, so it must be a new "+
			"namespace of its own (the default is %s)", ns, owner, DefaultCloudflaredNamespace))
	}
	// The allowlist of a tunnel is Cloudflare's: the gateway Service has no
	// load balancer to hold one, so a load balancer's answers would be
	// recorded and silently not applied.
	if a.LoadBalancer.Class != "" {
		errs = append(errs, errors.New("--load-balancer-class is for --edge gateway-tls: a tunnel's gateway "+
			"Service has no load balancer"))
	}
	if len(a.LoadBalancer.InboundCIDRs) > 0 {
		errs = append(errs, errors.New("--inbound-cidr is for --edge gateway-tls: a tunnel's previews are "+
			"reached through Cloudflare, so keep the allowlist there (a WAF custom rule, or Cloudflare Access)"))
	}
	if a.HostSuffix != "" && a.SignInHost != "" && ValidHost(a.HostSuffix) && ValidHost(a.SignInHost) {
		if err := CheckCloudflareDepth(a.HostSuffix, a.SignInHost, c.AdvancedCertificates); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// sharedNamespace names what else already lives in namespace ns, or ""
// when it is none the wizard knows of: Kubernetes' own namespaces, Dex's
// and those of the charts NEXT-STEPS.md installs. The recipe owns
// cloudflared's namespace (it enforces restricted on it, and rolling back
// deletes it), so it may be none of these.
func sharedNamespace(a Answers, ns string) string {
	switch {
	case ns == "default" || strings.HasPrefix(ns, "kube-"):
		return "one of Kubernetes' own namespaces"
	case ns == a.Dex.Namespace:
		return "Dex's namespace"
	case ns == CertManager.Namespace:
		return "cert-manager's namespace"
	case ns == TailscaleOperator.Namespace:
		return "the Tailscale operator's namespace"
	}
	return ""
}

var (
	tunnelIDPattern   = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	accountTagPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// TunnelCredentials is a locally-managed tunnel's credentials file
// (`cloudflared tunnel create` writes ~/.cloudflared/<id>.json): the
// account tag, the tunnel ID and the tunnel secret. Whoever holds it can
// serve every name the tunnel routes, so it is held only to be written
// into its Secret manifest, and its formatting is redacted.
type TunnelCredentials struct {
	raw []byte
	id  string
}

func (TunnelCredentials) String() string               { return redacted }
func (TunnelCredentials) GoString() string             { return redacted }
func (TunnelCredentials) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }
func (TunnelCredentials) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (TunnelCredentials) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// IsZero reports no credentials.
func (c TunnelCredentials) IsZero() bool { return len(c.raw) == 0 }

// TunnelID is the tunnel the credentials run: not a secret.
func (c TunnelCredentials) TunnelID() string { return c.id }

// ErrTunnelCredentials reports a tunnel credentials file the wizard
// refuses. It never quotes the file.
var ErrTunnelCredentials = errors.New("tunnel credentials")

// ParseTunnelCredentials reads a tunnel credentials file: a JSON object
// with AccountTag (32 hex digits), TunnelID (a UUID) and TunnelSecret (at
// least 32 bytes, base64), and optionally Endpoint, the file as
// cloudflared writes it. Anything else is refused, cert.pem (the
// zone-wide login certificate, which never enters the cluster) above all.
func ParseTunnelCredentials(raw []byte) (TunnelCredentials, error) {
	if len(raw) > maxCredential {
		return TunnelCredentials{}, fmt.Errorf("%w: more than %d bytes", ErrTunnelCredentials, maxCredential)
	}
	raw = bytes.TrimSpace(raw)
	if bytes.HasPrefix(raw, []byte("-----BEGIN")) {
		return TunnelCredentials{}, fmt.Errorf("%w: this is a PEM file, likely cert.pem from `cloudflared tunnel "+
			"login`, which must never enter the cluster: give ~/.cloudflared/<tunnel id>.json, which `cloudflared "+
			"tunnel create` writes", ErrTunnelCredentials)
	}
	var doc map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&doc); err != nil || dec.More() {
		return TunnelCredentials{}, fmt.Errorf("%w: not one JSON object (give ~/.cloudflared/<tunnel id>.json)",
			ErrTunnelCredentials)
	}
	str := func(key string) (string, bool) {
		var s string
		v, ok := doc[key]
		return s, ok && json.Unmarshal(v, &s) == nil
	}
	for k := range doc {
		if !slices.Contains([]string{"AccountTag", "TunnelID", "TunnelSecret", "Endpoint"}, k) {
			return TunnelCredentials{}, fmt.Errorf("%w: unknown field %q (give ~/.cloudflared/<tunnel id>.json "+
				"as cloudflared wrote it)", ErrTunnelCredentials, k)
		}
	}
	if _, ok := doc["Endpoint"]; ok {
		if _, isStr := str("Endpoint"); !isStr {
			return TunnelCredentials{}, fmt.Errorf("%w: Endpoint is not a string", ErrTunnelCredentials)
		}
	}
	if tag, ok := str("AccountTag"); !ok || !accountTagPattern.MatchString(tag) {
		return TunnelCredentials{}, fmt.Errorf("%w: AccountTag is not 32 hex digits", ErrTunnelCredentials)
	}
	id, ok := str("TunnelID")
	if !ok || !tunnelIDPattern.MatchString(id) {
		return TunnelCredentials{}, fmt.Errorf("%w: TunnelID is not a lowercase UUID", ErrTunnelCredentials)
	}
	secret, ok := str("TunnelSecret")
	if b, err := base64.StdEncoding.DecodeString(secret); !ok || err != nil || len(b) < 32 {
		return TunnelCredentials{}, fmt.Errorf("%w: TunnelSecret is not at least 32 bytes of base64",
			ErrTunnelCredentials)
	}
	return TunnelCredentials{raw: slices.Clone(raw), id: id}, nil
}

// ErrTunnelMismatch reports a credentials file on disk for another tunnel
// than answers.yaml records.
var ErrTunnelMismatch = errors.New("the tunnel credentials are for another tunnel")

// ResolveTunnelCredentials is the tunnel credentials a plan renders (design
// §4.1: a secret file is never regenerated while it exists): the file
// given this run, else the one an earlier run wrote under dir, else none.
// Credentials on disk for another tunnel than a records are refused.
func ResolveTunnelCredentials(dir string, a Answers, given TunnelCredentials) (TunnelCredentials, error) {
	if !given.IsZero() {
		if a.Edge != EdgeCloudflareTunnel {
			return TunnelCredentials{}, fmt.Errorf("%w: --tunnel-credentials is for --edge cloudflare-tunnel",
				ErrTunnelCredentials)
		}
		return given, nil
	}
	if a.Edge != EdgeCloudflareTunnel {
		return TunnelCredentials{}, nil
	}
	v, ok, err := readSecretKey(filepath.Join(dir, filepath.FromSlash(CloudflaredSecretFile)), CloudflaredSecretKey,
		"--tunnel-credentials")
	if err != nil || !ok {
		return TunnelCredentials{}, err
	}
	c, err := ParseTunnelCredentials(v)
	if err != nil {
		return TunnelCredentials{}, fmt.Errorf("%s: %w; give --tunnel-credentials again", CloudflaredSecretFile, err)
	}
	if a.Cloudflare.TunnelID != "" && c.TunnelID() != a.Cloudflare.TunnelID {
		return TunnelCredentials{}, fmt.Errorf("%w: %s runs tunnel %s, answers.yaml records %s; give "+
			"--tunnel-credentials for the tunnel you mean", ErrTunnelMismatch, CloudflaredSecretFile, c.TunnelID(),
			a.Cloudflare.TunnelID)
	}
	return c, nil
}

// tunnelSecretFile renders the tunnel credentials' Secret manifest.
func tunnelSecretFile(a Answers, s Secrets) ([]File, error) {
	if a.Edge != EdgeCloudflareTunnel {
		return nil, nil
	}
	if s.Tunnel.IsZero() {
		return nil, errors.New("no tunnel credentials to render (--tunnel-credentials)")
	}
	if s.Tunnel.TunnelID() != a.Cloudflare.TunnelID {
		return nil, fmt.Errorf("%w: the credentials run tunnel %s, the answers name %s", ErrTunnelMismatch,
			s.Tunnel.TunnelID(), a.Cloudflare.TunnelID)
	}
	data, err := ghapp.RenderSecret(CloudflaredSecret, a.CloudflaredNamespace(),
		map[string][]byte{CloudflaredSecretKey: s.Tunnel.raw})
	if err != nil {
		return nil, err
	}
	return []File{{Path: CloudflaredSecretFile, Data: data, Secret: true}}, nil
}

// cloudflareTunnelRecipe is R1 (design §4.3): cloudflared in the cluster
// connects out to Cloudflare and sends both names to the gateway's
// Service, so nothing listens on the host, no certificate lives in the
// cluster (Cloudflare's edge holds them) and no ingress controller is
// needed. Cloudflare's edge appends the viewer's address to
// X-Forwarded-For and cloudflared adds none, so forwardedHops is 1.
func cloudflareTunnelRecipe(a Answers, v values) ([]File, error) {
	gw := v.at("previewAuth", "gateway")
	gw["forwardedHops"] = forwardedHopsEdge
	// The gateway's exposure is none: no Ingress, no HTTPRoute, no
	// LoadBalancer and no TLS of its own, whatever the operator's values
	// held for another recipe.
	v.at("previewAuth", "gateway", "ingress")["enabled"] = false
	v.at("previewAuth", "gateway", "httpRoute")["parentRefs"] = []any{}
	svc := v.at("previewAuth", "gateway", "service")
	svc["type"] = "ClusterIP"
	svc["loadBalancerClass"] = ""
	v.at("previewAuth", "gateway", "tls")["secrets"] = []any{}
	// A proxied record resolves to Cloudflare's addresses, never one the
	// cluster publishes; the allowlist is Cloudflare's (NEXT-STEPS.md).
	gw["inboundCIDRs"] = []string{}
	gw["publishedAddresses"] = []string{}
	// cloudflared reaches the gateway's 8082 from its own Pod addresses,
	// which an ingressFrom narrowed to an ALB's subnets would refuse, and
	// ingressFrom takes CIDRs only: it is emptied (the relay's ports admit
	// any source, the chart's default), and NEXT-STEPS.md says so.
	v.at("previewAuth")["ingressFrom"] = []string{}
	data, err := cloudflaredManifests(a)
	if err != nil {
		return nil, err
	}
	return []File{{Path: CloudflaredFile, Data: data}}, nil
}

// cloudflaredConfig is cloudflared's config.yaml: the locally-managed
// tunnel's ingress rules, first match wins, every preview host and the
// sign-in host to the gateway's Service, then a 404 for anything else the
// tunnel is sent (design §12 [3]).
func cloudflaredConfig(a Answers) ([]byte, error) {
	origin := fmt.Sprintf("http://%s.%s.svc.cluster.local:80", a.GatewayService(), a.Namespace)
	cfg := map[string]any{
		"tunnel":           a.Cloudflare.TunnelID,
		"credentials-file": "/etc/cloudflared/credentials/" + CloudflaredSecretKey,
		"metrics":          fmt.Sprintf("0.0.0.0:%d", cloudflaredMetricsPort),
		"no-autoupdate":    true,
		"ingress": []any{
			map[string]any{"hostname": "*." + a.HostSuffix, "service": origin},
			map[string]any{"hostname": a.SignInHost, "service": origin},
			map[string]any{"service": "http_status:404"},
		},
	}
	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("render cloudflared's config: %w", err)
	}
	return out, nil
}

// cloudflaredManifests is manifests/cloudflared.yaml: cloudflared's
// namespace, ServiceAccount, config, Deployment and NetworkPolicy, in the
// order they apply.
func cloudflaredManifests(a Answers) ([]byte, error) {
	ns := a.CloudflaredNamespace()
	cfg, err := cloudflaredConfig(a)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(cfg)
	labels := map[string]any{
		"app.kubernetes.io/name":     cloudflaredName,
		"app.kubernetes.io/instance": TunnelName,
		"app.kubernetes.io/part-of":  "patchy",
	}
	meta := func() map[string]any {
		return map[string]any{"name": cloudflaredName, "namespace": ns, "labels": labels}
	}
	ready := func() map[string]any { return map[string]any{"path": "/ready", "port": "metrics"} }
	readiness := map[string]any{"httpGet": ready(), "periodSeconds": 10, "failureThreshold": 3}
	liveness := map[string]any{"httpGet": ready(), "initialDelaySeconds": 30, "periodSeconds": 10,
		"failureThreshold": 6}
	objs := []map[string]any{
		{
			"apiVersion": "v1", "kind": "Namespace",
			"metadata": map[string]any{"name": ns, "labels": map[string]any{
				"pod-security.kubernetes.io/enforce": "restricted",
			}},
		},
		{
			"apiVersion": "v1", "kind": "ServiceAccount", "metadata": meta(),
			"automountServiceAccountToken": false,
		},
		{
			"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta(),
			"data": map[string]any{CloudflaredConfigKey: string(cfg)},
		},
		{
			"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta(),
			"spec": map[string]any{
				"replicas": 2,
				"selector": map[string]any{"matchLabels": labels},
				"template": map[string]any{
					"metadata": map[string]any{
						"labels":      labels,
						"annotations": map[string]any{cloudflaredConfigAnnotation: hex.EncodeToString(sum[:])},
					},
					"spec": cloudflaredPodSpec(labels, readiness, liveness),
				},
			},
		},
		cloudflaredNetworkPolicy(a, meta(), labels),
	}
	var out []byte
	for i, o := range objs {
		body, err := yaml.Marshal(o)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", CloudflaredFile, err)
		}
		if i > 0 {
			out = append(out, "---\n"...)
		}
		out = append(out, body...)
	}
	head := []string{
		"Generated by `patchy setup previews`.",
		"cloudflared for the locally-managed tunnel " + a.Cloudflare.TunnelID + ": every preview host and the sign-in",
		"host to the preview gateway. It needs Secret " + CloudflaredSecret + " (" + CloudflaredSecretFile + "),",
		"applied after this file, which creates the namespace. Cloudflare's edge terminates TLS, so Cloudflare",
		"sees preview traffic in the clear.",
	}
	var h []byte
	for _, c := range head {
		h = append(h, "# "+c+"\n"...)
	}
	return append(h, out...), nil
}

// cloudflaredPodSpec runs cloudflared as the image's own non-root user,
// with no Kubernetes credential, a read-only root and no capability, two
// replicas spread over nodes.
func cloudflaredPodSpec(labels, readiness, liveness map[string]any) map[string]any {
	return map[string]any{
		"serviceAccountName":           cloudflaredName,
		"automountServiceAccountToken": false,
		"enableServiceLinks":           false,
		"securityContext": map[string]any{
			"runAsNonRoot": true, "runAsUser": cloudflaredUser, "runAsGroup": cloudflaredUser,
			"fsGroup": cloudflaredUser, "seccompProfile": map[string]any{"type": "RuntimeDefault"},
		},
		"affinity": map[string]any{"podAntiAffinity": map[string]any{
			"preferredDuringSchedulingIgnoredDuringExecution": []any{map[string]any{
				"weight": 100,
				"podAffinityTerm": map[string]any{
					"topologyKey":   "kubernetes.io/hostname",
					"labelSelector": map[string]any{"matchLabels": labels},
				},
			}},
		}},
		"containers": []any{map[string]any{
			"name":  cloudflaredName,
			"image": Cloudflared.Ref(),
			// The image's entrypoint is `cloudflared --no-autoupdate`.
			"args":           []string{"tunnel", "--config", "/etc/cloudflared/config/config.yaml", "run"},
			"ports":          []any{map[string]any{"name": "metrics", "containerPort": cloudflaredMetricsPort}},
			"readinessProbe": readiness,
			"livenessProbe":  liveness,
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "50m", "memory": "64Mi"},
				"limits":   map[string]any{"memory": "256Mi"},
			},
			"securityContext": map[string]any{
				"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
				"capabilities": map[string]any{"drop": []string{"ALL"}},
			},
			"volumeMounts": []any{
				map[string]any{"name": "config", "mountPath": "/etc/cloudflared/config", "readOnly": true},
				map[string]any{"name": "credentials", "mountPath": "/etc/cloudflared/credentials", "readOnly": true},
			},
		}},
		"volumes": []any{
			map[string]any{"name": "config", "configMap": map[string]any{"name": cloudflaredName}},
			map[string]any{"name": "credentials", "secret": map[string]any{
				"secretName": CloudflaredSecret, "defaultMode": 0o440,
			}},
		},
	}
}

// privateRanges are the addresses cloudflared's egress to the internet
// leaves out, so its 443 and 7844 reach public addresses only (Cloudflare's
// among them, but not only Cloudflare's: the rule names no Cloudflare
// range) and nothing in the cluster, the node network or a metadata
// service.
var (
	privateV4 = []string{"10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.168.0.0/16"}
	privateV6 = []string{"::1/128", "fc00::/7", "fe80::/10"}
)

// cloudflaredNetworkPolicy lets cloudflared reach DNS, the gateway's
// listener and any public address on 443 and 7844 (TCP and QUIC), which is
// where Cloudflare's edge answers, and nothing else, and admits only the
// kubelet's probes to its metrics port.
func cloudflaredNetworkPolicy(a Answers, meta, labels map[string]any) map[string]any {
	internet := []any{
		map[string]any{"ipBlock": map[string]any{"cidr": "0.0.0.0/0", "except": privateV4}},
		map[string]any{"ipBlock": map[string]any{"cidr": "::/0", "except": privateV6}},
	}
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": meta,
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": labels},
			"policyTypes": []string{"Ingress", "Egress"},
			"ingress": []any{map[string]any{
				"ports": []any{map[string]any{"protocol": "TCP", "port": cloudflaredMetricsPort}},
			}},
			"egress": []any{
				map[string]any{
					"to": []any{map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]any{
						"kubernetes.io/metadata.name": "kube-system",
					}}}},
					"ports": []any{map[string]any{"protocol": "UDP", "port": 53},
						map[string]any{"protocol": "TCP", "port": 53}},
				},
				map[string]any{
					"to": []any{map[string]any{
						"namespaceSelector": map[string]any{"matchLabels": map[string]any{
							"kubernetes.io/metadata.name": a.Namespace,
						}},
						"podSelector": map[string]any{"matchLabels": map[string]any{gatewayPodLabel: "true"}},
					}},
					"ports": []any{map[string]any{"protocol": "TCP", "port": gatewayPodPort}},
				},
				map[string]any{
					"to": internet,
					"ports": []any{map[string]any{"protocol": "TCP", "port": 443},
						map[string]any{"protocol": "TCP", "port": 7844}, map[string]any{"protocol": "UDP", "port": 7844}},
				},
			},
		},
	}
}
