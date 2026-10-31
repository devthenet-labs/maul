// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package report

import (
	"regexp"
	"slices"

	"go.yaml.in/yaml/v3"
)

// Bounds of FrontmatterKeyPaths.
const (
	// FrontmatterKeyPathsMax is the most key paths it returns.
	FrontmatterKeyPathsMax = 40
	// FrontmatterKeyPathsMaxBytes bounds the paths joined by ", ".
	FrontmatterKeyPathsMaxBytes = 600
	// keyPathDepth is the deepest a path is followed.
	keyPathDepth = 4
)

// plainKey is a key a path shows as written: anything else is shown as ?,
// so a path is maul's own text whatever key the agent wrote.
var plainKey = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

// FrontmatterKeyPaths describes a report's frontmatter by its key paths,
// never its values: "verdicts", "verdicts[].outcome", one per distinct
// path in document order, list items folded into [], at most keyPathDepth
// keys deep, FrontmatterKeyPathsMax paths and FrontmatterKeyPathsMaxBytes
// joined. It is for diagnosing a refused report without its text (S1 bug 5:
// the refused report is not kept), so it returns nil for a document with
// no frontmatter, or one that is not YAML.
func FrontmatterKeyPaths(data []byte) []string {
	block, _, err := splitFrontmatter(data)
	if err != nil {
		return nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(block, &doc); err != nil || len(doc.Content) == 0 {
		return nil
	}
	w := keyWalk{}
	w.walk(doc.Content[0], "", 0)
	return w.paths
}

// keyWalk collects key paths within the bounds.
type keyWalk struct {
	paths []string
	bytes int
	full  bool
}

func (w *keyWalk) walk(n *yaml.Node, prefix string, depth int) {
	if w.full || depth == keyPathDepth {
		return
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := "?"
			if k := n.Content[i]; k.Kind == yaml.ScalarNode && plainKey.MatchString(k.Value) {
				key = k.Value
			}
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			w.add(path)
			w.walk(n.Content[i+1], path, depth+1)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			w.walk(c, prefix+"[]", depth)
		}
	}
}

// add records path once, unless the bounds are reached.
func (w *keyWalk) add(path string) {
	if slices.Contains(w.paths, path) {
		return
	}
	cost := len(path)
	if len(w.paths) > 0 {
		cost += len(", ")
	}
	if len(w.paths) == FrontmatterKeyPathsMax || w.bytes+cost > FrontmatterKeyPathsMaxBytes {
		w.full = true
		return
	}
	w.paths = append(w.paths, path)
	w.bytes += cost
}
