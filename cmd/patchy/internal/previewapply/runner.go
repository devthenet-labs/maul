// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewapply

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewsetup"
)

// Cluster is what --apply asks of the Kubernetes API.
type Cluster interface {
	// Apply server-side applies obj.
	Apply(ctx context.Context, obj *unstructured.Unstructured) error
	// Exists reports whether obj's kind, namespace and name exist,
	// reading metadata only (never a Secret's data).
	Exists(ctx context.Context, obj *unstructured.Unstructured) (bool, error)
	// NamespaceExists reports whether namespace name exists.
	NamespaceExists(ctx context.Context, name string) (bool, error)
	// EnsureNamespace creates namespace name unless it exists, reporting
	// whether it created it.
	EnsureNamespace(ctx context.Context, name string) (bool, error)
	// SecretExists reports whether the Secret exists, reading its metadata
	// only.
	SecretExists(ctx context.Context, namespace, name string) (bool, error)
	// WaitCertificateReady waits, until ctx ends, for a cert-manager
	// Certificate's Ready condition.
	WaitCertificateReady(ctx context.Context, namespace, name string) error
	// WaitDeploymentAvailable waits, until ctx ends, for a Deployment to
	// roll out.
	WaitDeploymentAvailable(ctx context.Context, namespace, name string) error
}

// Helm is what --apply asks of Helm.
type Helm interface {
	// Version reports helm's version, failing when helm cannot run.
	Version(ctx context.Context) (string, error)
	// Revision is the release's current revision, 0 when it is not
	// installed.
	Revision(ctx context.Context, release, namespace string) (int, error)
	// Run runs helm with args (previewsetup.HelmArgs).
	Run(ctx context.Context, args []string) error
}

// Runner runs an --apply plan.
type Runner struct {
	Cluster Cluster
	Helm    Helm
	// Check runs the checks of `patchy check previews`, printing its
	// report, and fails when a check fails.
	Check func(ctx context.Context) error
	// Log is where each step is narrated (the caller's stderr).
	Log io.Writer
	// ReadFile reads a generated file; nil is os.ReadFile, bounded.
	ReadFile func(path string) ([]byte, error)
}

// ErrCheckFailed reports an apply whose every step ran but whose checks
// did not all pass.
var ErrCheckFailed = errors.New("the checks did not all pass")

// maxManifest bounds a generated file read back: they are a few
// kilobytes.
const maxManifest = 1 << 20

// ErrWouldReplace reports a run that would replace, in the cluster, a Dex
// client secret it drew anew: refused before anything changes.
var ErrWouldReplace = errors.New("refused to replace a Dex client secret already in the cluster")

// undo is how to reverse one completed step, by hand: the commands, and
// what no command can reverse.
type undo struct{ commands, notes []string }

// Run runs steps in order, stopping at the first failure. Each step is
// logged as it starts and ends. On a failure before the checks it prints
// how to undo what this run changed, newest first, and returns an error
// naming the step; a failed check returns ErrCheckFailed with everything
// left applied.
func (r Runner) Run(ctx context.Context, steps []previewsetup.Step) error {
	if err := r.guard(ctx, steps); err != nil {
		r.logf("patchy: apply: %v\n", err)
		return err
	}
	var undos []undo
	for i, s := range steps {
		r.logf("patchy: apply %d/%d: %s\n", i+1, len(steps), s.Title)
		u, skipped, err := r.step(ctx, s)
		if err != nil {
			if s.Kind == previewsetup.StepCheck {
				r.logf("patchy: apply %d/%d: %v. Everything above stays applied: what the checks name, or a step "+
					"left to you (below), is what is left.\n", i+1, len(steps), err)
				return fmt.Errorf("%w: run patchy check previews -n %s again once they are done", ErrCheckFailed,
					s.Namespace)
			}
			r.logf("patchy: apply %d/%d failed: %v\n", i+1, len(steps), err)
			r.printUndo(undos, u)
			return fmt.Errorf("apply stopped at step %d/%d (%s): %w", i+1, len(steps), s.Title, err)
		}
		if skipped != "" {
			r.logf("patchy: apply %d/%d: skipped: %s\n", i+1, len(steps), skipped)
			continue
		}
		if len(u.commands) > 0 || len(u.notes) > 0 {
			undos = append(undos, u)
		}
		r.logf("patchy: apply %d/%d: done\n", i+1, len(steps))
	}
	return nil
}

// printUndo says how to undo what the run changed, newest first, with the
// failed step's own undo (it may have changed part of what it does)
// first.
func (r Runner) printUndo(done []undo, failed undo) {
	var cmds, notes []string
	cmds = append(cmds, failed.commands...)
	notes = append(notes, failed.notes...)
	for _, u := range slices.Backward(done) {
		cmds = append(cmds, u.commands...)
		notes = append(notes, u.notes...)
	}
	defer func() {
		for _, n := range notes {
			r.logf("patchy: not undone by any command here: %s\n", n)
		}
	}()
	if len(cmds) == 0 {
		r.logf("patchy: this run created nothing to delete: fix what it says and run --apply again (each step is " +
			"safe to repeat).\n")
		return
	}
	r.logf("patchy: fix what it says and run --apply again (each step is safe to repeat), or undo what this run " +
		"changed, newest first:\n")
	for _, c := range cmds {
		r.logf("  %s\n", c)
	}
	r.logf("patchy: NEXT-STEPS.md's \"Rolling back\" undoes the whole install.\n")
}

// guard refuses, before any step changes anything, a plan that would
// replace an object a NoReplace file names: a Dex client secret drawn
// anew this run while one is already in the cluster. Server-side apply
// would overwrite it, breaking sign-in until Dex is rolled, and its
// earlier content could not be put back. Only metadata is read.
func (r Runner) guard(ctx context.Context, steps []previewsetup.Step) error {
	for _, s := range steps {
		for _, f := range s.NoReplace {
			objs, err := r.objects(s, f)
			if err != nil {
				return err
			}
			for _, o := range objs {
				existed, err := r.Cluster.Exists(ctx, o)
				if err != nil {
					return applyError(o, s.Secret, err)
				}
				if existed {
					return fmt.Errorf("%w: %s is there, and this run drew a new secret because its output "+
						"directory holds no secret file. Run again with -o the directory that wrote the live "+
						"one, or pass --rotate %s to replace it on purpose (then roll Dex and the relay)",
						ErrWouldReplace, describe(o), previewsetup.RotateDexClientSecret)
				}
			}
		}
	}
	return nil
}

// objects reads and decodes one of the step's files.
func (r Runner) objects(s previewsetup.Step, f string) ([]*unstructured.Unstructured, error) {
	data, err := r.read(f)
	if err != nil {
		return nil, err
	}
	o, err := Objects(data)
	if err != nil {
		if s.Secret {
			return nil, fmt.Errorf("%s is not a manifest this command wrote: run setup previews again", f)
		}
		return nil, fmt.Errorf("%s: %w", f, err)
	}
	return o, nil
}

// step runs one step: the undo for what it changes, a reason when it is
// skipped, or its error.
func (r Runner) step(ctx context.Context, s previewsetup.Step) (undo, string, error) {
	switch s.Kind {
	case previewsetup.StepPreflight:
		return undo{}, "", r.preflight(ctx, s)
	case previewsetup.StepApply:
		return r.apply(ctx, s)
	case previewsetup.StepNamespace:
		created, err := r.Cluster.EnsureNamespace(ctx, s.Namespace)
		if err != nil || !created {
			return undo{}, "", err
		}
		return undo{commands: []string{s.Conn.Kubectl() + " delete namespace " + s.Namespace}}, "", nil
	case previewsetup.StepRequireSecret:
		ok, err := r.Cluster.SecretExists(ctx, s.Namespace, s.Name)
		switch {
		case err != nil:
			return undo{}, "", fmt.Errorf("read Secret %s/%s's metadata: %w", s.Namespace, s.Name, err)
		case !ok:
			return undo{}, "", fmt.Errorf("no Secret %s/%s: give the OAuth client with --tailscale-oauth-dir, or "+
				"create the Secret as NEXT-STEPS.md step 6 says", s.Namespace, s.Name)
		}
		return undo{}, "", nil
	case previewsetup.StepHelm:
		u, err := r.helm(ctx, s)
		return u, "", err
	case previewsetup.StepWaitCertificate:
		wctx, cancel := context.WithTimeout(ctx, s.Timeout)
		defer cancel()
		return undo{}, "", r.Cluster.WaitCertificateReady(wctx, s.Namespace, s.Name)
	case previewsetup.StepWaitDeployment:
		wctx, cancel := context.WithTimeout(ctx, s.Timeout)
		defer cancel()
		return undo{}, "", r.Cluster.WaitDeploymentAvailable(wctx, s.Namespace, s.Name)
	case previewsetup.StepCheck:
		return undo{}, "", r.Check(ctx)
	}
	return undo{}, "", fmt.Errorf("unknown step %q", s.Kind)
}

// preflight checks, before anything changes, that helm runs and the
// patchy release is installed: --apply upgrades it, never installs it.
func (r Runner) preflight(ctx context.Context, s previewsetup.Step) error {
	v, err := r.Helm.Version(ctx)
	if err != nil {
		return fmt.Errorf("--apply runs helm (3.8 or later): %w", err)
	}
	r.logf("patchy: helm %s\n", v)
	rev, err := r.Helm.Revision(ctx, s.Helm.Release, s.Helm.Namespace)
	if err != nil {
		return fmt.Errorf("read release %s in namespace %s: %w", s.Helm.Release, s.Helm.Namespace, err)
	}
	if rev == 0 {
		return fmt.Errorf("release %s is not installed in namespace %s: install patchy first, or name its "+
			"release with --release and its namespace with -n", s.Helm.Release, s.Helm.Namespace)
	}
	r.logf("patchy: release %s is at revision %d\n", s.Helm.Release, rev)
	return nil
}

// apply server-side applies every object in the step's files, in order.
func (r Runner) apply(ctx context.Context, s previewsetup.Step) (undo, string, error) {
	if ns := s.SkipUnlessNamespace; ns != "" {
		ok, err := r.Cluster.NamespaceExists(ctx, ns)
		if err != nil {
			return undo{}, "", fmt.Errorf("read namespace %s: %w", ns, err)
		}
		if !ok {
			return undo{}, fmt.Sprintf("no namespace %s here: apply %s where Dex runs", ns,
				strings.Join(s.Files, ", ")), nil
		}
	}
	var objs []*unstructured.Unstructured
	for _, f := range s.Files {
		o, err := r.objects(s, f)
		if err != nil {
			return undo{}, "", err
		}
		objs = append(objs, o...)
	}
	// Only what this run creates is undone by deleting it: an object that
	// was there is updated in place, its earlier content not kept, which
	// the undo says of a Secret.
	var u undo
	for _, o := range objs {
		existed, err := r.Cluster.Exists(ctx, o)
		if err != nil {
			return u, "", applyError(o, s.Secret, err)
		}
		if err := r.Cluster.Apply(ctx, o); err != nil {
			return u, "", applyError(o, s.Secret, err)
		}
		if existed {
			r.logf("patchy: applied %s (updated in place)\n", describe(o))
			if s.Secret {
				u.notes = append(u.notes, describe(o)+" was replaced in place; its earlier content is not kept")
			}
			continue
		}
		u.commands = append([]string{deleteCommand(s.Conn, o)}, u.commands...)
		r.logf("patchy: applied %s (created)\n", describe(o))
	}
	return u, "", nil
}

// helm runs one upgrade, recording the revision it replaces for the undo.
func (r Runner) helm(ctx context.Context, s previewsetup.Step) (undo, error) {
	h := *s.Helm
	rev, err := r.Helm.Revision(ctx, h.Release, h.Namespace)
	if err != nil {
		return undo{}, fmt.Errorf("read release %s in namespace %s: %w", h.Release, h.Namespace, err)
	}
	helm := s.Conn.Helm()
	u := undo{commands: []string{fmt.Sprintf("%s rollback %s %d -n %s", helm, h.Release, rev, h.Namespace)}}
	if rev == 0 {
		u.commands = []string{fmt.Sprintf("%s uninstall %s -n %s", helm, h.Release, h.Namespace)}
	}
	return u, r.Helm.Run(ctx, previewsetup.HelmArgs(h, s.Timeout))
}

func (r Runner) read(path string) ([]byte, error) {
	if r.ReadFile != nil {
		return r.ReadFile(path)
	}
	f, err := os.Open(path) //nolint:gosec // a file this command wrote, named by the plan
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxManifest+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > maxManifest {
		return nil, fmt.Errorf("%s is larger than %d bytes: not a file this command wrote", path, maxManifest)
	}
	return data, nil
}

func (r Runner) logf(format string, args ...any) {
	if r.Log != nil {
		_, _ = fmt.Fprintf(r.Log, format, args...)
	}
}
