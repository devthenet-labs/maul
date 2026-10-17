// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
	"github.com/bitwise-media-group/patchy/internal/version"
)

// defaultPreviewsDir is where `setup previews` writes when -o is not given.
const defaultPreviewsDir = "patchy-previews"

// setupPreviewsFlags are `setup previews`' flags beyond the answers.
type setupPreviewsFlags struct {
	output      string
	force       bool
	dryRun      bool
	rotate      string
	nodeLabel   string
	eksAutoMode string
	dnsOptions  []string
	// dnsCredentialsFile and dnsCredentialsStdin are where the DNS-01
	// credential is read from: never a flag value, which would land in
	// shell history.
	dnsCredentialsFile  string
	dnsCredentialsStdin bool
	// tunnelCredentials is the locally-managed Cloudflare tunnel's
	// credentials file (~/.cloudflared/<tunnel id>.json).
	tunnelCredentials string
	// detect reads the cluster (read-only, never a Secret) to fill in
	// answers; off, the command never contacts a cluster.
	detect bool
	// nonInteractive asks nothing even on a terminal; yes accepts the
	// changes shown without asking, as --force does.
	nonInteractive bool
	yes            bool
}

// newSetupPreviewsCmd is `setup previews` (design
// .claude/plans/preview-setup-design.md §4): flags are the contract, and on
// a terminal it asks for whatever they leave unanswered.
func newSetupPreviewsCmd(opts *Options) *cobra.Command {
	f := &setupPreviewsFlags{}
	a := previewsetup.Defaults()
	cmd := &cobra.Command{
		Use:   "previews",
		Short: "Write the files that expose previews and sign viewers in to them",
		Long: "Write the files that expose patchy's previews at https://<project>-<issue>.<host suffix>\n" +
			"and sign viewers in to them: patchy's chart values, the manifests the edge needs, the\n" +
			"relay's Dex client secret as two Secret manifests, answers.yaml and NEXT-STEPS.md, all\n" +
			"in -o (default ./patchy-previews, created 0700, every file 0600). Nothing is applied and\n" +
			"no provider is contacted: NEXT-STEPS.md lists the commands, in order.\n\n" +
			"Every answer is a flag. On a terminal, the command asks for each one the flags leave\n" +
			"unanswered, naming the flag that answers it, so a session can be repeated with\n" +
			"--non-interactive; without a terminal (or with --non-interactive) a missing answer is\n" +
			"an error naming its flag. A credential is never typed at a prompt: only the path of the\n" +
			"file that holds it.\n\n" +
			"No cluster is contacted unless --detect is given. --detect reads the cluster your\n" +
			"kubeconfig names, read-only and never a Secret: the nodes (the platform, and which carry\n" +
			"the preview label and taint), the kubernetes Service and the API server's EndpointSlice,\n" +
			"the kube-dns Service, IngressClasses, GatewayClasses, the cert-manager and Tailscale\n" +
			"operator CRDs, kube-system's Pod images (what enforces NetworkPolicy) and the release's\n" +
			"ConfigMaps in -n (an install's existing preview settings). It prints what it found, and\n" +
			"fills in only answers neither a flag nor an earlier run gave: the platform, the host\n" +
			"suffix, sign-in host and Dex issuer of an existing install, the kubernetes Service's /32\n" +
			"as --api-server-cidr on EKS, and --cert-manager-installed and\n" +
			"--tailscale-operator-installed.\n\n" +
			"--edge existing routes through\n" +
			"an Ingress class or a Gateway API parent already in the cluster; --edge gateway-tls makes\n" +
			"the gateway's Service a LoadBalancer that\n" +
			"terminates TLS itself; --edge tailscale gives that Service a tailnet device through the\n" +
			"Tailscale operator. The last two write a cert-manager Certificate and, unless --dns manual,\n" +
			"the ACME Issuer that answers its DNS-01 challenge (a ClusterIssuer when cert-manager's own\n" +
			"identity answers, with no stored credential), plus values for cert-manager\n" +
			"and the Tailscale operator at the chart versions patchy pins. A DNS credential is read\n" +
			"from --dns-credentials-file or --dns-credentials-from-stdin, never a flag value, and is\n" +
			"written only into a Secret manifest.\n\n" +
			"--edge cloudflare-tunnel runs cloudflared in a namespace of its own, sending both names to\n" +
			"the gateway's Service through a locally-managed tunnel; Cloudflare's edge holds the\n" +
			"certificates. Create the tunnel first (cloudflared tunnel login, then cloudflared tunnel\n" +
			"create patchy-previews) and give its credentials file to --tunnel-credentials: it is\n" +
			"written only into a Secret manifest in cloudflared's namespace (cert.pem is refused, and\n" +
			"never leaves your workstation). Cloudflare's free Universal SSL covers a zone's first-level\n" +
			"names only, so the --host-suffix must be a registrable domain itself and the\n" +
			"--sign-in-host at most one label below its own, unless --cloudflare-acm says the zones\n" +
			"have Advanced Certificate Manager.\n\n" +
			"Previews must sit on a registrable domain of their own: a --sign-in-host or --dex-issuer\n" +
			"on the --host-suffix's registrable domain is refused, because preview code would run\n" +
			"same-site with sign-in. With only one domain, register a second, cheap one for previews.\n\n" +
			"A re-run starts from answers.yaml; flags given override it. It shows how each file would\n" +
			"change and writes nothing changed without --force or -y (on a terminal it asks). The\n" +
			"Dex client secret is read back\n" +
			"from the Secret files, never drawn again, unless --rotate dex-client-secret. --dry-run\n" +
			"prints the files and their differences (never a secret) and writes nothing.",
		Example: "  patchy setup previews --detect -n patchy\n\n" +
			"  patchy setup previews --platform k3s --edge existing --ingress-class traefik \\\n" +
			"    --host-suffix acme-previews.dev --sign-in-host preview-auth.patchy.acme.dev \\\n" +
			"    --dex-issuer https://dex.patchy.acme.dev --viewer-team acme:qa --registry ghcr.io \\\n" +
			"    --image-path-prefix acme/patchy/previews\n\n" +
			"  patchy setup previews --platform k3d --edge tailscale --dns route53 \\\n" +
			"    --dns-option region=us-east-1 --dns-option accessKeyID=AKIAIOSFODNN7EXAMPLE \\\n" +
			"    --dns-credentials-file ~/.config/dns01-secret-access-key ...\n\n" +
			"  patchy setup previews --platform k3s --edge cloudflare-tunnel \\\n" +
			"    --tunnel-credentials ~/.cloudflared/<tunnel id>.json --host-suffix acme-previews.dev \\\n" +
			"    --sign-in-host preview-auth.acme.dev ...",
		Args:              cobra.NoArgs,
		ValidArgsFunction: noFileCompletion,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSetupPreviews(cmd, opts, f, a)
		},
	}
	fl := cmd.Flags()
	fl.StringVar((*string)(&a.Platform), "platform", "", "the cluster: "+strings.Join(previewsetup.Platforms(), ", "))
	fl.StringVar((*string)(&a.Edge), "edge", "", "how previews reach viewers: "+strings.Join(previewsetup.Edges(), ", "))
	fl.StringVar(&a.HostSuffix, "host-suffix", "", "previews are https://<label>.<host suffix>: a domain of their own")
	fl.StringVar(&a.SignInHost, "sign-in-host", "", "where viewers sign in, on another registrable domain")
	fl.StringVar(&a.Dex.Issuer, "dex-issuer", "", "your Dex's issuer URL (https)")
	fl.StringVar(&a.Dex.Namespace, "dex-namespace", a.Dex.Namespace, "the namespace Dex runs in")
	fl.StringVar(&a.Dex.ClientID, "dex-client-id", a.Dex.ClientID, "the relay's static client at Dex")
	fl.StringArrayVar(&a.Viewers.Teams, "viewer-team", nil, "a Dex GitHub group that may view previews, "+
		"<org>:<team-slug> (repeatable)")
	fl.StringArrayVar(&a.Viewers.Users, "viewer-user", nil, "a GitHub login that may view previews (repeatable)")
	fl.BoolVar(&a.NoViewers, "no-viewers", false, "name no viewer yet: every sign-in is refused until you do")
	fl.StringVar(&a.Registry, "registry", "", "the registry host preview images come from")
	fl.StringVar(&a.ImagePathPrefix, "image-path-prefix", a.ImagePathPrefix,
		"the registry path preview images sit directly under")
	fl.StringVar(&a.NodeIsolation.Mode, "node-isolation", "",
		"required or none (default: required, except on --platform other, which must say)")
	fl.StringVar(&f.nodeLabel, "node-label", a.NodeIsolation.Label+"="+a.NodeIsolation.LabelValue,
		"the label preview nodes carry, key=value")
	fl.StringVar(&f.eksAutoMode, "eks-auto-mode", "", "--platform eks: the EKS Auto Mode NodePool and NodeClass "+
		"preview nodes come from, <nodePool>/<nodeClass> (instead of --node-label)")
	fl.StringVar(&a.NodeIsolation.TaintKey, "taint-key", a.NodeIsolation.TaintKey,
		"the NoExecute taint preview nodes carry")
	fl.StringVar(&a.APIServerCIDR, "api-server-cidr", a.APIServerCIDR,
		"the kubernetes Service's /32 the preview-controller may reach, or auto (a render-time lookup)")
	fl.StringVar(&a.Existing.IngressClass, "ingress-class", "", "--edge existing: the Ingress class that routes "+
		"both names (traefik, nginx, alb, ...)")
	fl.StringVar(&a.Existing.GatewayParent, "gateway-parent", "", "--edge existing: the Gateway API parent "+
		"that routes both names, <namespace>/<name>[/<section>]")
	fl.StringVar(&a.Existing.TLS, "edge-tls", "", "--edge existing: certificate (a cert-manager Certificate in "+
		"the release namespace) or edge (the edge holds it); default from the edge")
	fl.StringVar(&a.Certificate.Issuer, "certificate-issuer", "", "--edge existing, or --dns manual: your "+
		"cert-manager issuer, Issuer/<name> or ClusterIssuer/<name> (default "+previewsetup.DefaultCertificateIssuer+")")
	fl.StringVar(&a.Certificate.DNS, "dns", "", "--edge gateway-tls or tailscale: the DNS-01 solver of the Issuer "+
		"written: "+strings.Join(previewsetup.DNSProviders(), ", ")+" (manual: your own --certificate-issuer)")
	fl.StringArrayVar(&f.dnsOptions, "dns-option", nil, "a setting of the DNS-01 solver, key=value, never a "+
		"secret (repeatable; route53: region, accessKeyID; google: project, serviceAccountKey; "+
		"azure: subscriptionID, resourceGroupName, environment, clientID, tenantID; rfc2136: nameserver, "+
		"tsigKeyName, tsigAlgorithm)")
	fl.StringVar(&f.dnsCredentialsFile, "dns-credentials-file", "", "a file holding the DNS-01 solver's "+
		"credential (a token, a secret key, or google's JSON key), written only into a Secret manifest")
	fl.BoolVar(&f.dnsCredentialsStdin, "dns-credentials-from-stdin", false, "read the DNS-01 solver's "+
		"credential from standard input instead")
	fl.StringVar(&a.Certificate.ACMEEmail, "acme-email", "", "the ACME account's contact address (optional)")
	fl.BoolVar(&a.Certificate.CertManagerInstalled, "cert-manager-installed", false, "cert-manager already runs: "+
		"write no values for it")
	fl.StringVar(&a.LoadBalancer.Class, "load-balancer-class", "", "--edge gateway-tls: the gateway Service's "+
		"loadBalancerClass, where the cluster has more than one implementation")
	fl.StringArrayVar(&a.LoadBalancer.InboundCIDRs, "inbound-cidr", nil, "--edge gateway-tls: a client CIDR "+
		"that may open previews, kept at the load balancer (repeatable; default all)")
	fl.StringVar(&a.Tailscale.Hostname, "tailscale-hostname", "", "--edge tailscale: the gateway's device name "+
		"(default "+previewsetup.DefaultTailscaleHostname+")")
	fl.StringVar(&a.Tailscale.Tag, "tailscale-tag", "", "--edge tailscale: the device's ACL tag (default "+
		previewsetup.DefaultTailscaleTag+")")
	fl.StringArrayVar(&a.Tailscale.Addresses, "tailscale-address", nil, "--edge tailscale: the device's tailnet "+
		"address once it has appeared, what the DNS records hold (repeatable)")
	fl.BoolVar(&a.Tailscale.OperatorInstalled, "tailscale-operator-installed", false, "--edge tailscale: the "+
		"Tailscale operator already runs: write no values for it")
	fl.StringVar(&f.tunnelCredentials, "tunnel-credentials", "", "--edge cloudflare-tunnel: the locally-managed "+
		"tunnel's credentials file (~/.cloudflared/<tunnel id>.json), written only into a Secret manifest")
	fl.BoolVar(&a.Cloudflare.AdvancedCertificates, "cloudflare-acm", false, "--edge cloudflare-tunnel: the zones "+
		"have Advanced Certificate Manager, so names deeper than Universal SSL's first level are allowed")
	fl.StringVar(&a.Cloudflare.Namespace, "cloudflared-namespace", "", "--edge cloudflare-tunnel: the namespace "+
		"cloudflared runs in, a new one its manifest creates and rolling back deletes (default "+
		previewsetup.DefaultCloudflaredNamespace+")")
	fl.StringVar(&a.Release, "release", a.Release, "the patchy Helm release")
	// -o names a directory here, not an output format.
	fl.StringVarP(&f.output, "output", "o", defaultPreviewsDir, "the directory to write to")
	fl.BoolVar(&f.force, "force", false, "replace files that would change")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print the files and their differences; write nothing")
	fl.StringVar(&f.rotate, "rotate", "", "draw a new secret although one is written: "+
		previewsetup.RotateDexClientSecret)
	fl.BoolVar(&f.detect, "detect", false, "read the cluster (read-only, never a Secret) to fill in answers")
	fl.BoolVar(&f.nonInteractive, "non-interactive", false, "ask nothing, even on a terminal: a missing answer "+
		"is an error")
	fl.BoolVarP(&f.yes, "yes", "y", false, "write the changes shown without asking (as --force)")
	_ = cmd.RegisterFlagCompletionFunc("platform", fixedCompletion(previewsetup.Platforms()))
	_ = cmd.RegisterFlagCompletionFunc("edge", fixedCompletion(previewsetup.Edges()))
	_ = cmd.RegisterFlagCompletionFunc("node-isolation", fixedCompletion([]string{
		previewsetup.IsolationRequired, previewsetup.IsolationNone}))
	_ = cmd.RegisterFlagCompletionFunc("edge-tls", fixedCompletion([]string{
		previewsetup.TLSCertificate, previewsetup.TLSEdge}))
	_ = cmd.RegisterFlagCompletionFunc("rotate", fixedCompletion([]string{previewsetup.RotateDexClientSecret}))
	_ = cmd.RegisterFlagCompletionFunc("dns", fixedCompletion(previewsetup.DNSProviders()))
	for _, name := range []string{"host-suffix", "sign-in-host", "dex-issuer", "dex-namespace", "dex-client-id",
		"viewer-team", "viewer-user", "registry", "image-path-prefix", "node-label", "eks-auto-mode", "taint-key",
		"api-server-cidr", "ingress-class", "gateway-parent", "certificate-issuer", "release", "dns-option",
		"acme-email", "load-balancer-class", "inbound-cidr", "tailscale-hostname", "tailscale-tag",
		"tailscale-address", "cloudflared-namespace"} {
		_ = cmd.RegisterFlagCompletionFunc(name, noFileCompletion)
	}
	return cmd
}

// answerFlags maps each answer flag to the field it sets, so a re-run can
// lay only the flags given over answers.yaml.
func answerFlags(dst *previewsetup.Answers, src previewsetup.Answers) map[string]func() {
	return map[string]func(){
		"platform": func() { dst.Platform = src.Platform },
		// Leaving the tunnel drops its answers, which no other edge takes
		// (a flag of its given with another edge is refused before this).
		"edge": func() {
			if src.Edge != previewsetup.EdgeCloudflareTunnel {
				dst.Cloudflare = previewsetup.Cloudflare{}
			}
			dst.Edge = src.Edge
		},
		"host-suffix":       func() { dst.HostSuffix = src.HostSuffix },
		"sign-in-host":      func() { dst.SignInHost = src.SignInHost },
		"dex-issuer":        func() { dst.Dex.Issuer = src.Dex.Issuer },
		"dex-namespace":     func() { dst.Dex.Namespace = src.Dex.Namespace },
		"dex-client-id":     func() { dst.Dex.ClientID = src.Dex.ClientID },
		"viewer-team":       func() { dst.Viewers.Teams, dst.NoViewers = src.Viewers.Teams, false },
		"viewer-user":       func() { dst.Viewers.Users, dst.NoViewers = src.Viewers.Users, false },
		"no-viewers":        func() { dst.NoViewers, dst.Viewers = src.NoViewers, previewsetup.Viewers{} },
		"registry":          func() { dst.Registry = src.Registry },
		"image-path-prefix": func() { dst.ImagePathPrefix = src.ImagePathPrefix },
		"node-isolation":    func() { dst.NodeIsolation.Mode = src.NodeIsolation.Mode },
		"node-label": func() {
			dst.NodeIsolation.Label, dst.NodeIsolation.LabelValue = src.NodeIsolation.Label, src.NodeIsolation.LabelValue
		},
		"eks-auto-mode":   func() { dst.NodeIsolation.EKSAutoMode = src.NodeIsolation.EKSAutoMode },
		"taint-key":       func() { dst.NodeIsolation.TaintKey = src.NodeIsolation.TaintKey },
		"api-server-cidr": func() { dst.APIServerCIDR = src.APIServerCIDR },
		"ingress-class": func() {
			dst.Existing.IngressClass, dst.Existing.GatewayParent = src.Existing.IngressClass, ""
		},
		"gateway-parent": func() {
			dst.Existing.GatewayParent, dst.Existing.IngressClass = src.Existing.GatewayParent, ""
		},
		"edge-tls":           func() { dst.Existing.TLS = src.Existing.TLS },
		"certificate-issuer": func() { dst.Certificate.Issuer = src.Certificate.Issuer },
		"release":            func() { dst.Release = src.Release },
		// Another solver takes other options: the ones answered for the
		// last are dropped (dns-option, given too, sorts after it).
		"dns": func() {
			if dst.Certificate.DNS != src.Certificate.DNS {
				dst.Certificate.DNSOptions = nil
			}
			dst.Certificate.DNS = src.Certificate.DNS
		},
		"dns-option":             func() { dst.Certificate.DNSOptions = src.Certificate.DNSOptions },
		"acme-email":             func() { dst.Certificate.ACMEEmail = src.Certificate.ACMEEmail },
		"cert-manager-installed": func() { dst.Certificate.CertManagerInstalled = src.Certificate.CertManagerInstalled },
		"load-balancer-class":    func() { dst.LoadBalancer.Class = src.LoadBalancer.Class },
		"inbound-cidr":           func() { dst.LoadBalancer.InboundCIDRs = src.LoadBalancer.InboundCIDRs },
		"tailscale-hostname":     func() { dst.Tailscale.Hostname = src.Tailscale.Hostname },
		"tailscale-tag":          func() { dst.Tailscale.Tag = src.Tailscale.Tag },
		"tailscale-address":      func() { dst.Tailscale.Addresses = src.Tailscale.Addresses },
		"tailscale-operator-installed": func() {
			dst.Tailscale.OperatorInstalled = src.Tailscale.OperatorInstalled
		},
		"cloudflare-acm":        func() { dst.Cloudflare.AdvancedCertificates = src.Cloudflare.AdvancedCertificates },
		"cloudflared-namespace": func() { dst.Cloudflare.Namespace = src.Cloudflare.Namespace },
	}
}

// cloudflareFlags are the flags only --edge cloudflare-tunnel takes.
var cloudflareFlags = []string{"tunnel-credentials", "cloudflare-acm", "cloudflared-namespace"}

// exclusiveAnswerFlags are the answer flags that may not be given
// together: each one's setter clears the other's answer, so on a re-run
// the last to run would win silently.
var exclusiveAnswerFlags = [][2]string{
	{"ingress-class", "gateway-parent"},
	{"dns-credentials-file", "dns-credentials-from-stdin"},
	{"no-viewers", "viewer-team"},
	{"no-viewers", "viewer-user"},
}

// resolveAnswers is the run's answers: answers.yaml under dir when there
// is one, with the flags given laid over it; else the flags over the
// defaults.
func resolveAnswers(cmd *cobra.Command, opts *Options, f *setupPreviewsFlags,
	flags previewsetup.Answers) (previewsetup.Answers, bool, error) {
	for _, pair := range exclusiveAnswerFlags {
		if cmd.Flags().Changed(pair[0]) && cmd.Flags().Changed(pair[1]) {
			return flags, false, errUsage(fmt.Errorf("--%s and --%s are exclusive", pair[0], pair[1]))
		}
	}
	eks, err := previewsetup.ParseEKSAutoMode(f.eksAutoMode)
	if err != nil {
		return flags, false, errUsage(err)
	}
	flags.NodeIsolation.EKSAutoMode = eks
	dnsOptions, err := parseDNSOptions(f.dnsOptions)
	if err != nil {
		return flags, false, errUsage(err)
	}
	flags.Certificate.DNSOptions = dnsOptions
	if f.nodeLabel != "" {
		key, value, ok := strings.Cut(f.nodeLabel, "=")
		if !ok {
			return flags, false, errUsage(fmt.Errorf("--node-label %q is not key=value", f.nodeLabel))
		}
		flags.NodeIsolation.Label, flags.NodeIsolation.LabelValue = key, value
	}
	if opts.Namespace != "" {
		flags.Namespace = opts.Namespace
	}
	path := filepath.Join(f.output, previewsetup.AnswersFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return flags, false, nil
	}
	if err != nil {
		return flags, false, err
	}
	a, err := previewsetup.ParseAnswers(data)
	if err != nil {
		return flags, false, fmt.Errorf("%s: %w; fix it, or move it away to start afresh", path, err)
	}
	setters := answerFlags(&a, flags)
	// In a fixed order, though no two flags given may now touch one answer.
	for _, name := range slices.Sorted(maps.Keys(setters)) {
		if cmd.Flags().Changed(name) {
			setters[name]()
		}
	}
	if cmd.Flags().Changed("namespace") {
		a.Namespace = flags.Namespace
	}
	return a, true, nil
}

// runSetupPreviews resolves the answers, plans the files and writes them.
func runSetupPreviews(cmd *cobra.Command, opts *Options, f *setupPreviewsFlags, flags previewsetup.Answers) error {
	if f.rotate != "" && f.rotate != previewsetup.RotateDexClientSecret {
		return errUsage(fmt.Errorf("--rotate %q: the one secret to rotate is %s", f.rotate,
			previewsetup.RotateDexClientSecret))
	}
	a, rerun, err := resolveAnswers(cmd, opts, f, flags)
	if err != nil {
		return err
	}
	a.Normalize()
	p, tunnel, err := completeAnswers(cmd, opts, f, &a)
	if err != nil {
		return err
	}
	given, err := readDNSCredential(cmd.InOrStdin(), opts.ErrOut, f, a)
	if err != nil {
		return err
	}
	rotate := f.rotate != ""
	secrets, err := previewsetup.ResolveSecrets(f.output, rotate, rand.Reader)
	if err != nil {
		return err
	}
	if secrets.DNS, err = previewsetup.ResolveDNSCredential(f.output, a, given); err != nil {
		return errUsage(err)
	}
	secrets.Tunnel = tunnel
	files, err := previewsetup.Plan(a, secrets, version.Version)
	if err != nil {
		return err
	}
	c, err := previewsetup.Compare(f.output, files)
	if err != nil {
		return err
	}
	if f.dryRun {
		printComparison(opts.Out, c, true)
		notef(opts.ErrOut, "patchy: dry run: nothing written to %s.\n", f.output)
		return nil
	}
	// A secret file changes only when the operator asked for it: a
	// rotation, or a DNS credential or tunnel credentials given anew.
	accept := previewsetup.Accept{All: f.force || f.yes,
		Secrets: rotate || !given.IsZero() || f.tunnelCredentials != ""}
	written, err := writePreviews(opts.Out, p, c, accept)
	if err != nil {
		return err
	}
	reportWrite(opts.ErrOut, f.output, c, written, secrets, rerun)
	return nil
}

// completeAnswers finishes a's answers: detection when asked for, the
// prompts on a terminal (the returned prompter, nil off one), the tunnel
// credentials, and then every answer present and valid.
func completeAnswers(cmd *cobra.Command, opts *Options, f *setupPreviewsFlags, a *previewsetup.Answers) (
	*prompter, previewsetup.TunnelCredentials, error) {
	var p *prompter
	if !f.nonInteractive && !f.dnsCredentialsStdin && opts.previewsTerminal(cmd.InOrStdin()) {
		p = &prompter{in: bufio.NewReader(cmd.InOrStdin()), out: opts.ErrOut}
	}
	if err := detectAndAsk(cmd, opts, f, a, p); err != nil {
		return p, previewsetup.TunnelCredentials{}, err
	}
	tunnel, err := resolveTunnel(cmd, opts.ErrOut, f, a)
	if err != nil {
		return p, tunnel, err
	}
	if m := a.Missing(); len(m) > 0 {
		how := "run it in a terminal to be asked, or give them as flags"
		if p != nil {
			how = "give them as flags"
		}
		return p, tunnel, errUsage(fmt.Errorf("answer %s (%s)", strings.Join(m, ", "), how))
	}
	if err := a.Validate(); err != nil {
		return p, tunnel, errUsage(err)
	}
	if !previewsetup.Ready(a.Edge) {
		return p, tunnel, errUsage(fmt.Errorf("--edge %s: %w", a.Edge, previewsetup.ErrRecipeNotReady))
	}
	return p, tunnel, nil
}

// writePreviews writes the comparison as accept allows. A changed file
// not accepted lists every change on out, and on a terminal (p) asks
// whether to write them all, no by default.
func writePreviews(out io.Writer, p *prompter, c previewsetup.Comparison, accept previewsetup.Accept) ([]string,
	error) {
	written, err := previewsetup.Write(c, accept)
	var conflict *previewsetup.ConflictError
	if !errors.As(err, &conflict) {
		return written, err
	}
	printComparison(out, c, false)
	if p == nil {
		return nil, err
	}
	ok, aerr := p.confirm("write these changes")
	if aerr != nil || !ok {
		return nil, errors.Join(err, aerr)
	}
	accept.All = true
	return previewsetup.Write(c, accept)
}

// parseDNSOptions turns --dns-option key=value pairs into the solver's
// settings; a key given twice is refused rather than one silently winning.
func parseDNSOptions(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	opts := make(map[string]string, len(pairs))
	for _, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch _, dup := opts[k]; {
		case !ok || k == "" || v == "":
			return nil, fmt.Errorf("--dns-option %q is not key=value", kv)
		case dup:
			return nil, fmt.Errorf("--dns-option %s given twice", k)
		}
		opts[k] = v
	}
	return opts, nil
}

// readDNSCredential reads the DNS-01 credential from the file or standard
// input named, bounded, and warns when the file is readable by others. It
// is never echoed, and nothing is read when neither is given.
func readDNSCredential(in io.Reader, errOut io.Writer, f *setupPreviewsFlags,
	a previewsetup.Answers) (previewsetup.Credential, error) {
	var raw []byte
	switch {
	case f.dnsCredentialsStdin:
		b, err := io.ReadAll(io.LimitReader(in, maxCredentialRead))
		if err != nil {
			return previewsetup.Credential{}, fmt.Errorf("read the DNS credential from standard input: %w", err)
		}
		raw = b
	case f.dnsCredentialsFile != "":
		file, err := os.Open(f.dnsCredentialsFile)
		if err != nil {
			return previewsetup.Credential{}, errUsage(fmt.Errorf("--dns-credentials-file: %w", err))
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil {
			return previewsetup.Credential{}, err
		}
		if !info.Mode().IsRegular() {
			return previewsetup.Credential{}, errUsage(fmt.Errorf("--dns-credentials-file %s is not a regular "+
				"file: use --dns-credentials-from-stdin for a pipe", f.dnsCredentialsFile))
		}
		if info.Mode().Perm()&0o077 != 0 {
			notef(errOut, "patchy: warning: %s is readable by other users (mode %04o): chmod 600 it.\n",
				f.dnsCredentialsFile, info.Mode().Perm())
		}
		if raw, err = io.ReadAll(io.LimitReader(file, maxCredentialRead)); err != nil {
			return previewsetup.Credential{}, fmt.Errorf("read --dns-credentials-file: %w", err)
		}
	default:
		return previewsetup.Credential{}, nil
	}
	c, err := previewsetup.ParseCredential(a, raw)
	if err != nil {
		return previewsetup.Credential{}, errUsage(err)
	}
	return c, nil
}

// resolveTunnel reads the tunnel credentials file named, else the one an
// earlier run wrote, and records the tunnel it runs in a. The file is
// never echoed; nothing is read off the cloudflare-tunnel recipe, whose
// flags are refused with any other edge.
func resolveTunnel(cmd *cobra.Command, errOut io.Writer, f *setupPreviewsFlags, a *previewsetup.Answers) (
	previewsetup.TunnelCredentials, error) {
	var given previewsetup.TunnelCredentials
	if a.Edge != previewsetup.EdgeCloudflareTunnel {
		for _, name := range cloudflareFlags {
			if cmd.Flags().Changed(name) {
				return given, errUsage(fmt.Errorf("--%s is for --edge %s", name, previewsetup.EdgeCloudflareTunnel))
			}
		}
	}
	if f.tunnelCredentials != "" {
		raw, err := readCredentialFile(errOut, "--tunnel-credentials", f.tunnelCredentials)
		if err != nil {
			return given, err
		}
		if given, err = previewsetup.ParseTunnelCredentials(raw); err != nil {
			return given, errUsage(fmt.Errorf("--tunnel-credentials %s: %w", f.tunnelCredentials, err))
		}
	}
	c, err := previewsetup.ResolveTunnelCredentials(f.output, *a, given)
	if err != nil {
		return c, errUsage(err)
	}
	if !c.IsZero() {
		a.Cloudflare.TunnelID = c.TunnelID()
	}
	return c, nil
}

// readCredentialFile reads a credential file, bounded: a regular file
// only, with a warning when other users can read it.
func readCredentialFile(errOut io.Writer, flag, path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errUsage(fmt.Errorf("%s: %w", flag, err))
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errUsage(fmt.Errorf("%s %s is not a regular file", flag, path))
	}
	if info.Mode().Perm()&0o077 != 0 {
		notef(errOut, "patchy: warning: %s is readable by other users (mode %04o): chmod 600 it.\n", path,
			info.Mode().Perm())
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxCredentialRead))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", flag, err)
	}
	return raw, nil
}

// maxCredentialRead bounds a credential read: past previewsetup's limit,
// so an oversized one is refused rather than cut.
const maxCredentialRead = 1 << 20

// printComparison lists every file with its state, then the differences of
// the files that are not secrets. all lists unchanged files too.
func printComparison(w io.Writer, c previewsetup.Comparison, all bool) {
	for _, ch := range c.Changes {
		if ch.State == previewsetup.StateUnchanged && !all {
			continue
		}
		note := ""
		if ch.File.Secret {
			note = "  (a secret: never shown)"
		}
		_, _ = fmt.Fprintf(w, "%-9s %s%s\n", ch.State, ch.File.Path, note)
	}
	for _, p := range c.Stale {
		_, _ = fmt.Fprintf(w, "%-9s %s  (no longer generated: delete it once nothing applies it)\n", "stale", p)
	}
	for _, ch := range c.Changes {
		if ch.Diff != "" {
			_, _ = fmt.Fprintf(w, "\n%s", ch.Diff)
		}
	}
}

// reportWrite tells the operator what was written and what is left.
func reportWrite(w io.Writer, dir string, c previewsetup.Comparison, written []string, s previewsetup.Secrets,
	rerun bool) {
	if len(written) == 0 {
		notef(w, "patchy: %s is up to date: nothing to write.\n", dir)
	} else {
		notef(w, "patchy: wrote %d file(s) to %s, each mode 0600: %s\n", len(written), dir,
			strings.Join(written, ", "))
	}
	if info, err := os.Stat(dir); err == nil && info.Mode().Perm()&0o077 != 0 {
		notef(w, "patchy: warning: %s is readable by other users (mode %04o): its secrets/ files are not, but "+
			"consider chmod 700.\n", dir, info.Mode().Perm())
	}
	for _, p := range c.Stale {
		notef(w, "patchy: %s is no longer generated: delete it once nothing applies it.\n", p)
	}
	if slices.Contains(written, previewsetup.CloudflaredSecretFile) {
		notef(w, "patchy: wrote the tunnel credentials into %s: apply or encrypt it; never commit it as it is.\n",
			filepath.Join(dir, previewsetup.CloudflaredSecretFile))
	}
	if slices.Contains(written, previewsetup.DNSSecretFile) {
		notef(w, "patchy: wrote the DNS credential into %s: apply or encrypt it; never commit it as it is.\n",
			filepath.Join(dir, previewsetup.DNSSecretFile))
	}
	switch {
	case s.Fresh && rerun:
		notef(w, "patchy: drew a new Dex client secret: apply both files under %s/secrets/ and roll Dex and the "+
			"relay (kubectl rollout restart).\n", dir)
	case s.Fresh:
		notef(w, "patchy: drew the relay's Dex client secret into %s/secrets/: the only copy. Apply or encrypt "+
			"those files; never commit them as they are.\n", dir)
	}
	notef(w, "patchy: next: %s\n", filepath.Join(dir, previewsetup.NextStepsFile))
}
