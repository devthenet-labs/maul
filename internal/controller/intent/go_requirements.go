// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/report"
	"github.com/devthenet-labs/maul/internal/templates"
)

// goModPath is the one go.mod an intent pull request's body accounts for:
// the repository root's. Other ecosystems, and nested modules, are not.
const goModPath = "go.mod"

// goRequirements are the Go module requirements a build pushed that its
// base did not hold, each against the approved plan's new_dependencies, for
// the pull request's body: the go.mod at the build's base commit and at the
// commit it pushed, both read from GitHub. None (nil) when the pushed tree
// has no go.mod, either go.mod does not parse, GitHub refuses the read or
// cannot return it inline (a symlink, a directory, over 1 MiB), or nothing
// changed; only a transient error is returned, so the pass retries.
func (p *pass) goRequirements(ctx context.Context, run *v1alpha1.IntentRun) ([]templates.GoRequirement, error) {
	read := func(ref string) ([]byte, bool, error) {
		data, err := p.r.GitHub.FileContent(ctx, run.Spec.Repository.URL, goModPath, ref)
		switch {
		case ghclient.IsNotFound(err):
			return nil, false, nil
		case ghclient.IsRefused(err), errors.Is(err, ghclient.ErrNotInline):
			p.r.log().LogAttrs(ctx, slog.LevelWarn, "GitHub would not return go.mod; the pull request body omits "+
				"its requirements", slog.String("intent", p.in.Name), slog.String("ref", ref),
				slog.String("error", err.Error()))
			return nil, false, nil
		case err != nil:
			return nil, false, err
		}
		return data, true, nil
	}
	head, ok, err := read(run.Status.PushedCommit)
	if err != nil || !ok {
		return nil, err
	}
	base, _, err := read(run.Status.BaseSHA)
	if err != nil {
		return nil, err
	}
	var planned []string
	if pl := p.in.Status.Plan; pl != nil {
		raw, err := p.planBytes(ctx, pl)
		if err != nil {
			return nil, err
		}
		if parsed, err := report.ParsePlan(raw); err == nil {
			planned = parsed.NewDependencies
		}
	}
	reqs, err := goRequirementChanges(base, head, planned)
	if err != nil {
		p.r.log().LogAttrs(ctx, slog.LevelWarn, "a go.mod does not parse; the pull request body omits its "+
			"requirements", slog.String("intent", p.in.Name), slog.String("error", err.Error()))
		return nil, nil
	}
	return reqs, nil
}

// goRequirementChanges compares two go.mod files: each requirement of head
// that base does not hold at the same version, sorted by module path, with
// what the plan's new_dependencies say of it. A nil base is a repository
// with no go.mod before the build: every requirement is added.
func goRequirementChanges(base, head []byte, planned []string) ([]templates.GoRequirement, error) {
	was := map[string]string{}
	if base != nil {
		f, err := modfile.ParseLax(goModPath, base, nil)
		if err != nil {
			return nil, err
		}
		for _, r := range f.Require {
			was[r.Mod.Path] = r.Mod.Version
		}
	}
	f, err := modfile.ParseLax(goModPath, head, nil)
	if err != nil {
		return nil, err
	}
	var out []templates.GoRequirement
	for _, r := range f.Require {
		if v, ok := was[r.Mod.Path]; ok && v == r.Mod.Version {
			continue
		}
		req := templates.GoRequirement{Path: r.Mod.Path, Version: r.Mod.Version, Was: was[r.Mod.Path],
			Indirect: r.Indirect}
		req.InPlan, req.PlanVersion = plannedVersion(planned, r.Mod.Path)
		out = append(out, req)
	}
	slices.SortStableFunc(out, func(a, b templates.GoRequirement) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

// plannedVersion reports whether a new_dependencies entry names the module
// path as one of its words ("golang.org/x/mod v0.28.0",
// "golang.org/x/mod@v0.28.0"), and the first semantic version that entry
// names, "" for none.
func plannedVersion(planned []string, path string) (bool, string) {
	for _, entry := range planned {
		words := strings.FieldsFunc(entry, func(r rune) bool {
			return unicode.IsSpace(r) || strings.ContainsRune("@,;()[]`\"'", r)
		})
		if !slices.Contains(words, path) {
			continue
		}
		for _, w := range words {
			if semver.IsValid(w) {
				return true, w
			}
		}
		return true, ""
	}
	return false, ""
}
