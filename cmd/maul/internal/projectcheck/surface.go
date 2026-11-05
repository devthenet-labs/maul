// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"fmt"
	"strings"
	"time"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/cmd/maul/internal/checkreport"
	"github.com/devthenet-labs/maul/internal/netprofile"
)

// The keys of intent-controller's network profile settings (the chart's
// maul.networkProfilesData), the same ones it launches builds with.
const (
	keyNetworkProfilesEnabled = "MAUL_NETWORK_PROFILES_ENABLED"
	keyNetworkProfiles        = "MAUL_NETWORK_PROFILES"
)

// surfaceInfo is the agents-namespace surface (a cli or library) of a
// Project, as far as it could be resolved.
type surfaceInfo struct {
	kind string
	// key is the surface repository's key, empty when it did not resolve.
	key string
	url string
}

// surface checks a cli or library surface's shape and resolves its
// repository. A Project without one adds nothing, so S1 reports are
// unchanged. The returned surface is nil without one.
func (r *run) surface() *surfaceInfo {
	s := r.p.Spec.Surfaces
	if !s.Agents() {
		return nil
	}
	info := &surfaceInfo{kind: "library"}
	repoKey := ""
	if s.CLI != nil {
		info.kind, repoKey = "cli", s.CLI.Repository
	} else {
		repoKey = s.Library.Repository
	}
	if reason := r.surfaceRefusal(s); reason != "" {
		r.add(CheckSurface, "", checkreport.Fail, "%s", reason)
		return info
	}
	repo, reason := r.surfaceRepository(repoKey)
	if repo == nil {
		r.add(CheckSurface, "", checkreport.Fail, "%s", reason)
		return info
	}
	info.key, info.url = repo.Name, repo.URL
	what := info.kind
	if s.Library != nil {
		what += " (" + string(s.Library.Language) + ")"
	}
	r.add(CheckSurface, info.key, checkreport.Pass, "the %s surface is evaluated in the agents namespace: the "+
		"tool pod builds %s at the pull request's head and its merge base in the repository's accepted runner image",
		what, info.key)
	return info
}

// surfaceRefusal is why the surfaces as a whole are not evaluable, "" when
// they are.
func (r *run) surfaceRefusal(s *v1alpha1.ProjectSurfaces) string {
	p := r.p
	switch {
	case s.CLI != nil && s.Library != nil:
		return "a Project has at most one of surfaces.cli and surfaces.library"
	case s.Browser != nil:
		return v1alpha1.ReasonSurfaceCombinationUnsupported + ": surfaces.browser drives a preview, and a Project " +
			"with a cli or library surface has none"
	case p.Spec.Preview != nil || hasRepositoryPreview(p):
		return v1alpha1.ReasonSurfaceCombinationUnsupported + ": the Project sets a preview beside its cli or " +
			"library surface; one host is evaluated per Project, so remove the preview or the surface"
	case s.Library != nil && s.Library.Language != v1alpha1.LibraryLanguageGo:
		return fmt.Sprintf("surfaces.library.language is %q; only %q is accepted", s.Library.Language,
			v1alpha1.LibraryLanguageGo)
	}
	return ""
}

func hasRepositoryPreview(p *v1alpha1.Project) bool {
	for _, repo := range p.Spec.Repositories {
		if repo.Preview != nil {
			return true
		}
	}
	return false
}

// surfaceRepository resolves the surface's repository key: the one named,
// else the Project's only repository.
func (r *run) surfaceRepository(key string) (*v1alpha1.ProjectRepository, string) {
	repos := r.p.Spec.Repositories
	if key == "" {
		if len(repos) == 1 {
			return &repos[0], ""
		}
		return nil, fmt.Sprintf("the surface names no repository, and the Project has %d (set the surface's "+
			"repository)", len(repos))
	}
	names := make([]string, 0, len(repos))
	for i := range repos {
		if repos[i].Name == key {
			return &repos[i], ""
		}
		names = append(names, repos[i].Name)
	}
	return nil, fmt.Sprintf("the surface's repository %q is not in spec.repositories (%s)", key,
		strings.Join(names, ", "))
}

// surfaceBuild checks a cli surface's build argv, binary and timeout, which
// the schema also enforces: this is for a Project applied before the CRD
// was current, and says which rule is broken.
func (r *run) surfaceBuild(info *surfaceInfo) {
	cli := r.p.Spec.Surfaces.CLI
	if cli == nil {
		return
	}
	if reason := buildRefusal(cli.Build); reason != "" {
		r.add(CheckSurfaceBuild, info.key, checkreport.Fail, "surfaces.cli.build: %s", reason)
		return
	}
	if !v1alpha1.ValidCLIBinary(cli.Binary) {
		r.add(CheckSurfaceBuild, info.key, checkreport.Fail, "surfaces.cli.binary %q is not an absolute path "+
			"under %s with at most two names below it", cli.Binary, v1alpha1.CLIBinDir)
		return
	}
	if t := cli.BuildTimeout; t != nil && (t.Duration < time.Minute || t.Duration > v1alpha1.MaxCLIBuildTimeout) {
		r.add(CheckSurfaceBuild, info.key, checkreport.Fail, "surfaces.cli.buildTimeout %s is outside 1m to %s",
			t.Duration, v1alpha1.MaxCLIBuildTimeout)
		return
	}
	r.add(CheckSurfaceBuild, info.key, checkreport.Pass, "the build (%d arguments) writes under %s, the binary is "+
		"%s, and each tree's build is bounded by %s", len(cli.Build), v1alpha1.CLIBinDir, cli.Binary, cli.Timeout())
}

// buildRefusal says which rule of v1alpha1.ValidCLIBuild argv breaks, ""
// when it breaks none.
func buildRefusal(argv []string) string {
	if len(argv) == 0 || len(argv) > v1alpha1.MaxCLIBuildArgs {
		return fmt.Sprintf("1 to %d arguments are needed, not %d", v1alpha1.MaxCLIBuildArgs, len(argv))
	}
	for i, a := range argv {
		switch {
		case a == "":
			return fmt.Sprintf("argument %d is empty", i+1)
		case len(a) > v1alpha1.MaxCLIBuildArgBytes:
			return fmt.Sprintf("argument %d is over %d bytes", i+1, v1alpha1.MaxCLIBuildArgBytes)
		case strings.ContainsFunc(a, func(c rune) bool { return c < ' ' || c == 0x7f }):
			return fmt.Sprintf("argument %d holds a control character", i+1)
		}
	}
	if !v1alpha1.ValidCLIBuild(argv) {
		return fmt.Sprintf("no argument names %s as a directory (%s/<name> or %s), so the build writes nowhere "+
			"the tool pod reads", v1alpha1.CLIBinDir, v1alpha1.CLIBinDir, v1alpha1.CLIBinDir)
	}
	return ""
}

// surfaceProfile reports the network profile the surface repository's build
// gets from intent-controller's own settings, which is the profile the tool
// pod gets too: its network is never wider than the build's.
func (r *run) surfaceProfile(info *surfaceInfo, s settings) {
	c := s.Intent
	switch {
	case c.Err != nil || !c.Found():
		r.add(CheckSurfaceProfile, info.key, checkreport.Skip, "cannot read intent-controller's settings, so "+
			"the build's network profile is unknown: %v", orNoConfigMap(c.Err))
		return
	case !c.Bool(keyNetworkProfilesEnabled):
		r.add(CheckSurfaceProfile, info.key, checkreport.Pass, "no network profile applies (%s is not true in "+
			"%s): the build and the tool pod reach only the artifact server, so the build must need no download",
			keyNetworkProfilesEnabled, c.From)
		return
	}
	set, err := netprofile.Parse([]byte(c.Data[keyNetworkProfiles]))
	if err != nil {
		r.add(CheckSurfaceProfile, info.key, checkreport.Fail, "%s in %s does not parse, so intent-controller "+
			"refuses to start: %v", keyNetworkProfiles, c.From, err)
		return
	}
	prof, why := set.For(info.url)
	switch why {
	case "":
		r.add(CheckSurfaceProfile, info.key, checkreport.Pass, "the build and the tool pod get profile %s "+
			"(route %q): the tool pod's network is never wider than the build's", prof.Name, prof.Route)
	case netprofile.ReasonAmbiguous:
		r.add(CheckSurfaceProfile, info.key, checkreport.Fail, "more than one network profile matches %s "+
			"(ambiguous), so none is applied and the build runs with no download route; narrow the profiles' "+
			"repository patterns", info.url)
	case netprofile.ReasonBadURL:
		r.add(CheckSurfaceProfile, info.key, checkreport.Fail, "%s is not a repository URL a profile can match "+
			"(%s)", info.url, why)
	default:
		r.add(CheckSurfaceProfile, info.key, checkreport.Pass, "no network profile applies to %s (%s): the build "+
			"and the tool pod reach only the artifact server, so the build must need no download", info.url, why)
	}
}

func orNoConfigMap(err error) any {
	if err != nil {
		return err
	}
	return "no intent-controller ConfigMap found"
}
