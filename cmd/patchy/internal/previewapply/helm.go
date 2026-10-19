// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewapply

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
)

// ExecHelm is the Helm port over the helm binary, run with the caller's
// kubeconfig and context so it reaches the cluster the API client does.
type ExecHelm struct {
	// Binary is the helm command; "" is helm on PATH.
	Binary string
	// Kubeconfig and Context are the caller's --kubeconfig and --context,
	// passed on when set.
	Kubeconfig, Context string
	// Out receives an upgrade's own output (the caller's stderr).
	Out io.Writer
}

var _ Helm = ExecHelm{}

// maxHelmOutput bounds what is kept of a short helm command's output.
const maxHelmOutput = 1 << 20

func (h ExecHelm) binary() string {
	if h.Binary != "" {
		return h.Binary
	}
	return "helm"
}

// kubeArgs are the connection flags helm takes for the caller's.
func (h ExecHelm) kubeArgs() []string {
	var args []string
	if h.Kubeconfig != "" {
		args = append(args, "--kubeconfig", h.Kubeconfig)
	}
	if h.Context != "" {
		args = append(args, "--kube-context", h.Context)
	}
	return args
}

// output runs a short helm command and returns its standard output,
// bounded; on failure the error carries its standard error, trimmed.
func (h ExecHelm) output(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, h.binary(), args...) //nolint:gosec // helm, with the plan's own arguments
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limitWriter{w: &out, n: maxHelmOutput}, &limitWriter{w: &errOut, n: maxHelmOutput}
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errOut.String()); msg != "" {
			return nil, fmt.Errorf("helm %s: %w: %s", args[0], err, msg)
		}
		return nil, fmt.Errorf("helm %s: %w", args[0], err)
	}
	return out.Bytes(), nil
}

// Version is `helm version --short`.
func (h ExecHelm) Version(ctx context.Context) (string, error) {
	if _, err := exec.LookPath(h.binary()); err != nil {
		return "", fmt.Errorf("no helm command: %w", err)
	}
	out, err := h.output(ctx, "version", "--short")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// errReleaseNotFound is helm's answer for a release that is not there.
const errReleaseNotFound = "release: not found"

// Revision is the release's revision from `helm status -o json`, 0 when
// helm says it is not found.
func (h ExecHelm) Revision(ctx context.Context, release, namespace string) (int, error) {
	args := append([]string{"status", release, "-n", namespace, "-o", "json"}, h.kubeArgs()...)
	out, err := h.output(ctx, args...)
	if err != nil {
		if strings.Contains(err.Error(), errReleaseNotFound) {
			return 0, nil
		}
		return 0, err
	}
	var status struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return 0, fmt.Errorf("helm status %s: %w", release, err)
	}
	if status.Version <= 0 {
		return 0, fmt.Errorf("helm status %s: no revision in its answer", release)
	}
	return status.Version, nil
}

// Run runs helm with args and the caller's connection flags, its output
// streamed to Out.
func (h ExecHelm) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("no helm command")
	}
	cmd := exec.CommandContext(ctx, h.binary(), slices.Concat(args, h.kubeArgs())...) //nolint:gosec // the plan's
	out := h.Out
	if out == nil {
		out = io.Discard
	}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm %s %s: %w (its output is above)", args[0], firstRelease(args), err)
	}
	return nil
}

// firstRelease is the release an upgrade's arguments name.
func firstRelease(args []string) string {
	for _, a := range args[1:] {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// limitWriter keeps the first n bytes written and drops the rest, never
// failing the writer.
type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		keep := p
		if len(keep) > l.n {
			keep = keep[:l.n]
		}
		k, err := l.w.Write(keep)
		l.n -= k
		if err != nil {
			return len(p), nil //nolint:nilerr // a full buffer drops the rest; the command runs on
		}
	}
	return len(p), nil
}
