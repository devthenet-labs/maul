// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewdetect"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/tailnet"
)

// detectTimeout bounds detection's reads: a dozen list and get calls.
const detectTimeout = time.Minute

// detectPreviews is --detect: a read-only look at the cluster the caller's
// kubeconfig names (never a Secret, nothing written), judged into
// Findings. A read refused is a note in the Findings; failing to reach the
// cluster at all (no read the API server answered) is an error, since the
// operator asked for detection. Connect makes no request, so that is only
// known once the reads have been tried.
func detectPreviews(ctx context.Context, opts *Options, namespace string) (previewdetect.Findings, error) {
	env, err := opts.Connect()
	if err != nil {
		return previewdetect.Findings{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()
	opts.debugf("detecting the preview install's surroundings (release namespace %s)", namespace)
	o := previewdetect.Observe(ctx, env.Client, namespace)
	if o.Unreachable != nil {
		return previewdetect.Findings{}, fmt.Errorf("--detect could not reach the cluster: %w", o.Unreachable)
	}
	return previewdetect.Judge(o), nil
}

// detectAndAsk runs detection when asked for, laying what it finds over a,
// then, with a prompter (a terminal), asks for what is still unanswered.
// Detection runs again over the answers given at the prompts: what it fills
// in can depend on them (the edge decides whether cert-manager matters).
func detectAndAsk(cmd *cobra.Command, opts *Options, f *setupPreviewsFlags, a *previewsetup.Answers,
	p *prompter) error {
	given := func(flag string) bool { return cmd.Flags().Changed(flag) }
	var found *previewdetect.Findings
	if f.detect {
		fd, err := detectPreviews(cmd.Context(), opts, a.Namespace)
		if err != nil {
			return err
		}
		found = &fd
		filled := previewdetect.Apply(fd, a, given)
		reportDetected(opts.ErrOut, previewdetect.Report(fd, *a), filled)
	}
	if p != nil && needsAsking(*a) {
		if err := askPreviews(p, a, f, found); err != nil {
			return err
		}
		a.Normalize()
		if found != nil {
			reportFilled(opts.ErrOut, previewdetect.Apply(*found, a, given))
		}
	}
	if f.detect && a.Edge == previewsetup.EdgeTailscale {
		detectTailnet(cmd.Context(), opts, a, given)
	}
	return nil
}

// detectTailnet is --detect's tailnet half, for the tailscale recipe: the
// gateway's device as this machine's own Tailscale client sees it
// (`tailscale status --json`, read-only; the cluster records the device's
// address only in a Secret, which detection never reads), its address
// filled while unanswered and the exact DNS records printed.
func detectTailnet(ctx context.Context, opts *Options, a *previewsetup.Answers, given previewdetect.Given) {
	status := opts.tailnetStatus
	if status == nil {
		status = tailnet.Local
	}
	m := tailnet.Lookup(ctx, status, a.TailscaleHostname(), []string{a.TailscaleTag()})
	report, filled := previewdetect.ApplyTailnet(m, a, given)
	reportDetected(opts.ErrOut, report, filled)
}

// needsAsking reports answers a still lacks: a flag Missing names, a
// setting the DNS-01 solver requires, or the acknowledgement a shared
// registrable domain needs.
func needsAsking(a previewsetup.Answers) bool {
	if len(a.Missing()) > 0 || a.SharedUnacknowledged() {
		return true
	}
	if !a.WritesIssuer() {
		return false
	}
	for _, o := range previewsetup.DNSOptionSpecs(a.Certificate.DNS) {
		if _, set := a.Certificate.DNSOptions[o.Key]; o.Required && !set {
			return true
		}
	}
	return false
}

// reportDetected prints what detection found and filled in, on stderr.
func reportDetected(w io.Writer, lines, filled []string) {
	for _, l := range lines {
		notef(w, "patchy: detected: %s\n", l)
	}
	reportFilled(w, filled)
}

// reportFilled prints the answers detection filled in.
func reportFilled(w io.Writer, filled []string) {
	for _, l := range filled {
		notef(w, "patchy: detected answer: %s\n", l)
	}
}

// previewsTerminal reports whether `setup previews` may ask its questions
// on r: a terminal, or what a test pins.
func (o *Options) previewsTerminal(r io.Reader) bool {
	if o.promptTerminal != nil {
		return o.promptTerminal(r)
	}
	f, ok := r.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
