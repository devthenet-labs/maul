// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/checkreport"
	"github.com/bitwise-media-group/patchy/internal/controller/preview"
)

// preview-controller's node isolation settings (--preview-node-*,
// --preview-taint-key; the chart's preview.nodeIsolation).
const (
	keyPreviewNodeSelector  = "PATCHY_PREVIEW_NODE_SELECTOR"
	keyPreviewNodePool      = "PATCHY_PREVIEW_NODE_POOL"
	keyPreviewNodeClass     = "PATCHY_PREVIEW_NODE_CLASS"
	keyPreviewTaintKey      = "PATCHY_PREVIEW_TAINT_KEY"
	keyPreviewNodeIsolation = "PATCHY_PREVIEW_NODE_ISOLATION"
)

// The node labels preview-controller fills from the NodePool and NodeClass
// settings: EKS Auto Mode's (internal/controller/preview's own, which keeps
// them unexported; a test pins the two).
const (
	nodePoolLabel  = "karpenter.sh/nodepool"
	nodeClassLabel = "eks.amazonaws.com/nodeclass"
)

// nodeNamesShown bounds the node names one reason lists.
const nodeNamesShown = 5

// nodes checks the preview nodes, with node isolation required: at least
// one node carries both the selector every slot Pod renders and the
// <taint key>=true:NoExecute taint every slot Pod tolerates, and no node
// carries only one of them. A node selected but not so tainted lets any Pod
// run beside preview code; a node tainted but not selected is a preview node
// no slot Pod reaches, usually a label typo. With an EKS Auto Mode NodePool
// no node is a SKIP: the NodePool launches them when a slot Pod asks.
func (c *Checker) nodes(ctx context.Context, s Settings) {
	switch {
	case s.Preview.Err != nil:
		c.add(CheckPreviewNodes, checkreport.Skip, "cannot read the controllers' settings: %v", s.Preview.Err)
		return
	case !s.Preview.Found():
		c.add(CheckPreviewNodes, checkreport.Skip, "previews are off")
		return
	case strings.TrimSpace(s.Preview.Data[keyPreviewNodeIsolation]) == preview.NodeIsolationNone:
		c.add(CheckPreviewNodes, checkreport.Warn, "node isolation is off (preview.nodeIsolation.mode none): "+
			"preview Pods run agent-written code on the same nodes as everything else")
		return
	}
	selector, err := nodeSelector(s.Preview)
	taint := strings.TrimSpace(s.Preview.Data[keyPreviewTaintKey])
	switch {
	case err != nil:
		c.add(CheckPreviewNodes, checkreport.Fail, "%s: %v", s.Preview.From, err)
		return
	case len(selector) == 0 || taint == "":
		c.add(CheckPreviewNodes, checkreport.Fail, "%s requires node isolation but sets no node selector or taint "+
			"key, which preview-controller refuses to start with (preview.nodeIsolation)", s.Preview.From)
		return
	}
	var list corev1.NodeList
	if err := c.cfg.Reader.List(ctx, &list); err != nil {
		c.add(CheckPreviewNodes, checkreport.Skip, "cannot list the nodes: %v", err)
		return
	}
	v := judgeNodes(selector, taint, list.Items)
	want := fmt.Sprintf("the node selector %s and the taint %s=true:NoExecute", formatSelector(selector), taint)
	switch {
	case len(v.untainted) > 0 || len(v.unselected) > 0:
		var why []string
		if len(v.untainted) > 0 {
			why = append(why, fmt.Sprintf("%s match%s the node selector without the taint, so any Pod may run "+
				"beside preview code there", names(v.untainted), plural(len(v.untainted), "es", "")))
		}
		if len(v.unselected) > 0 {
			why = append(why, fmt.Sprintf("%s carr%s the taint without the node selector's labels, so no slot Pod "+
				"lands there", names(v.unselected), plural(len(v.unselected), "ies", "y")))
		}
		c.add(CheckPreviewNodes, checkreport.Fail, "every preview node needs %s: %s", want, strings.Join(why, "; "))
	case len(v.isolated) > 0:
		c.add(CheckPreviewNodes, checkreport.Pass, "%s carr%s %s, and no other node carries either", names(v.isolated),
			plural(len(v.isolated), "ies", "y"), want)
	case selector[nodePoolLabel] != "":
		c.add(CheckPreviewNodes, checkreport.Skip, "no node carries %s now, which is expected while no preview "+
			"runs: NodePool %s launches them when a slot Pod asks", want, selector[nodePoolLabel])
	default:
		c.add(CheckPreviewNodes, checkreport.Fail, "no node carries %s, so no slot Pod can schedule: add a "+
			"preview node with both set at registration (the label and taint before it joins, never a taint "+
			"added after other Pods landed)", want)
	}
}

// nodeSelector is the node selector every slot Pod renders, as
// preview-controller builds it: the JSON selector with the NodePool and
// NodeClass filling their EKS keys.
func nodeSelector(cfg ControllerConfig) (map[string]string, error) {
	selector, err := preview.ParseNodeSelector(cfg.Data[keyPreviewNodeSelector])
	if err != nil {
		return nil, err
	}
	out := maps.Clone(selector)
	if out == nil {
		out = map[string]string{}
	}
	if v := strings.TrimSpace(cfg.Data[keyPreviewNodePool]); v != "" {
		out[nodePoolLabel] = v
	}
	if v := strings.TrimSpace(cfg.Data[keyPreviewNodeClass]); v != "" {
		out[nodeClassLabel] = v
	}
	return out, nil
}

// nodeVerdict sorts the nodes: isolated carries the selector and the taint
// as slot Pods tolerate it; untainted the selector alone (or the taint key
// with another value or effect, which slot Pods do not tolerate or other
// Pods survive); unselected the taint key without the selector. Every list
// is sorted by name; a node on none of them is no preview node.
type nodeVerdict struct {
	isolated, untainted, unselected []string
}

// judgeNodes is preview-nodes' sorting, pure.
func judgeNodes(selector map[string]string, taintKey string, nodes []corev1.Node) nodeVerdict {
	var v nodeVerdict
	for i := range nodes {
		n := &nodes[i]
		selected := len(selector) > 0
		for k, want := range selector {
			if got, ok := n.Labels[k]; !ok || got != want {
				selected = false
				break
			}
		}
		hasKey, tainted := false, false
		for _, t := range n.Spec.Taints {
			if t.Key != taintKey {
				continue
			}
			hasKey = true
			if t.Value == "true" && t.Effect == corev1.TaintEffectNoExecute {
				tainted = true
			}
		}
		switch {
		case selected && tainted:
			v.isolated = append(v.isolated, n.Name)
		case selected:
			v.untainted = append(v.untainted, n.Name)
		case hasKey:
			v.unselected = append(v.unselected, n.Name)
		}
	}
	slices.Sort(v.isolated)
	slices.Sort(v.untainted)
	slices.Sort(v.unselected)
	return v
}

// formatSelector renders a selector as k=v pairs, sorted.
func formatSelector(selector map[string]string) string {
	pairs := make([]string, 0, len(selector))
	for _, k := range slices.Sorted(maps.Keys(selector)) {
		pairs = append(pairs, k+"="+selector[k])
	}
	return strings.Join(pairs, ",")
}

// names lists node names, at most nodeNamesShown of them.
func names(nodes []string) string {
	prefix := "node "
	if len(nodes) > 1 {
		prefix = "nodes "
	}
	if len(nodes) > nodeNamesShown {
		return fmt.Sprintf("%s%s and %d more", prefix, strings.Join(nodes[:nodeNamesShown], ", "),
			len(nodes)-nodeNamesShown)
	}
	return prefix + strings.Join(nodes, ", ")
}
