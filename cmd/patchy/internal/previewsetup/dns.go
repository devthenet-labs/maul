// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// DNSManual is --dns manual: the operator's own issuer (--certificate-issuer)
// answers the challenge, and the recipe writes no Issuer.
const DNSManual = "manual"

// The DNS-01 Issuer's names, in the release namespace beside the one
// Certificate that uses it (design §4.3 R2: a namespaced Issuer, not a
// ClusterIssuer, so no other namespace can spend the DNS credential).
const (
	IssuerFile = "manifests/issuer.yaml"
	// issuerAccountSecret holds the ACME account key cert-manager creates.
	issuerAccountSecret = "preview-dns01-account"
	// DNSSecret holds the DNS provider's credential, under the key the
	// provider's solver names (dnsProvider.secretKey).
	DNSSecret     = "preview-dns01-credentials"
	DNSSecretFile = "secrets/" + DNSSecret + ".secret.yaml"
	// ACMEServer is Let's Encrypt's production directory.
	ACMEServer = "https://acme-v02.api.letsencrypt.org/directory"
)

// dnsOption is one setting a DNS-01 solver takes, all non-secret: a
// credential never travels as an option.
type dnsOption struct {
	key      string
	required bool
	valid    func(string) bool
	// what describes a valid value, for the refusal.
	what string
}

// dnsProvider is one cert-manager DNS-01 solver the recipes write.
type dnsProvider struct {
	// solver is the field under dns01 cert-manager names it by.
	solver  string
	options []dnsOption
	// secretKey is the key the credential is stored under in DNSSecret.
	secretKey string
	// credential reports whether a solver with these options takes a
	// credential from DNSSecret: never for ambient identities (IRSA, GKE
	// workload identity, an Azure managed identity).
	credential func(opts map[string]string) bool
	// json says the credential is a JSON document (a service account key),
	// kept as it is; every other credential is one token, trimmed.
	json bool
	// spec renders the solver's body; withSecret names DNSSecret.
	spec func(opts map[string]string, withSecret bool) map[string]any
}

var (
	awsRegionPattern     = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]{1,2}$`)
	route53ZonePattern   = regexp.MustCompile(`^Z[A-Z0-9]{1,31}$`)
	awsKeyIDPattern      = regexp.MustCompile(`^AKIA[A-Z0-9]{16}$`)
	gcpProjectPattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	uuidPattern          = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)
	azureGroupPattern    = regexp.MustCompile(`^[A-Za-z0-9._()-]{0,89}[A-Za-z0-9_()-]$`)
	tsigKeyNamePattern   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}))*\.?$`)
	azureEnvironments    = []string{"AzurePublicCloud", "AzureChinaCloud", "AzureGermanCloud", "AzureUSGovernmentCloud"}
	tsigAlgorithms       = []string{"HMACSHA256", "HMACSHA512"}
	defaultTSIGAlgorithm = "HMACSHA512"
)

func secretRef(key string) map[string]any { return map[string]any{"name": DNSSecret, "key": key} }

// dnsProviders are the solvers the recipes write: cert-manager's built-in
// ones (design §12 [4]) but ACMEDNS and Akamai, which need an account
// registration flow of their own. Any other solver is --dns manual.
var dnsProviders = map[string]dnsProvider{
	"cloudflare": {
		solver: "cloudflare", secretKey: "api-token",
		credential: func(map[string]string) bool { return true },
		spec: func(_ map[string]string, withSecret bool) map[string]any {
			m := map[string]any{}
			if withSecret {
				m["apiTokenSecretRef"] = secretRef("api-token")
			}
			return m
		},
	},
	"route53": {
		solver: "route53", secretKey: "secret-access-key",
		options: []dnsOption{
			{key: "region", required: true, valid: awsRegionPattern.MatchString, what: "an AWS region"},
			{key: "hostedZoneID", valid: route53ZonePattern.MatchString,
				what: "a Route53 hosted zone ID (only when both names sit in that one zone)"},
			{key: "accessKeyID", valid: awsKeyIDPattern.MatchString,
				what: "an IAM user's access key ID (AKIA...): without it, cert-manager's own identity (IRSA, " +
					"EKS Pod Identity) answers"},
		},
		credential: func(o map[string]string) bool { return o["accessKeyID"] != "" },
		spec: func(o map[string]string, withSecret bool) map[string]any {
			m := map[string]any{"region": o["region"]}
			if z := o["hostedZoneID"]; z != "" {
				m["hostedZoneID"] = z
			}
			if id := o["accessKeyID"]; id != "" {
				m["accessKeyID"] = id
				if withSecret {
					m["secretAccessKeySecretRef"] = secretRef("secret-access-key")
				}
			}
			return m
		},
	},
	"digitalocean": {
		solver: "digitalocean", secretKey: "access-token",
		credential: func(map[string]string) bool { return true },
		spec: func(_ map[string]string, _ bool) map[string]any {
			// tokenSecretRef is required by cert-manager's schema: named
			// whether or not the wizard writes the Secret.
			return map[string]any{"tokenSecretRef": secretRef("access-token")}
		},
	},
	"google": {
		solver: "cloudDNS", secretKey: "key.json", json: true,
		options: []dnsOption{
			{key: "project", required: true, valid: gcpProjectPattern.MatchString, what: "a Google Cloud project ID"},
			{key: "serviceAccountKey", valid: func(v string) bool { return v == "true" },
				what: "true, to authenticate with a service account key (--dns-credentials-file) instead of " +
					"cert-manager's workload identity"},
		},
		credential: func(o map[string]string) bool { return o["serviceAccountKey"] == "true" },
		spec: func(o map[string]string, withSecret bool) map[string]any {
			m := map[string]any{"project": o["project"]}
			if o["serviceAccountKey"] == "true" && withSecret {
				m["serviceAccountSecretRef"] = secretRef("key.json")
			}
			return m
		},
	},
	"azure": {
		solver: "azureDNS", secretKey: "client-secret",
		options: []dnsOption{
			{key: "subscriptionID", required: true, valid: uuidPattern.MatchString, what: "a subscription ID (a UUID)"},
			{key: "resourceGroupName", required: true, valid: azureGroupPattern.MatchString,
				what: "a resource group name"},
			{key: "environment", valid: func(v string) bool { return slices.Contains(azureEnvironments, v) },
				what: "one of " + strings.Join(azureEnvironments, ", ")},
			{key: "clientID", valid: uuidPattern.MatchString,
				what: "a service principal's client ID (with tenantID); without it, a managed identity answers"},
			{key: "tenantID", valid: uuidPattern.MatchString, what: "the service principal's tenant ID (a UUID)"},
		},
		credential: func(o map[string]string) bool { return o["clientID"] != "" },
		spec: func(o map[string]string, withSecret bool) map[string]any {
			m := map[string]any{"subscriptionID": o["subscriptionID"], "resourceGroupName": o["resourceGroupName"]}
			if e := o["environment"]; e != "" {
				m["environment"] = e
			}
			if id := o["clientID"]; id != "" {
				m["clientID"] = id
				m["tenantID"] = o["tenantID"]
				if withSecret {
					m["clientSecretSecretRef"] = secretRef("client-secret")
				}
			}
			return m
		},
	},
	"rfc2136": {
		solver: "rfc2136", secretKey: "tsig-secret",
		options: []dnsOption{
			{key: "nameserver", required: true, valid: validNameserver,
				what: "the authoritative server, <address>[:<port>] or <host>[:<port>]"},
			{key: "tsigKeyName", required: true, valid: tsigKeyNamePattern.MatchString, what: "a TSIG key name"},
			{key: "tsigAlgorithm", valid: func(v string) bool { return slices.Contains(tsigAlgorithms, v) },
				what: "one of " + strings.Join(tsigAlgorithms, ", ")},
		},
		credential: func(map[string]string) bool { return true },
		spec: func(o map[string]string, withSecret bool) map[string]any {
			algo := o["tsigAlgorithm"]
			if algo == "" {
				algo = defaultTSIGAlgorithm
			}
			m := map[string]any{"nameserver": o["nameserver"], "tsigKeyName": o["tsigKeyName"],
				"tsigAlgorithm": algo}
			if withSecret {
				m["tsigSecretSecretRef"] = secretRef("tsig-secret")
			}
			return m
		},
	},
}

// DNSProviders lists every --dns value, in the order the help names them.
func DNSProviders() []string {
	return []string{"route53", "cloudflare", "google", "azure", "digitalocean", "rfc2136", DNSManual}
}

// DNSOptionKeys lists the --dns-option keys a provider takes, in order.
func DNSOptionKeys(provider string) []string {
	out := make([]string, 0, len(dnsProviders[provider].options))
	for _, o := range dnsProviders[provider].options {
		out = append(out, o.key)
	}
	return out
}

// validNameserver is an rfc2136 nameserver: an address or a DNS name,
// with an optional port.
func validNameserver(v string) bool {
	host := v
	if h, port, err := net.SplitHostPort(v); err == nil {
		if n, perr := netip.ParseAddrPort("0.0.0.0:" + port); perr != nil || n.Port() == 0 {
			return false
		}
		host = h
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return ValidHost(host)
}

// DNSCredentialKey is the key the DNS credential is stored under for a's
// solver, "" when it takes none (DNSManual, an ambient identity, or a
// recipe that writes no Issuer).
func (a Answers) DNSCredentialKey() string {
	if !a.WritesIssuer() {
		return ""
	}
	p, ok := dnsProviders[a.Certificate.DNS]
	if !ok || !p.credential(a.Certificate.DNSOptions) {
		return ""
	}
	return p.secretKey
}

// DNSCredentialIsJSON reports a credential kept as a JSON document.
func (a Answers) DNSCredentialIsJSON() bool { return dnsProviders[a.Certificate.DNS].json }

// validateCertificate checks how the certificate is issued: the issuer's
// grammar on every recipe, and the DNS-01 solver on the recipes that write
// one.
func (a Answers) validateCertificate() []error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	c := a.Certificate
	if c.Issuer != "" {
		kind, name, ok := strings.Cut(c.Issuer, "/")
		if !ok || (kind != "Issuer" && kind != "ClusterIssuer") || !validSubdomain(name) {
			bad("--certificate-issuer %q is not Issuer/<name> or ClusterIssuer/<name>", c.Issuer)
		}
	}
	if !a.GatewayTerminatesTLS() {
		return errs
	}
	if c.DNS == "" {
		return errs // Missing names it
	}
	if !slices.Contains(DNSProviders(), c.DNS) {
		bad("--dns %q is none of %s", c.DNS, strings.Join(DNSProviders(), ", "))
		return errs
	}
	if c.DNS == DNSManual {
		if len(c.DNSOptions) > 0 {
			bad("--dns-option with --dns manual: your own issuer's solver takes its settings itself")
		}
		if c.ACMEEmail != "" {
			bad("--acme-email with --dns manual: your own issuer holds its ACME account")
		}
		return errs
	}
	if c.Issuer != "" {
		bad("--certificate-issuer with --dns %s: the recipe writes the Issuer %s itself; use --dns manual to "+
			"name your own", c.DNS, DefaultCertificateIssuer)
	}
	if c.ACMEEmail != "" && !validEmail(c.ACMEEmail) {
		bad("--acme-email %q is not a bare address (name@domain)", c.ACMEEmail)
	}
	return append(errs, c.validateDNSOptions()...)
}

// validateDNSOptions checks the solver's options: every required one
// given, every one given valid and known to the solver.
func (c Certificate) validateDNSOptions() []error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	p := dnsProviders[c.DNS]
	known := map[string]bool{}
	for _, o := range p.options {
		known[o.key] = true
		v, set := c.DNSOptions[o.key]
		switch {
		case !set && o.required:
			bad("--dns %s needs --dns-option %s=<%s>", c.DNS, o.key, o.what)
		case set && !o.valid(v):
			bad("--dns-option %s=%q is not %s", o.key, v, o.what)
		}
	}
	for _, k := range sortedKeys(c.DNSOptions) {
		if !known[k] {
			want := "none"
			if keys := DNSOptionKeys(c.DNS); len(keys) > 0 {
				want = strings.Join(keys, ", ")
			}
			bad("--dns-option %s: --dns %s takes %s", k, c.DNS, want)
		}
	}
	if c.DNS == "azure" && (c.DNSOptions["clientID"] == "") != (c.DNSOptions["tenantID"] == "") {
		bad("--dns azure: give clientID and tenantID together (a service principal), or neither (a managed identity)")
	}
	return errs
}

// emailPattern is a bare address: no display name, no comments, one @.
var emailPattern = regexp.MustCompile(`^[A-Za-z0-9.!#$%&'*+/=?^_{|}~-]{1,64}@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+$`)

func validEmail(s string) bool { return len(s) <= 254 && emailPattern.MatchString(s) }

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// issuer is the namespaced ACME Issuer the gateway-tls and tailscale
// recipes write: Let's Encrypt, one DNS-01 solver, its credential (when it
// takes one) in DNSSecret beside it.
func issuer(a Answers) ([]byte, error) {
	p := dnsProviders[a.Certificate.DNS]
	_, name := a.Issuer()
	acme := map[string]any{
		"server":              ACMEServer,
		"privateKeySecretRef": map[string]any{"name": issuerAccountSecret},
		"solvers": []any{map[string]any{"dns01": map[string]any{
			p.solver: p.spec(a.Certificate.DNSOptions, a.DNSCredentialKey() != ""),
		}}},
	}
	if a.Certificate.ACMEEmail != "" {
		acme["email"] = a.Certificate.ACMEEmail
	}
	comment := []string{"The ACME Issuer the wildcard Certificate names: namespaced, so only this namespace's " +
		"Certificates can", "spend the DNS credential. A wildcard needs DNS-01."}
	if a.DNSCredentialKey() != "" {
		comment = append(comment, fmt.Sprintf("Its credential is Secret %s, key %s.", DNSSecret, a.DNSCredentialKey()))
	}
	return manifest(map[string]any{
		"apiVersion": "cert-manager.io/v1",
		"kind":       "Issuer",
		"metadata":   map[string]any{"name": name, "namespace": a.Namespace},
		"spec":       map[string]any{"acme": acme},
	}, comment...)
}
