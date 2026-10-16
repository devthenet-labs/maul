// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Node isolation keeps hostile preview code on nodes of its own: every slot
// Pod selects the operator's preview nodes (a node selector) and tolerates
// their NoExecute taint, which keeps everything else off them. The selector
// is generic; the EKS Auto Mode NodePool and NodeClass flags fill two of its
// keys, so an install configured by them renders exactly what it always did.
const (
	// NodeIsolationRequired renders a node selector and a toleration on every
	// slot Pod, and refuses to start without both: the default.
	NodeIsolationRequired = "required"
	// NodeIsolationNone renders neither, an explicit opt-out for a cluster
	// with no dedicated preview nodes. Preview Pods then share nodes with
	// everything else.
	NodeIsolationNone = "none"

	// nodePoolLabel and nodeClassLabel are the keys the NodePool and
	// NodeClass settings fill: EKS Auto Mode's node labels.
	nodePoolLabel  = "karpenter.sh/nodepool"
	nodeClassLabel = "eks.amazonaws.com/nodeclass"
)

// ParseNodeSelector decodes the --preview-node-selector JSON object of label
// key to value. Empty input is no selector.
func ParseNodeSelector(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	var out map[string]string
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("preview node selector: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("preview node selector: trailing data after the JSON object")
	}
	return out, nil
}

// nodeSelector is the node selector every slot Pod renders: NodeSelector
// with NodePool and NodeClass filling their EKS keys, or nil when there is
// none.
func (s Settings) nodeSelector() map[string]string {
	if len(s.NodeSelector) == 0 && s.NodePool == "" && s.NodeClass == "" {
		return nil
	}
	out := maps.Clone(s.NodeSelector)
	if out == nil {
		out = make(map[string]string, 2)
	}
	if s.NodePool != "" {
		out[nodePoolLabel] = s.NodePool
	}
	if s.NodeClass != "" {
		out[nodeClassLabel] = s.NodeClass
	}
	return out
}

// validateNodeIsolation checks the isolation settings. Required (the
// default) needs a non-empty node selector and a taint key; none needs
// neither, so an operator's selector is never silently dropped. Every key
// is a qualified label name and every value a label value, the taint key a
// qualified name, and NodePool or NodeClass may not contradict a value the
// selector gives their key.
func (s Settings) validateNodeIsolation() error {
	for key, field := range map[string]string{nodePoolLabel: s.NodePool, nodeClassLabel: s.NodeClass} {
		if v, ok := s.NodeSelector[key]; ok && field != "" && v != field {
			return fmt.Errorf("the node selector's %s %q contradicts the configured %q", key, v, field)
		}
	}
	selector := s.nodeSelector()
	for _, key := range slices.Sorted(maps.Keys(selector)) {
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			return fmt.Errorf("node selector key %q: %s", key, strings.Join(errs, "; "))
		}
		if errs := validation.IsValidLabelValue(selector[key]); len(errs) > 0 {
			return fmt.Errorf("node selector value %q for %s: %s", selector[key], key, strings.Join(errs, "; "))
		}
	}
	if s.TaintKey != "" {
		if errs := validation.IsQualifiedName(s.TaintKey); len(errs) > 0 {
			return fmt.Errorf("taint key %q: %s", s.TaintKey, strings.Join(errs, "; "))
		}
	}
	switch s.NodeIsolation {
	case "", NodeIsolationRequired:
		if len(selector) == 0 || s.TaintKey == "" {
			return fmt.Errorf("node isolation is required: set a node selector (--preview-node-selector, or " +
				"--preview-node-pool and --preview-node-class) and a taint key (--preview-taint-key), or set " +
				"--preview-node-isolation=none")
		}
	case NodeIsolationNone:
		if len(selector) > 0 || s.TaintKey != "" {
			return fmt.Errorf("node isolation is none, but a node selector or taint key is set: unset them, " +
				"or require isolation")
		}
	default:
		return fmt.Errorf("node isolation %q is neither %q nor %q", s.NodeIsolation, NodeIsolationRequired,
			NodeIsolationNone)
	}
	return nil
}
