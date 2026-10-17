// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
}

// newSetupPreviewsCmd is `setup previews` (design
// .claude/plans/preview-setup-design.md §4). Hidden while it is built: it
// asks nothing yet (every answer is a flag), and only the existing-edge
// recipe has landed.
func newSetupPreviewsCmd(opts *Options) *cobra.Command {
	f := &setupPreviewsFlags{}
	a := previewsetup.Defaults()
	cmd := &cobra.Command{
		Use:    "previews",
		Hidden: true,
		Short:  "Write the files that expose previews and sign viewers in to them (incomplete)",
		Long: "Write the files that expose patchy's previews at https://<project>-<issue>.<host suffix>\n" +
			"and sign viewers in to them: patchy's chart values, the manifests the edge needs, the\n" +
			"relay's Dex client secret as two Secret manifests, answers.yaml and NEXT-STEPS.md, all\n" +
			"in -o (default ./patchy-previews, created 0700, every file 0600). Nothing is applied and\n" +
			"no cluster or provider is contacted: NEXT-STEPS.md lists the commands, in order.\n\n" +
			"Still being built: every answer is a flag (no prompts yet), and only --edge existing (an\n" +
			"Ingress class, or a Gateway API parent, already in the cluster) has a recipe.\n\n" +
			"Previews must sit on a registrable domain of their own: a --sign-in-host or --dex-issuer\n" +
			"on the --host-suffix's registrable domain is refused, because preview code would run\n" +
			"same-site with sign-in.\n\n" +
			"A re-run starts from answers.yaml; flags given override it. It shows how each file would\n" +
			"change and writes nothing changed without --force. The Dex client secret is read back\n" +
			"from the Secret files, never drawn again, unless --rotate dex-client-secret. --dry-run\n" +
			"prints the files and their differences (never a secret) and writes nothing.",
		Example: "  patchy setup previews --platform k3s --edge existing --ingress-class traefik \\\n" +
			"    --host-suffix acme-previews.dev --sign-in-host preview-auth.patchy.acme.dev \\\n" +
			"    --dex-issuer https://dex.patchy.acme.dev --viewer-team acme:qa --registry ghcr.io \\\n" +
			"    --image-path-prefix acme/patchy/previews",
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
	fl.StringVar(&a.Existing.CertificateIssuer, "certificate-issuer", "", "--edge existing: the cert-manager "+
		"issuer, Issuer/<name> or ClusterIssuer/<name> (default "+previewsetup.DefaultCertificateIssuer+")")
	fl.StringVar(&a.Release, "release", a.Release, "the patchy Helm release")
	// -o names a directory here, not an output format.
	fl.StringVarP(&f.output, "output", "o", defaultPreviewsDir, "the directory to write to")
	fl.BoolVar(&f.force, "force", false, "replace files that would change")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print the files and their differences; write nothing")
	fl.StringVar(&f.rotate, "rotate", "", "draw a new secret although one is written: "+
		previewsetup.RotateDexClientSecret)
	_ = cmd.RegisterFlagCompletionFunc("platform", fixedCompletion(previewsetup.Platforms()))
	_ = cmd.RegisterFlagCompletionFunc("edge", fixedCompletion(previewsetup.Edges()))
	_ = cmd.RegisterFlagCompletionFunc("node-isolation", fixedCompletion([]string{
		previewsetup.IsolationRequired, previewsetup.IsolationNone}))
	_ = cmd.RegisterFlagCompletionFunc("edge-tls", fixedCompletion([]string{
		previewsetup.TLSCertificate, previewsetup.TLSEdge}))
	_ = cmd.RegisterFlagCompletionFunc("rotate", fixedCompletion([]string{previewsetup.RotateDexClientSecret}))
	for _, name := range []string{"host-suffix", "sign-in-host", "dex-issuer", "dex-namespace", "dex-client-id",
		"viewer-team", "viewer-user", "registry", "image-path-prefix", "node-label", "eks-auto-mode", "taint-key",
		"api-server-cidr", "ingress-class", "gateway-parent", "certificate-issuer", "release"} {
		_ = cmd.RegisterFlagCompletionFunc(name, noFileCompletion)
	}
	return cmd
}

// answerFlags maps each answer flag to the field it sets, so a re-run can
// lay only the flags given over answers.yaml.
func answerFlags(dst *previewsetup.Answers, src previewsetup.Answers) map[string]func() {
	return map[string]func(){
		"platform":          func() { dst.Platform = src.Platform },
		"edge":              func() { dst.Edge = src.Edge },
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
		"certificate-issuer": func() { dst.Existing.CertificateIssuer = src.Existing.CertificateIssuer },
		"release":            func() { dst.Release = src.Release },
	}
}

// resolveAnswers is the run's answers: answers.yaml under dir when there
// is one, with the flags given laid over it; else the flags over the
// defaults.
func resolveAnswers(cmd *cobra.Command, opts *Options, f *setupPreviewsFlags,
	flags previewsetup.Answers) (previewsetup.Answers, bool, error) {
	eks, err := previewsetup.ParseEKSAutoMode(f.eksAutoMode)
	if err != nil {
		return flags, false, errUsage(err)
	}
	flags.NodeIsolation.EKSAutoMode = eks
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
	for name, set := range answerFlags(&a, flags) {
		if cmd.Flags().Changed(name) {
			set()
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
	if m := a.Missing(); len(m) > 0 {
		return errUsage(fmt.Errorf("answer %s (setup previews asks nothing yet: every answer is a flag)",
			strings.Join(m, ", ")))
	}
	if err := a.Validate(); err != nil {
		return errUsage(err)
	}
	if !previewsetup.Ready(a.Edge) {
		return errUsage(fmt.Errorf("--edge %s: %w; use --edge existing for now", a.Edge,
			previewsetup.ErrRecipeNotReady))
	}
	rotate := f.rotate != ""
	secrets, err := previewsetup.ResolveSecrets(f.output, rotate, rand.Reader)
	if err != nil {
		return err
	}
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
	written, err := previewsetup.Write(c, previewsetup.Accept{All: f.force, Secrets: rotate})
	var conflict *previewsetup.ConflictError
	if errors.As(err, &conflict) {
		printComparison(opts.Out, c, false)
		return err
	}
	if err != nil {
		return err
	}
	reportWrite(opts.ErrOut, f.output, c, written, secrets, rerun)
	return nil
}

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
