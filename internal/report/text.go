// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Bounds shared by the intent reports (plan and build).
const (
	// SummaryMaxChars bounds a report's one-line summary, in characters. It
	// is the Intent status field's bound, and the summary is also what the
	// intent controller composes the pushed commit's subject from.
	SummaryMaxChars = 200
	// ItemMaxChars bounds one list item or other short free-text value, in
	// characters.
	ItemMaxChars = 500
	// BodyMaxBytes bounds the markdown after an intent report's frontmatter:
	// it is posted to GitHub as an issue comment or a pull-request body.
	BodyMaxBytes = 48 << 10
	// ReportMaxBytes bounds a whole intent report, frontmatter included. It
	// is sized to what a plan's approval comment can carry, so that a plan
	// this package accepts is never one GitHub cannot show its approver:
	// the comment holds the report verbatim, in a code block, beneath
	// maul's header, and GitHub refuses a comment over 65,536 characters.
	// The 8 KiB this bound leaves is the header's worst case with room to
	// spare: its own text (under 2 KiB with the longest names and labels),
	// the summary it repeats (at most 800 bytes), the new dependencies it
	// repeats (PlanMaxNewDependencies of at most DependencyMaxBytes, each
	// fenced as code) and the code block's fences — the reason a plan holds
	// no run of more than PlanMaxBacktickRun backticks, which would lengthen
	// every fence past it. The run status field that records a report holds
	// 64 KiB.
	ReportMaxBytes = 56 << 10
)

// checkDocument applies the bounds every intent report shares before any of
// it is parsed: the whole document's size, then what it may hold
// (checkVisible). Invalid UTF-8 and invisible characters are refused rather
// than repaired, because the report's exact bytes are what the plan's
// approval digest is taken over — a repair would change what was approved,
// and JSON would silently rewrite invalid UTF-8 on the way out of the pod.
// How a report lays its text out (checkLayout) only the plan is held to:
// only the plan is read verbatim, in a code block, by the human approving
// what the build will do.
func checkDocument(kind string, data []byte) error {
	if len(data) > ReportMaxBytes {
		return fmt.Errorf("report: %s: %d bytes, over the %d-byte bound", kind, len(data), ReportMaxBytes)
	}
	return checkVisible(kind, data)
}

// checkBody applies the body bound.
func checkBody(kind, body string) error {
	if len(body) > BodyMaxBytes {
		return fmt.Errorf("report: %s: body is %d bytes, over the %d-byte bound", kind, len(body), BodyMaxBytes)
	}
	return nil
}

// oneLine trims s and checks it is a non-empty single line of at most
// maxChars characters holding no rune that hidden names: no line break,
// tab or other control character (a terminal escape, NEL), no Unicode line
// or paragraph separator, and nothing that renders invisibly or reorders
// text (a bidi override, a zero-width space, a variation selector, a tag
// character). checkVisible has already refused such a rune written into
// the document; this catches one a YAML escape spells ("\u200b"), which
// the document shows as visible text but the decoded value holds. And s
// must be valid UTF-8: an invalid byte decodes to U+FFFD, which no rune
// check flags. decodeFrontmatter already refuses the explicit tag that can
// decode to such bytes (!!binary), but a value that is not text would be
// rewritten on its way to JSON, so each field holds the rule itself. These
// values reach GitHub and later prompts, and a summary becomes a commit
// subject, so they carry nothing a reader cannot see and break nowhere.
func oneLine(field, s string, maxChars int) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return s, fmt.Errorf("%s is required", field)
	case !utf8.ValidString(s):
		return s, fmt.Errorf("%s is not valid UTF-8", field)
	case utf8.RuneCountInString(s) > maxChars:
		return s, fmt.Errorf("%s is %d characters, over %d", field, utf8.RuneCountInString(s), maxChars)
	}
	if i := strings.IndexFunc(s, invisible); i >= 0 {
		r, _ := utf8.DecodeRuneInString(s[i:])
		return s, fmt.Errorf("%s holds U+%04X, %s, where one line of visible text is required", field, r, hidden(r))
	}
	return s, nil
}

// lines applies oneLine to every item of a list of at most maxItems,
// trimming each in place.
func lines(field string, items []string, maxItems, maxChars int) error {
	if len(items) > maxItems {
		return fmt.Errorf("%s has %d items, over %d", field, len(items), maxItems)
	}
	var errs []error
	for i := range items {
		var err error
		if items[i], err = oneLine(fmt.Sprintf("%s[%d]", field, i), items[i], maxChars); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// freeTextLine captures a mapping key and its value, and listItemLine a
// sequence item, as a YAML frontmatter lays them out one per line.
var (
	freeTextLine = regexp.MustCompile(`^([ \t]*([A-Za-z_]+):[ \t]+)(.*)$`)
	listItemLine = regexp.MustCompile(`^([ \t]*-[ \t]+)(.*)$`)
)

// repairFreeText double-quotes the unquoted values of the named free-text
// keys, and of every sequence item, in a frontmatter block, on the lines
// only names (every line when only is nil). It is the intent reports'
// counterpart of repairSummaries: models write these values as plain prose,
// and prose holding a colon ("Question: which store?") is not a valid plain
// scalar — in a list it even parses as a mapping. Quoting a plain scalar
// that was already valid keeps its string value, so this is safe only over
// prose and lists of it: decodeRepairing confines it to the top-level keys
// a report declares so (repairWithin), attempts it only after a strict
// parse has failed, and surfaces the original error if the repaired block
// fails too. A null (yamlNull) is the exception: quoting it would turn "no
// value" into the string "null", so it is left as it is.
func repairFreeText(block []byte, keys map[string]bool, only map[int]bool) ([]byte, bool) {
	lines := strings.Split(string(block), "\n")
	changed := false
	for i, line := range lines {
		if only != nil && !only[i] {
			continue
		}
		var prefix, val string
		if m := listItemLine.FindStringSubmatch(line); m != nil {
			prefix, val = m[1], m[2]
		} else if m := freeTextLine.FindStringSubmatch(line); m != nil && keys[m[2]] {
			prefix, val = m[1], m[3]
		} else {
			continue
		}
		val = strings.TrimRight(val, " \t\r")
		if val == "" || yamlNull(val) {
			// A null stays null: quoted, "~" or "null" would become that
			// string, and a reason a model left out that way would then read
			// as given.
			continue
		}
		switch val[0] {
		case '"', '\'', '|', '>', '&', '*', '[', '{', '!':
			// Already quoted, a block or flow collection, anchored, an alias
			// or tagged — leave it.
			continue
		}
		val = strings.ReplaceAll(val, `\`, `\\`)
		val = strings.ReplaceAll(val, `"`, `\"`)
		lines[i] = prefix + `"` + val + `"`
		changed = true
	}
	if !changed {
		return block, false
	}
	return []byte(strings.Join(lines, "\n")), true
}

// repairWithin is repairFreeText confined to the top-level keys in scope:
// those whose whole value is prose or a list of it. Every other value — a
// plan's acceptance criteria, whose lists hold numbers, booleans and
// literals the approver read as written — keeps its exact bytes, so the
// repair never retypes what was approved; a document it cannot confine so
// is not repaired at all, and its strict error stands.
//
// The block, which does not parse, is quoted on the lines its layout puts
// under a key in scope (candidateLines), and the result is parsed to prove
// every line that changed belongs to a key in scope there: which key a line
// belongs to is the parser's call, never the layout's alone. A document
// with an anchor or alias is refused, since one lets text in scope be read
// out of it; so is one whose top level is not a block mapping of one key
// per line.
func repairWithin(block []byte, keys, scope map[string]bool) ([]byte, bool) {
	repaired, changed := repairFreeText(block, keys, candidateLines(block, scope))
	if !changed {
		return nil, false
	}
	final, ok := scopeLines(repaired, scope)
	if !ok {
		return nil, false
	}
	before, after := strings.Split(string(block), "\n"), strings.Split(string(repaired), "\n")
	for i := range after {
		if after[i] != before[i] && !final[i] {
			return nil, false
		}
	}
	return repaired, true
}

// topKeyLine matches a top-level mapping key as a frontmatter lays it out:
// at the start of its line.
var topKeyLine = regexp.MustCompile(`^([A-Za-z_]+):(?:[ \t]|$)`)

// candidateLines returns the indexes (from zero) of the lines block's
// layout puts under a top-level key in scope: from a key's line to the next
// top-level key's. It only proposes; repairWithin proves by parsing.
func candidateLines(block []byte, scope map[string]bool) map[int]bool {
	lines := map[int]bool{}
	in := false
	for i, line := range strings.Split(string(block), "\n") {
		if m := topKeyLine.FindStringSubmatch(line); m != nil {
			in = scope[m[1]]
		}
		if in {
			lines[i] = true
		}
	}
	return lines
}

// scopeLines parses block and returns the indexes (from zero) of its lines
// that belong to a top-level key in scope: from the key's line to the line
// before the next key, or to the block's end. It reports false when that
// cannot be said safely: the block does not parse as one block mapping, two
// keys share a line, or any node carries an anchor or is an alias.
func scopeLines(block []byte, scope map[string]bool) (map[int]bool, bool) {
	var doc yaml.Node
	if err := yaml.Unmarshal(block, &doc); err != nil || len(doc.Content) != 1 {
		return nil, false
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode || root.Style&yaml.FlowStyle != 0 || referential(root) {
		return nil, false
	}
	total := strings.Count(string(block), "\n") + 1
	lines := map[int]bool{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		start, end := root.Content[i].Line-1, total
		if i+2 < len(root.Content) {
			end = root.Content[i+2].Line - 1
		}
		if end <= start {
			return nil, false
		}
		if !scope[root.Content[i].Value] {
			continue
		}
		for l := start; l < end; l++ {
			lines[l] = true
		}
	}
	return lines, true
}

// referential reports whether any node of the tree under n carries an
// anchor or is an alias.
func referential(n *yaml.Node) bool {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return true
	}
	for _, c := range n.Content {
		if referential(c) {
			return true
		}
	}
	return false
}

// yamlNull reports whether a plain value is one of YAML's null spellings,
// alone or before a comment.
func yamlNull(val string) bool {
	if i := strings.IndexAny(val, " \t"); i >= 0 {
		if rest := strings.TrimLeft(val[i:], " \t"); rest != "" && rest[0] != '#' {
			return false
		}
		val = val[:i]
	}
	switch val {
	case "~", "null", "Null", "NULL":
		return true
	}
	return false
}

// decodeRepairing decodes an intent report's frontmatter block into out
// (decodeFrontmatter), retrying once when the decode fails over
// repairWithin's quoting of the free-text keys and list items under the
// top-level keys in scope. reset zeroes out before the retry. The original
// error is the one reported.
func decodeRepairing(block []byte, out any, keys, scope map[string]bool, reset func()) error {
	err := decodeFrontmatter(block, out)
	if err == nil {
		return nil
	}
	repaired, ok := repairWithin(block, keys, scope)
	if !ok {
		return err
	}
	reset()
	if rerr := decodeFrontmatter(repaired, out); rerr != nil {
		return err
	}
	return nil
}

// decodeFrontmatter decodes an intent report's frontmatter block strictly
// (decodeStrict), once it is held to what its reader sees: each value is
// the text the document shows. So the block is one YAML document — after a
// document end marker ("...") text may follow that no field holds and no
// bound covers, which a strict decode of the first document never reads —
// and no node carries an explicit tag. A tag makes a value something other
// than its text: !!binary decodes base64 into whatever bytes it encodes,
// invalid UTF-8 included, so the approver would read base64 in the plan
// while the controller recorded the decoded value. No field of these
// schemas needs one. The non-specific tag "!" is not refused: it keeps a
// value the string it is written as.
//
// The Finding flow's reports keep decodeStrict alone.
func decodeFrontmatter(block []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(block))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("report: frontmatter: %w", err)
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return errors.New("report: frontmatter: more follows the end of its YAML document; " +
			"the frontmatter is one document, ended only by the closing --- fence")
	}
	if n := taggedNode(&doc); n != nil {
		return fmt.Errorf("report: frontmatter: line %d: a value tagged %s; "+
			"write every value as plain YAML, with no explicit tag", n.Line, n.Tag)
	}
	return decodeStrict(block, out)
}

// taggedNode returns the first node of the tree under n written with an
// explicit tag, or nil. It walks Content alone: an alias's target is a node
// of the tree, visited where it stands.
func taggedNode(n *yaml.Node) *yaml.Node {
	if n.Style&yaml.TaggedStyle != 0 {
		return n
	}
	for _, c := range n.Content {
		if t := taggedNode(c); t != nil {
			return t
		}
	}
	return nil
}
