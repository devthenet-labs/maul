// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/devthenet-labs/maul/cmd/maul/internal/previewdetect"
	"github.com/devthenet-labs/maul/cmd/maul/internal/previewsetup"
)

// maxPromptTries bounds how often one question is asked again after an
// answer it refuses.
const maxPromptTries = 3

// errPromptClosed reports standard input closing while a question waits.
var errPromptClosed = errors.New("standard input closed before every question was answered: answer with flags " +
	"instead (each question names its flag)")

// prompter asks the wizard's questions: each on stderr, naming the flag
// that answers it, so a session can be replayed with --non-interactive.
// Nothing it reads is ever a secret: credentials come from files.
type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

// question is one prompt: the flag that answers it, what it asks, the
// answers it takes (any when empty), the answer an empty line gives (none
// when empty), whether an empty answer is allowed, and a check of the
// answer.
type question struct {
	flag     string
	text     string
	choices  []string
	def      string
	optional bool
	check    func(string) error
}

// line reads one answer, trimmed.
func (p *prompter) line(prompt string) (string, error) {
	notef(p.out, "%s", prompt)
	s, err := p.in.ReadString('\n')
	switch {
	case errors.Is(err, io.EOF) && s == "":
		notef(p.out, "\n")
		return "", errUsage(errPromptClosed)
	case err != nil && !errors.Is(err, io.EOF):
		return "", fmt.Errorf("read an answer: %w", err)
	}
	return strings.TrimSpace(s), nil
}

// ask asks q until it gets an answer q takes, at most maxPromptTries times.
func (p *prompter) ask(q question) (string, error) {
	prompt := "? " + q.text + " (--" + q.flag + ")"
	if len(q.choices) > 0 {
		prompt += " [" + strings.Join(q.choices, ", ") + "]"
	}
	switch {
	case q.def != "":
		prompt += " (" + q.def + ")"
	case q.optional:
		prompt += " (blank: none)"
	}
	prompt += ": "
	for range maxPromptTries {
		s, err := p.line(prompt)
		if err != nil {
			return "", err
		}
		if s == "" {
			s = q.def
		}
		switch {
		case s == "" && q.optional:
			return "", nil
		case s == "":
			notef(p.out, "  an answer is needed.\n")
			continue
		case len(q.choices) > 0 && !slices.Contains(q.choices, s):
			notef(p.out, "  answer one of %s.\n", strings.Join(q.choices, ", "))
			continue
		}
		if q.check != nil {
			if err := q.check(s); err != nil {
				notef(p.out, "  %v\n", err)
				continue
			}
		}
		return s, nil
	}
	return "", errUsage(fmt.Errorf("no answer --%s takes after %d tries", q.flag, maxPromptTries))
}

// confirm asks a yes-or-no question, no by default.
func (p *prompter) confirm(what string) (bool, error) {
	s, err := p.line("? " + what + "? [y/N]: ")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(s, "y") || strings.EqualFold(s, "yes"), nil
}

// askPreviews asks for every answer a still lacks, in the order the flags
// are listed, with what detection found (when it ran) as defaults. It sets
// f's credential file paths from the answers to their questions; the files
// themselves are read later, as a flag's would be.
func askPreviews(p *prompter, a *previewsetup.Answers, f *setupPreviewsFlags, found *previewdetect.Findings) error {
	notef(p.out, "maul: a few questions; each names the flag that answers it, so a run can be repeated with "+
		"--non-interactive. An empty answer takes the default shown.\n")
	if a.Platform == "" {
		s, err := p.ask(question{flag: "platform", text: "the cluster", choices: previewsetup.Platforms()})
		if err != nil {
			return err
		}
		a.Platform = previewsetup.Platform(s)
	}
	if a.Edge == "" {
		s, err := p.ask(question{flag: "edge", text: "how previews reach viewers: cloudflare-tunnel (Cloudflare's " +
			"edge, no open port), gateway-tls (a load balancer), tailscale (a tailnet only), existing (an Ingress " +
			"or Gateway you run)", choices: previewsetup.Edges(), def: defaultEdge(a.Platform)})
		if err != nil {
			return err
		}
		a.Edge = previewsetup.Edge(s)
	}
	if err := askNames(p, a); err != nil {
		return err
	}
	if err := askViewers(p, a); err != nil {
		return err
	}
	if a.Registry == "" {
		s, err := p.ask(question{flag: "registry", text: "the registry host preview images come from (ghcr.io, " +
			"<account>.dkr.ecr.<region>.amazonaws.com)"})
		if err != nil {
			return err
		}
		a.Registry = strings.ToLower(s)
	}
	if a.Platform == previewsetup.PlatformOther && a.NodeIsolation.Mode == "" {
		s, err := p.ask(question{flag: "node-isolation", text: "run previews only on nodes of their own",
			choices: []string{previewsetup.IsolationRequired, previewsetup.IsolationNone},
			def:     previewsetup.IsolationRequired})
		if err != nil {
			return err
		}
		a.NodeIsolation.Mode = s
	}
	return askEdge(p, a, f, found)
}

// defaultEdge is the edge offered first: a tailnet for a home box (design
// §5.2), the cloud's own edge on EKS.
func defaultEdge(p previewsetup.Platform) string {
	switch p {
	case previewsetup.PlatformK3d, previewsetup.PlatformK3s:
		return string(previewsetup.EdgeTailscale)
	case previewsetup.PlatformEKS:
		return string(previewsetup.EdgeExisting)
	}
	return ""
}

// askNames asks for the host suffix, the sign-in host and Dex, each held
// to the same checks Validate makes.
func askNames(p *prompter, a *previewsetup.Answers) error {
	if a.HostSuffix == "" {
		s, err := p.ask(question{flag: "host-suffix", text: "previews are https://<label>.<host suffix>, on a " +
			"registrable domain of their own (acme-previews.dev)", check: func(s string) error {
			if !previewsetup.ValidHost(strings.ToLower(s)) {
				return fmt.Errorf("%q is not a lowercase DNS name of two or more labels", s)
			}
			return nil
		}})
		if err != nil {
			return err
		}
		a.HostSuffix = strings.ToLower(s)
	}
	if a.SignInHost == "" {
		s, err := p.ask(question{flag: "sign-in-host", text: "where viewers sign in, on another registrable " +
			"domain (preview-auth.maul.acme.dev)", check: func(s string) error {
			// A shared registrable domain is asked about next, on its own.
			return previewsetup.CheckHosts(a.HostSuffix, strings.ToLower(s), true)
		}})
		if err != nil {
			return err
		}
		a.SignInHost = strings.ToLower(s)
	}
	if err := askSharedDomain(p, a, a.SignInHost); err != nil {
		return err
	}
	if a.Dex.Issuer == "" {
		s, err := p.ask(question{flag: "dex-issuer", text: "your Dex's issuer URL (https://dex.maul.acme.dev)",
			check: func(s string) error { return previewsetup.CheckIssuer(s, a.HostSuffix, true) }})
		if err != nil {
			return err
		}
		a.Dex.Issuer = s
	}
	if u, err := url.Parse(a.Dex.Issuer); err == nil {
		return askSharedDomain(p, a, strings.ToLower(u.Hostname()))
	}
	return nil
}

// sharedDomainExplanation is what the wizard says before it asks for the
// acknowledgement: the residual risk, precisely (docs/configuration/
// preview-gateway.md, "Previews on the sign-in host's domain").
const sharedDomainExplanation = `  %[1]s is on %[2]s, the registrable domain the previews (<label>.%[3]s) are on, so
  preview code runs same-site, but never same-origin, with it. maul holds its own side to that: every
  cookie it sets is __Host- (no preview can set or overwrite one), its sign-in endpoints refuse a
  same-site request that is not a top-level navigation, and the status page refuses a same-site write
  (by Sec-Fetch-Site, else by Origin). What it cannot hold:
    - every other app on %[2]s: a preview can set cookies for the whole of %[2]s and send it same-site
      requests, so no app there may rely on a parent-domain cookie or on SameSite for CSRF;
    - Dex, when it is on %[2]s too: its cookies and forms are its own;
    - a browser that ignores __Host- prefixes or sends no Sec-Fetch headers (one that sends no Origin
      either could still write to a status page on %[2]s with the viewer's session).
  A domain of its own for previews avoids all of it.
`

// askSharedDomain asks for --shared-domain when host shares the suffix's
// registrable domain and the operator has not acknowledged it yet: it
// explains what that means and takes only "acknowledged". Anything else is
// a refusal naming the flags to change instead.
func askSharedDomain(p *prompter, a *previewsetup.Answers, host string) error {
	if a.SharedDomain() || !previewsetup.SharesSite(a.HostSuffix, host) {
		return nil
	}
	site, _ := previewsetup.Site(a.HostSuffix)
	notef(p.out, sharedDomainExplanation, host, site, a.HostSuffix)
	s, err := p.ask(question{flag: "shared-domain", text: "type " + previewsetup.SharedDomainAcknowledged +
		" to accept previews being same-site with " + host + ", or blank to stop and choose other names",
		optional: true, check: func(s string) error {
			if s != previewsetup.SharedDomainAcknowledged {
				return fmt.Errorf("answer %s, or leave it blank", previewsetup.SharedDomainAcknowledged)
			}
			return nil
		}})
	if err != nil {
		return err
	}
	if s == "" {
		return errUsage(fmt.Errorf("%s shares the registrable domain %s with --host-suffix %s: give another "+
			"--host-suffix (a domain of its own), --sign-in-host or --dex-issuer, or --shared-domain %s", host, site,
			a.HostSuffix, previewsetup.SharedDomainAcknowledged))
	}
	a.SharedDomainAck = s
	return nil
}

// askViewers asks who may view previews: teams as <org>:<team>, logins
// bare, or none.
func askViewers(p *prompter, a *previewsetup.Answers) error {
	if len(a.Viewers.Teams) > 0 || len(a.Viewers.Users) > 0 || a.NoViewers {
		return nil
	}
	s, err := p.ask(question{flag: "viewer-team, --viewer-user or --no-viewers", text: "who may view previews: " +
		"GitHub teams as <org>:<team-slug> and GitHub logins, comma-separated; none names nobody yet (every " +
		"sign-in is refused until you do)"})
	if err != nil {
		return err
	}
	if s == "none" {
		a.NoViewers = true
		return nil
	}
	for v := range strings.SplitSeq(s, ",") {
		switch v = strings.TrimSpace(v); {
		case v == "":
		case strings.Contains(v, ":"):
			a.Viewers.Teams = append(a.Viewers.Teams, v)
		default:
			a.Viewers.Users = append(a.Viewers.Users, v)
		}
	}
	return nil
}

// askEdge asks what the edge chosen needs: the existing edge's class or
// parent, the DNS-01 solver and its settings, and the credential files.
func askEdge(p *prompter, a *previewsetup.Answers, f *setupPreviewsFlags, found *previewdetect.Findings) error {
	if a.Edge == previewsetup.EdgeExisting && a.Platform != previewsetup.PlatformEKS &&
		a.Existing.IngressClass == "" && a.Existing.GatewayParent == "" {
		s, err := p.ask(question{flag: "ingress-class or --gateway-parent", text: "the Ingress class (traefik, " +
			"nginx, ...) or the Gateway API parent (<namespace>/<name>[/<section>]) that routes both names",
			def: detectedIngressClass(found), check: func(s string) error {
				if strings.Contains(s, "/") {
					_, _, _, err := previewsetup.ParseGatewayParent(s)
					return err
				}
				return nil
			}})
		if err != nil {
			return err
		}
		if strings.Contains(s, "/") {
			a.Existing.GatewayParent = s
		} else {
			a.Existing.IngressClass = s
		}
	}
	dnsChosen := false
	if a.GatewayTerminatesTLS() && a.Certificate.DNS == "" {
		s, err := p.ask(question{flag: "dns", text: "who answers the certificate's DNS-01 challenge (manual: " +
			"your own cert-manager issuer)", choices: previewsetup.DNSProviders()})
		if err != nil {
			return err
		}
		a.Certificate.DNS, dnsChosen = s, true
	}
	if a.WritesIssuer() {
		if err := askDNSOptions(p, a, dnsChosen); err != nil {
			return err
		}
	}
	return askCredentialFiles(p, a, f)
}

// detectedIngressClass is the IngressClass detection would offer: the
// cluster's default, else its only one.
func detectedIngressClass(found *previewdetect.Findings) string {
	if found == nil {
		return ""
	}
	for _, c := range found.IngressClasses {
		if c.Default {
			return c.Name
		}
	}
	if len(found.IngressClasses) == 1 {
		return found.IngressClasses[0].Name
	}
	return ""
}

// askDNSOptions asks for the solver's required settings it lacks and, when
// the solver was chosen at a prompt, its optional ones too (those decide,
// for one, whether a stored credential or cert-manager's own identity
// answers).
func askDNSOptions(p *prompter, a *previewsetup.Answers, all bool) error {
	for _, o := range previewsetup.DNSOptionSpecs(a.Certificate.DNS) {
		if _, set := a.Certificate.DNSOptions[o.Key]; set || (!o.Required && !all) {
			continue
		}
		s, err := p.ask(question{flag: "dns-option " + o.Key + "=", text: a.Certificate.DNS + " " + o.Key + ": " +
			o.What, optional: !o.Required, check: func(s string) error {
			if !o.Valid(s) {
				return fmt.Errorf("%q is not %s", s, o.What)
			}
			return nil
		}})
		if err != nil {
			return err
		}
		if s == "" {
			continue
		}
		if a.Certificate.DNSOptions == nil {
			a.Certificate.DNSOptions = map[string]string{}
		}
		a.Certificate.DNSOptions[o.Key] = s
	}
	return nil
}

// askCredentialFiles asks where the tunnel credentials, the Tailscale
// operator's OAuth client and the DNS-01 credential are, when the recipe needs one and neither a flag nor an
// earlier run gave it. Only the path is asked: the file is read as the
// flag's would be, and its content is never typed at a prompt.
func askCredentialFiles(p *prompter, a *previewsetup.Answers, f *setupPreviewsFlags) error {
	if a.Edge == previewsetup.EdgeCloudflareTunnel && a.Cloudflare.TunnelID == "" && f.tunnelCredentials == "" &&
		!exists(filepath.Join(f.output, filepath.FromSlash(previewsetup.CloudflaredSecretFile))) {
		s, err := p.ask(question{flag: "tunnel-credentials", text: "the locally-managed tunnel's credentials " +
			"file (cloudflared tunnel create maul-previews writes ~/.cloudflared/<tunnel id>.json)",
			check: regularFile})
		if err != nil {
			return err
		}
		f.tunnelCredentials = expandHome(s)
	}
	if a.WritesTailscaleOAuth() && f.tailscaleOAuthDir == "" &&
		!exists(filepath.Join(f.output, filepath.FromSlash(previewsetup.TailscaleOAuthFile))) {
		s, err := p.ask(question{flag: "tailscale-oauth-dir", text: "a directory holding the Tailscale operator's " +
			"OAuth client as files client_id and client_secret (later: NEXT-STEPS.md says how to make it)",
			optional: true, check: oauthDir})
		if err != nil {
			return err
		}
		f.tailscaleOAuthDir = expandHome(s)
	}
	if a.DNSCredentialKey() != "" && f.dnsCredentialsFile == "" && !f.dnsCredentialsStdin && f.dnsAWSProfile == "" &&
		!exists(filepath.Join(f.output, filepath.FromSlash(previewsetup.DNSSecretFile))) {
		s, err := p.ask(question{flag: "dns-credentials-file", text: "a file holding the " + a.Certificate.DNS +
			" DNS-01 credential (later: NEXT-STEPS.md says how to add it)", optional: true, check: regularFile})
		if err != nil {
			return err
		}
		f.dnsCredentialsFile = expandHome(s)
	}
	return nil
}

// oauthDir refuses a directory without the OAuth client's two files.
func oauthDir(s string) error {
	for _, name := range []string{previewsetup.TailscaleOAuthIDFile, previewsetup.TailscaleOAuthSecretFile} {
		if err := regularFile(filepath.Join(expandHome(s), name)); err != nil {
			return err
		}
	}
	return nil
}

// regularFile refuses a path that is not a regular file.
func regularFile(s string) error {
	info, err := os.Stat(expandHome(s))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", s)
	}
	return nil
}

// expandHome expands a leading ~/ to the home directory, as a shell would
// have for a flag.
func expandHome(s string) string {
	rest, ok := strings.CutPrefix(s, "~/")
	if !ok {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return s
	}
	return filepath.Join(home, rest)
}

// exists reports whether path exists.
func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}
