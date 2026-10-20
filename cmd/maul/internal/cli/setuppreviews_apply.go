// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/devthenet-labs/maul/cmd/maul/internal/kubecfg"
	"github.com/devthenet-labs/maul/cmd/maul/internal/previewapply"
	"github.com/devthenet-labs/maul/cmd/maul/internal/previewsetup"
	"github.com/devthenet-labs/maul/cmd/maul/internal/printer"
)

// applyFlags are `setup previews --apply`'s own flags (setup-previews-easy
// (d)): off unless --apply is given, and then each named, never guessed.
type applyFlags struct {
	apply        bool
	values       []string
	chartVersion string
	waitTimeout  time.Duration
}

// The --apply flags.
const (
	applyFlag        = "apply"
	valuesFlag       = "values"
	chartVersionFlag = "chart-version"
	waitTimeoutFlag  = "wait-timeout"
)

// addApplyFlags registers the --apply flags on fl.
func addApplyFlags(fl *pflag.FlagSet, f *applyFlags) {
	fl.BoolVar(&f.apply, applyFlag, false, "after writing the files, run the in-cluster steps of NEXT-STEPS.md "+
		"with your kubeconfig and helm: the Secrets, cert-manager and the Tailscale operator at the pinned "+
		"versions, the Certificate, the maul upgrade, then the checks (with --dry-run: print them)")
	fl.StringArrayVar(&f.values, valuesFlag, nil, "--apply: a values file the maul release already runs with "+
		"(repeatable, in order); the upgrade layers "+previewsetup.ValuesFile+" after them")
	fl.StringVar(&f.chartVersion, chartVersionFlag, "", "--apply: the maul chart version to upgrade the "+
		"release to, such as 0.12.41")
	fl.DurationVar(&f.waitTimeout, waitTimeoutFlag, previewsetup.DefaultApplyTimeout, "--apply: how long each "+
		"Helm install and each wait may take")
}

// checkApplyFlags refuses an --apply flag without --apply, and --apply
// without the values and chart version it upgrades with, before anything
// is written.
func checkApplyFlags(cmd *cobra.Command, f applyFlags) error {
	if !f.apply {
		for _, name := range []string{valuesFlag, chartVersionFlag, waitTimeoutFlag} {
			if cmd.Flags().Changed(name) {
				return errUsage(fmt.Errorf("--%s needs --%s", name, applyFlag))
			}
		}
		return nil
	}
	if len(f.values) == 0 {
		return errUsage(errors.New("--apply needs --values: the values file(s) the maul release already runs " +
			"with (helm get values <release> -n <namespace> shows them), which the upgrade layers " +
			previewsetup.ValuesFile + " after"))
	}
	if previewsetup.NormalizeChartVersion(f.chartVersion) == "" {
		return errUsage(fmt.Errorf("--apply needs --chart-version: the maul chart version to upgrade to, such "+
			"as 0.12.41 (%q is not one)", f.chartVersion))
	}
	if f.waitTimeout <= 0 {
		return errUsage(fmt.Errorf("--%s must be positive", waitTimeoutFlag))
	}
	for _, v := range f.values {
		info, err := os.Stat(v)
		if err != nil {
			return errUsage(fmt.Errorf("--values: %w", err))
		}
		if !info.Mode().IsRegular() {
			return errUsage(fmt.Errorf("--values %s is not a regular file", v))
		}
	}
	return nil
}

// planApply is the --apply plan for the files planned, nil without
// --apply. Its printed commands carry the caller's --kubeconfig and
// --context; freshSecret (a Dex client secret drawn because no file held
// one, not on --rotate) makes it refuse to replace one in the cluster.
func planApply(opts *Options, f *setupPreviewsFlags, a previewsetup.Answers, files []previewsetup.File,
	freshSecret bool) ([]previewsetup.Step, error) {
	if !f.apply.apply {
		return nil, nil
	}
	steps, err := previewsetup.ApplySteps(a, files, previewsetup.ApplyInput{Dir: f.output,
		ValuesFiles: f.apply.values, ChartVersion: f.apply.chartVersion, Timeout: f.apply.waitTimeout,
		Connection:     previewsetup.Connection{Kubeconfig: opts.Kubeconfig, Context: opts.Context},
		FreshDexSecret: freshSecret})
	if err != nil {
		return nil, errUsage(err)
	}
	return steps, nil
}

// previewApplyDeps is what --apply reaches: the cluster, helm and the
// checks. Tests fake all three.
type previewApplyDeps struct {
	cluster previewapply.Cluster
	helm    previewapply.Helm
	check   func(ctx context.Context) error
}

// applyDeps are the real ports over env: the caller's own kubeconfig for
// the API and for helm, and the checks of `check previews`.
func (o *Options) applyDeps(env *kubecfg.Env, namespace string) previewApplyDeps {
	if o.previewApply != nil {
		return o.previewApply(env, namespace)
	}
	return previewApplyDeps{
		cluster: previewapply.KubeCluster{Client: env.Client},
		helm:    previewapply.ExecHelm{Kubeconfig: o.Kubeconfig, Context: o.Context, Out: o.ErrOut},
		check: func(ctx context.Context) error {
			net := networkCheckPreviewsDeps
			if o.previewChecksNet != nil {
				net = o.previewChecksNet
			}
			deps := net()
			if o.tailnetStatus != nil {
				deps.tailnet = o.tailnetStatus
			}
			// Bounded as `check previews` is: the last step must not wait
			// on a hung resolver or preview host for ever.
			ctx, cancel := context.WithTimeout(ctx, checkPreviewsTimeout)
			defer cancel()
			return installChecks(ctx, o, env, namespace, deps, printer.FormatTable)
		},
	}
}

// runApply runs the plan against the cluster the caller's kubeconfig
// names, then says what is left to them.
func runApply(ctx context.Context, opts *Options, a previewsetup.Answers, steps []previewsetup.Step) error {
	env, err := opts.Connect()
	if err != nil {
		return err
	}
	before, after := previewsetup.ApplyManual(a)
	for _, m := range before {
		notef(opts.ErrOut, "maul: --apply takes this as done: %s\n", m)
	}
	d := opts.applyDeps(env, a.Namespace)
	err = previewapply.Runner{Cluster: d.cluster, Helm: d.helm, Check: d.check, Log: opts.ErrOut}.Run(ctx, steps)
	if err == nil || errors.Is(err, previewapply.ErrCheckFailed) {
		for _, m := range after {
			notef(opts.ErrOut, "maul: left to you: %s\n", m)
		}
	}
	if err == nil {
		notef(opts.ErrOut, "maul: applied: every step ran and no check failed.\n")
	}
	return err
}
