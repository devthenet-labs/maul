// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package templates

import (
	"fmt"
	"strings"

	"github.com/devthenet-labs/maul/internal/untrusted"
)

// FixDiffCut is what the fix diff a round after a fix quotes leaves out of
// the fix (code review 2026-11-18, agent-prompts 8). The evaluator reads
// the diff for special-casing, so a diff it has not seen whole is said to
// be so, beside the diff and in maul's own words, never left for it to
// take as the whole fix. The zero value: the diff is the whole fix.
type FixDiffCut struct {
	// ShownBytes of ReadBytes: of the diff maul read, made visible, the
	// bytes the prompt quotes.
	ShownBytes, ReadBytes int
	// ForgeCut are the repositories whose diff the forge's compare had
	// already cut when maul read it: the fix there is larger still.
	ForgeCut []string
	// Files are the files the diff does not show in full, as it names them
	// (UNTRUSTED: the pull request's), and MoreFiles counts those past
	// FixDiffFilesMaxBytes.
	Files     []string
	MoreFiles int
	// Unavailable are the repositories whose head moved but whose diff
	// maul could not read: none of their fix is shown.
	Unavailable []string
}

// whole reports a cut that leaves nothing out.
func (c FixDiffCut) whole() bool {
	return c.ShownBytes >= c.ReadBytes && len(c.ForgeCut) == 0 && len(c.Files) == 0 && c.MoreFiles == 0 &&
		len(c.Unavailable) == 0
}

// fixDiffView is the fix diff as the prompt quotes it: the diff made
// visible within FixDiffMaxBytes, maul's note of what it leaves out ("" for
// a whole diff) and the files it names as not shown in full. A diff the
// bound cuts here is said to be cut, as a controller's cut is: the prompt
// either quotes the whole diff or says it does not.
func fixDiffView(diff string, cut FixDiffCut) (block, note, files string) {
	if strings.TrimSpace(diff) != "" {
		block = strings.Trim(untrusted.Visible(diff, FixDiffMaxBytes), "\n")
		if read := len(untrusted.Escape(diff)); read > FixDiffMaxBytes {
			cut.ShownBytes, cut.ReadBytes = len(block), max(cut.ReadBytes, read)
		}
	}
	files, more := fixDiffFiles(cut.Files)
	cut.MoreFiles += more
	if cut.whole() {
		return block, "", ""
	}
	return block, fixDiffNote(cut, block != "", files != ""), files
}

// FixDiffWhole reports whether the prompt quotes diff as the whole fix:
// nothing of it is left out by cut, and the prompt's own bound
// (FixDiffMaxBytes) does not cut it either, so no note of what is missing
// is rendered. A change-focused round needs a whole diff, since its light
// look at an outcome rests on reading everything the fix changed.
func FixDiffWhole(diff string, cut FixDiffCut) bool {
	_, note, _ := fixDiffView(diff, cut)
	return note == ""
}

// fixDiffFiles lists files one per line, each made visible within
// FixDiffPathMaxBytes, at most FixDiffFilesMaxBytes in all; more counts
// the files left out.
func fixDiffFiles(files []string) (list string, more int) {
	var b strings.Builder
	for i, f := range files {
		line := quotedLine(f, FixDiffPathMaxBytes) + "\n"
		if b.Len()+len(line) > FixDiffFilesMaxBytes {
			return strings.TrimSuffix(b.String(), "\n"), len(files) - i
		}
		b.WriteString(line)
	}
	return strings.TrimSuffix(b.String(), "\n"), 0
}

// fixDiffNote is maul's statement that the evaluator has not seen the
// whole fix, and what of it is missing: a probe with data of its own, never
// a verdict, is what it asks for.
func fixDiffNote(cut FixDiffCut, shown, listed bool) string {
	var b strings.Builder
	b.WriteString("**You have not seen the whole fix.**")
	if shown && cut.ShownBytes < cut.ReadBytes {
		fmt.Fprintf(&b, " maul shows the first %d of the %d bytes of the diff it read.", cut.ShownBytes,
			cut.ReadBytes)
	}
	if len(cut.ForgeCut) > 0 {
		fmt.Fprintf(&b, " GitHub had already cut the diff of %s when maul read it, so the fix there is larger "+
			"still.", fixDiffRepos(cut.ForgeCut))
	}
	switch {
	case listed && cut.MoreFiles > 0:
		fmt.Fprintf(&b, " The files listed after the diff, and %d more, are not shown in full.", cut.MoreFiles)
	case listed:
		b.WriteString(" The files listed after the diff are not shown in full.")
	case cut.MoreFiles > 0:
		fmt.Fprintf(&b, " %d %s not shown in full.", cut.MoreFiles, plural(int32(cut.MoreFiles), "file is",
			"files are"))
	}
	if len(cut.Unavailable) > 0 {
		fmt.Fprintf(&b, " maul could not read the diff of %s since the heads the last round evaluated: none of "+
			"that fix is shown.", fixDiffRepos(cut.Unavailable))
	}
	b.WriteString(" Finding no special-casing in what is shown establishes nothing about the rest: never report " +
		"that the fix has none. Judge every outcome the change since the last round may touch with data of your " +
		"own the fix could not have foreseen.")
	return b.String()
}

// fixDiffRepos names repositories as code, at most fixDiffReposMax, the
// rest counted.
func fixDiffRepos(repos []string) string {
	var names []string
	for i, r := range repos {
		if i == fixDiffReposMax {
			names = append(names, fmt.Sprintf("%d more", len(repos)-i))
			break
		}
		names = append(names, code(quotedLine(r, 100)))
	}
	return strings.Join(names, ", ")
}
