// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/kubecfg"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewcheck"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/printer"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/tailnet"
	"github.com/bitwise-media-group/patchy/internal/version"
)

// checkPreviewsTimeout bounds a whole `check previews` run: a few cluster
// reads, one DNS lookup, one TLS handshake and a handful of HTTP requests
// per Ready Preview.
const checkPreviewsTimeout = 2 * time.Minute

// newCheckPreviewsCmd checks the preview install as a whole.
func newCheckPreviewsCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "previews",
		Short: "Check that the preview install is ready, before any Project uses it",
		Long: "Check the preview install in the release namespace (-n) from your workstation,\n" +
			"before any Project uses it: one line per check, PASS, FAIL, WARN, INFO or\n" +
			"SKIP, then the reason. check project judges one Project's previews; this\n" +
			"judges what every Project's previews stand on, with the same checks where\n" +
			"they overlap.\n\n" +
			"preview-config reads the controllers' ConfigMaps: previews and sign-in are\n" +
			"on and agree on the host suffix, the preview gateway alone serves preview\n" +
			"hosts (the legacy load balancer path still serving is a WARN, and serving\n" +
			"without sign-in, previewAuth.stage permit, a FAIL), the sign-in\n" +
			"host sits outside the suffix, and the suffix is a registrable domain of its\n" +
			"own, shared with neither the sign-in host, the relay's issuer nor Dex (a\n" +
			"WARN: preview code would run same-site with sign-in). Once the chart's\n" +
			"previewAuth.sharedSiteAcknowledged names that registrable domain (setup\n" +
			"previews --shared-domain acknowledged writes it), the sharing is INFO\n" +
			"instead. INFO, like WARN, never fails the run.\n\n" +
			"preview-dns, preview-tls, preview-auth, preview-auth-dex, preview-auth-host and\n" +
			"preview-gateway-allowlist are check project's, asked of\n" +
			"patchy-probe-0.<host suffix> (the relay's own probe host, which no Preview\n" +
			"owns) and of every Ready Preview, whatever its Project: the host resolves\n" +
			"to the gateway's edge and serves a trusted certificate, the relay and Dex\n" +
			"answer, the probe host is the gateway's 404 page, each Ready Preview\n" +
			"redirects to sign in, and a spoofed X-Forwarded-For cannot get past\n" +
			"previewAuth.gateway.inboundCIDRs.\n\n" +
			"On a Cloudflare Tunnel edge (the cloudflare-tunnel recipe), whose gateway\n" +
			"publishes no address, preview-dns passes when the record is a CNAME to the\n" +
			"tunnel cloudflared serves (<tunnel id>.cfargotunnel.com). A proxied record\n" +
			"shows only Cloudflare's addresses and hides its target: a SKIP, and\n" +
			"preview-auth-host proves the routing. Anything else is a WARN or a SKIP\n" +
			"saying what to look at. cloudflared's ConfigMap names the tunnel; its\n" +
			"credentials are never read.\n\n" +
			"On a tailnet edge (the tailscale recipe: the gateway's Service carries the\n" +
			"Tailscale operator's tailscale.com/expose annotation) with no\n" +
			"previewAuth.gateway.publishedAddresses, the operator records the device's\n" +
			"address only in a Secret, which is never read: this machine's own Tailscale\n" +
			"client is asked instead (tailscale status --json, read-only) for the device\n" +
			"of that hostname and tags. preview-dns then compares both records with its\n" +
			"address and, when one is missing or wrong, prints the exact records to\n" +
			"create. Off the tailnet, or without a tailscale command, it is a SKIP.\n\n" +
			"preview-viewers reads the bindings of the chart's <release>-preview-viewer\n" +
			"ClusterRole: at least one User or Group carrying the relay's claim prefix\n" +
			"should be bound, or every sign-in is refused 403 unless another role grants\n" +
			"get on projects/previews, which this cannot see (a WARN). preview-nodes\n" +
			"reads the nodes: with node isolation required, at least one carries preview-\n" +
			"controller's node selector and its <taint key>=true:NoExecute taint, and no\n" +
			"node carries the selector without that taint (a FAIL: any Pod could run\n" +
			"beside preview code there). A node with the taint but not the selector is\n" +
			"another install's or a label typo (a WARN); with an EKS Auto Mode NodePool,\n" +
			"no node yet is a SKIP; isolation off is a WARN.\n\n" +
			"preview-upstreams compares cert-manager, the Tailscale operator and\n" +
			"cloudflared, where a Deployment runs them, with the versions setup previews\n" +
			"pins, by the image's tag or the app.kubernetes.io/version label: older is a\n" +
			"WARN, and none running reports no line.\n\n" +
			"The check never reads a Secret, and without --isolation it never writes. Its\n" +
			"cluster reads, with your kubeconfig, are ConfigMaps, the preview placeholder\n" +
			"and gateway Ingresses, the gateway's Service, Previews, RoleBindings,\n" +
			"ClusterRoleBindings, Nodes and Deployments; a read you may not make is a\n" +
			"SKIP. The gateway's edge may admit only\n" +
			"previewAuth.gateway.inboundCIDRs, so from any other address the host checks\n" +
			"time out or are refused, and that is a SKIP, not a FAIL.\n\n" +
			"--isolation adds preview-netpol, the one check that writes to the cluster:\n" +
			"it proves the slot NetworkPolicy lets only the preview gateway reach a slot\n" +
			"Pod, as hack/preview-isolation-probe/ingress.sh --refused-is-blocked does.\n" +
			"First every NetworkPolicy in the slot namespace is read as written: none may\n" +
			"admit a peer but the gateway's exact one (the release namespace AND its\n" +
			"label) and, while the legacy load balancer path serves, ipBlocks; anything\n" +
			"else is a FAIL before a Pod exists. That proves the slot policy's shape; the\n" +
			"live run then proves the path end to end.\n" +
			"A netprobe listener starts in patchy-preview-<--isolation-slot> (on the\n" +
			"preview nodes); a client labelled as the gateway must reach it first; only\n" +
			"then do a client in the agents' namespace and an unlabelled one in the\n" +
			"release namespace run, and both must be blocked (a refused connection counts\n" +
			"as blocked only after that reach); and the listener must still be the same\n" +
			"running Pod afterwards, so a dead listener never passes as a block. All four\n" +
			"Pods are deleted at the end, on failure and on interrupt too. The image must\n" +
			"sit under preview-controller's image prefix as <prefix>netprobe:sha-<40 hex>\n" +
			"(--isolation-image; by default that tag at this CLI's commit): build it from\n" +
			"hack/preview-isolation-probe at the same commit. Anything short of a proven\n" +
			"block is a FAIL; slot policy ipBlocks (the legacy load balancer's subnets)\n" +
			"or a Pod left behind is a WARN.\n\n" +
			"-o json or -o yaml prints the whole report as data. The exit status is 1 when\n" +
			"any check fails; a WARN never fails the run.",
		Example: "  patchy check previews -n patchy\n" +
			"  patchy check previews -n patchy -o json | jq '.checks[] | select(.status != \"PASS\")'\n" +
			"  patchy check previews -n patchy --isolation \\\n" +
			"    --isolation-image registry.example/patchy/previews/netprobe:sha-0123456789abcdef0123456789abcdef01234567",
		Args:              cobra.NoArgs,
		ValidArgsFunction: noFileCompletion,
		RunE: func(cmd *cobra.Command, _ []string) error {
			deps, err := checkPreviewsNetwork(cmd)
			if err != nil {
				return errUsage(err)
			}
			if deps.isolation, err = isolationFromFlags(cmd); err != nil {
				return errUsage(err)
			}
			return runCheckPreviews(cmd.Context(), opts, deps)
		},
	}
	f := cmd.Flags()
	f.Bool(isolationFlag, false, "also run preview-netpol: create four short-lived probe Pods (a listener in a "+
		"slot, three clients) to prove only the preview gateway reaches a slot, then delete them; the one part "+
		"of the check that writes to the cluster")
	f.String(isolationImageFlag, "", "the netprobe image for --isolation, under the preview image prefix as "+
		"<prefix>netprobe:sha-<40 hex> (default: that tag at this CLI's commit)")
	f.Int(isolationSlotFlag, 0, "the slot the --isolation listener runs in")
	f.Duration(isolationTimeoutFlag, previewcheck.DefaultIsolationTimeout, "how long each --isolation Pod may "+
		"take to start or finish")
	checkPreviewsTestFlags(cmd)
	return cmd
}

// The --isolation flags.
const (
	isolationFlag        = "isolation"
	isolationImageFlag   = "isolation-image"
	isolationSlotFlag    = "isolation-slot"
	isolationTimeoutFlag = "isolation-timeout"
)

// isolationFromFlags is the probe the flags ask for, nil without
// --isolation; an --isolation-* flag without it is a usage error.
func isolationFromFlags(cmd *cobra.Command) (*previewcheck.Isolation, error) {
	f := cmd.Flags()
	on, _ := f.GetBool(isolationFlag)
	if !on {
		for _, name := range []string{isolationImageFlag, isolationSlotFlag, isolationTimeoutFlag} {
			if f.Changed(name) {
				return nil, fmt.Errorf("--%s needs --%s", name, isolationFlag)
			}
		}
		return nil, nil //nolint:nilnil // no probe asked for
	}
	image, _ := f.GetString(isolationImageFlag)
	slot, _ := f.GetInt(isolationSlotFlag)
	timeout, _ := f.GetDuration(isolationTimeoutFlag)
	if timeout <= 0 {
		return nil, fmt.Errorf("--%s must be positive", isolationTimeoutFlag)
	}
	return &previewcheck.Isolation{Image: image, Commit: version.Commit, Slot: slot, Timeout: timeout}, nil
}

// networkCheckPreviewsDeps is what a released binary reaches: the caller's
// own DNS, the system's roots and the hosts by name.
func networkCheckPreviewsDeps() checkPreviewsDeps {
	return checkPreviewsDeps{
		resolver: net.DefaultResolver,
		dialTLS:  previewcheck.DialTLS,
		http:     previewcheck.NewHTTPDoer(),
		tailnet:  tailnet.Local,
	}
}

// checkPreviewsDeps is what `check previews` reaches beyond the cluster: the
// caller's DNS and the preview hosts. Tests fake all of them.
type checkPreviewsDeps struct {
	resolver previewcheck.Resolver
	dialTLS  previewcheck.TLSDialer
	http     previewcheck.HTTPDoer
	// tailnet is the caller's Tailscale client, asked (read-only) for a
	// gateway the Tailscale operator exposes; nil asks nothing.
	tailnet tailnet.StatusFunc
	// isolation is the --isolation probe, nil without the flag; its Client
	// is the caller's kubeconfig client, and its Logs, when nil, read
	// through the kubeconfig's REST config.
	isolation *previewcheck.Isolation
}

// runCheckPreviews runs the install-level checks and renders the report;
// any failed check makes the command fail.
func runCheckPreviews(ctx context.Context, opts *Options, deps checkPreviewsDeps) error {
	format, err := printer.ParseFormat(opts.Output)
	if err != nil {
		return errUsage(err)
	}
	if opts.AllNamespaces {
		return errUsage(errors.New("check previews reads one install: name its release namespace with -n, not -A"))
	}
	env, err := opts.Connect()
	if err != nil {
		return err
	}
	if env.Namespace == "" {
		return errUsage(errors.New("check previews needs the release namespace: pass -n"))
	}
	timeout := checkPreviewsTimeout
	if iso := deps.isolation; iso != nil {
		iso.Client = env.Client
		if iso.Logs == nil {
			if iso.Logs, err = podLogReader(env.Config); err != nil {
				return err
			}
		}
		// The listener's start, the gateway client, then the other two
		// together, each bounded by the timeout; cleanup has its own.
		timeout += 3*iso.Timeout + time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return installChecks(ctx, opts, env, env.Namespace, deps, format)
}

// installChecks runs the install-level checks in namespace and renders
// the report in format; any failed check is an error. `setup previews
// --apply` ends with it.
func installChecks(ctx context.Context, opts *Options, env *kubecfg.Env, namespace string, deps checkPreviewsDeps,
	format printer.Format) error {
	opts.debugf("checking the preview install in namespace %s", namespace)
	report := previewcheck.Install(ctx, previewcheck.Config{
		Reader: env.Client, Namespace: namespace,
		Resolver: deps.resolver, DialTLS: deps.dialTLS, HTTP: deps.http, Isolation: deps.isolation,
		Tailnet: deps.tailnet,
	})
	if err := checkreport.Render(opts.Out, format, report, report.Lines()); err != nil {
		return err
	}
	if n := report.Failed(); n > 0 {
		return fmt.Errorf("%d check%s failed", n, plural(n, "", "s"))
	}
	return nil
}

// probeLogLimit bounds a probe client's log read: its burst writes a few
// dozen short lines.
const probeLogLimit = 1 << 20

// podLogReader reads a probe Pod's log through the kubeconfig's REST
// config.
func podLogReader(cfg *rest.Config) (previewcheck.LogReader, error) {
	if cfg == nil {
		return nil, errors.New("no REST config to read the probe Pods' logs with")
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build a client for the probe Pods' logs: %w", err)
	}
	limit := int64(probeLogLimit)
	return func(ctx context.Context, namespace, pod string) ([]byte, error) {
		return cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{Container: "probe",
			LimitBytes: &limit}).DoRaw(ctx)
	}, nil
}
