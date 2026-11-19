// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/devthenet-labs/maul/internal/agentrun"
	"github.com/devthenet-labs/maul/internal/ghclient"
	"github.com/devthenet-labs/maul/internal/templates"
	"github.com/devthenet-labs/maul/internal/untrusted"
)

// fixDiffPart is one moved head's compare since the heads the last round
// evaluated: GitHub's patch (UNTRUSTED: the pull request's), or
// unavailable when the compare failed.
type fixDiffPart struct {
	repo, base, head, patch string
	unavailable             bool
}

// fixDiffFile is one file the diff names, and where its part of the diff
// ends.
type fixDiffFile struct {
	path string
	end  int
}

// boundFixDiff is the fix diff a round after a fix hands the evaluator
// (code review 2026-11-18, agent-prompts 8): each readable part under its
// repository's name, made visible, and what it leaves out of the fix, nil
// when it leaves out nothing. The evaluator reads the diff for
// special-casing, so nothing is left out silently: a diff past
// FixDiffMaxBytes is cut at the last repository, commit, file or hunk
// boundary within its bound (else at a line's end), keeping room for the
// files it names as not shown in full (those cut through, those never
// reached, and the last of a diff GitHub had already cut); a compare that
// failed names its repository as unavailable. With more than one
// repository, a file is named under its repository's ("acme/api: x.go").
// The diff and the files it names stay within FixDiffMaxBytes together.
func boundFixDiff(parts []fixDiffPart) (string, *agentrun.FixDiffCut) {
	var b strings.Builder
	var files []fixDiffFile
	var stops []int
	cut := &agentrun.FixDiffCut{}
	for _, p := range parts {
		if p.unavailable {
			cut.Unavailable = append(cut.Unavailable, p.repo)
			continue
		}
		patch, forge := strings.CutSuffix(p.patch, ghclient.PatchTruncated)
		start := b.Len()
		b.WriteString(untrusted.Escape(fmt.Sprintf("# %s %s...%s\n%s\n", p.repo, p.base, p.head,
			strings.TrimRight(patch, "\n"))))
		var prefix string
		if len(parts) > 1 {
			prefix = untrusted.Escape(p.repo) + ": "
		}
		mine := scanFixDiff(b.String()[start:], start, prefix, &stops)
		if forge {
			// The forge cut its last file: never shown in full.
			cut.ForgeCut = append(cut.ForgeCut, p.repo)
			if len(mine) > 0 {
				mine[len(mine)-1].end = math.MaxInt
			}
		}
		files = append(files, mine...)
	}
	full := b.String()
	shown := full
	if names, _ := fixDiffNames(notShownWhole(files, len(full))); len(full)+namesBytes(names) >
		templates.FixDiffMaxBytes {
		shown = full[:fixDiffCutAt(full, stops, templates.FixDiffMaxBytes-templates.FixDiffFilesMaxBytes)]
	}
	cut.ShownBytes, cut.ReadBytes = len(shown), len(full)
	cut.Files, cut.MoreFiles = fixDiffNames(notShownWhole(files, len(shown)))
	if cut.ShownBytes == cut.ReadBytes && len(cut.Files) == 0 && cut.MoreFiles == 0 && len(cut.ForgeCut) == 0 &&
		len(cut.Unavailable) == 0 {
		return full, nil
	}
	return shown, cut
}

// scanFixDiff reads one part of the diff (text, at offset at of the
// whole): it adds where a cut may stop (the part, and each commit, file and
// hunk it starts) to stops, and returns the files it names, after prefix,
// each ending where the next file or commit starts, or with the part.
func scanFixDiff(text string, at int, prefix string, stops *[]int) []fixDiffFile {
	var files []fixDiffFile
	closeLast := func(end int) {
		if n := len(files); n > 0 && files[n-1].end < 0 {
			files[n-1].end = end
		}
	}
	*stops = append(*stops, at)
	o := at
	for line := range strings.SplitAfterSeq(text, "\n") {
		switch {
		case strings.HasPrefix(line, "From "):
			closeLast(o)
			*stops = append(*stops, o)
		case strings.HasPrefix(line, "diff --git "):
			*stops = append(*stops, o)
			if path, ok := fixDiffHeaderPath(line); ok {
				closeLast(o)
				files = append(files, fixDiffFile{path: prefix + path, end: -1})
			}
		case strings.HasPrefix(line, "@@"):
			*stops = append(*stops, o)
		}
		o += len(line)
	}
	closeLast(o)
	return files
}

// fixDiffHeaderPath is the file a "diff --git" line names (its new name),
// as the diff spells it: git quotes a name holding a non-ASCII character,
// a quote or a control character ("b/caf\303\251.go"), and the name is
// then what the quotes hold. ok is false for a line that is no file's
// header (a commit message's own text).
func fixDiffHeaderPath(line string) (path string, ok bool) {
	rest, _ := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "diff --git ")
	if !strings.HasPrefix(rest, "a/") && !strings.HasPrefix(rest, `"a/`) {
		return "", false
	}
	if i := strings.LastIndex(rest, ` "b/`); i >= 0 && strings.HasSuffix(rest, `"`) {
		return rest[i+4 : len(rest)-1], true
	}
	if i := strings.LastIndex(rest, " b/"); i >= 0 {
		return rest[i+3:], true
	}
	return "", false
}

// notShownWhole are the files whose part of the diff does not end within
// its first n bytes, each once, in order.
func notShownWhole(files []fixDiffFile, n int) []string {
	var out []string
	for _, f := range files {
		if f.end > n && !slices.Contains(out, f.path) {
			out = append(out, f.path)
		}
	}
	return out
}

// fixDiffNames bounds paths as the prompt lists them: each at most
// FixDiffPathMaxBytes, FixDiffFilesMaxBytes in all, the rest counted.
func fixDiffNames(paths []string) (names []string, more int) {
	var n int
	for i, p := range paths {
		p = untrusted.Cut(p, templates.FixDiffPathMaxBytes)
		if n+len(p)+1 > templates.FixDiffFilesMaxBytes {
			return names, len(paths) - i
		}
		names, n = append(names, p), n+len(p)+1
	}
	return names, 0
}

// namesBytes is the bytes names take listed one per line.
func namesBytes(names []string) int {
	var n int
	for _, p := range names {
		n += len(p) + 1
	}
	return n
}

// fixDiffCutAt is where diff is cut to at most limit bytes: the last stop
// within it, else the end of the last line within it, unless either leaves
// less than half of it; else the last character boundary.
func fixDiffCutAt(diff string, stops []int, limit int) int {
	for i := len(stops) - 1; i >= 0; i-- {
		if s := stops[i]; s <= limit && s >= limit/2 {
			return s
		}
	}
	if i := strings.LastIndexByte(diff[:limit], '\n'); i+1 >= limit/2 {
		return i + 1
	}
	return len(untrusted.Cut(diff, limit))
}
