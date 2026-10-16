{{/*
Copyright 2026 Bitwise Media Group Ltd.
SPDX-License-Identifier: MIT

Preview helpers shared by the slot foundation, the admission policies, the
preview-controller's ConfigMap, the preview nodes and NOTES.
*/}}

{{/*
patchy.previewNodeIsolation: preview.nodeIsolation resolved once, as JSON
(read it with fromJson). Fields:
  mode       required | none
  taintKey   the NoExecute taint every slot Pod tolerates ("" with none)
  nodePool   EKS Auto Mode NodePool: eksAutoMode.nodePool, else the legacy
             nodeIsolation.nodePool ("" when neither)
  nodeClass  the same for the NodeClass
  eks        both of nodePool and nodeClass are set
  selector   the generic selector alone (nodeIsolation.selector)
  pairs      the whole selector every slot Pod renders and the admission
             policy compares, as [key, value] pairs: the EKS NodePool and
             NodeClass labels first (in that order), then the generic keys
             sorted, so an install configured by the EKS names alone renders
             the very policy it always did
  legacy     the legacy keys in use, for NOTES
It fails the render on settings preview-controller would refuse to start
with (internal/controller/preview/isolation.go): a mode other than required
or none; required without a selector or a taint key; none with either; half
an EKS pair; a generic selector key that contradicts an EKS name.
Context: the root.
*/}}
{{- define "patchy.previewNodeIsolation" -}}
{{- $n := .Values.preview.nodeIsolation -}}
{{- $eks := $n.eksAutoMode | default dict -}}
{{- $mode := $n.mode | default "required" | toString -}}
{{- $legacy := list -}}
{{- $pool := $eks.nodePool | default "" | toString -}}
{{- $class := $eks.nodeClass | default "" | toString -}}
{{- if $n.nodePool }}{{ $legacy = append $legacy "nodePool" }}{{ if not $pool }}{{ $pool = $n.nodePool | toString }}{{ end }}{{ end -}}
{{- if $n.nodeClass }}{{ $legacy = append $legacy "nodeClass" }}{{ if not $class }}{{ $class = $n.nodeClass | toString }}{{ end }}{{ end -}}
{{- $taint := $n.taintKey | default "" | toString -}}
{{- $selector := $n.selector | default dict -}}
{{- if not (has $mode (list "required" "none")) }}{{ fail (printf "preview.nodeIsolation.mode %q must be required or none" $mode) }}{{ end -}}
{{- if and $pool (not $class) }}{{ fail "preview.nodeIsolation.nodeClass (eksAutoMode.nodeClass) is required with a NodePool: EKS Auto Mode isolation names both the NodePool and its NodeClass" }}{{ end -}}
{{- if and $class (not $pool) }}{{ fail "preview.nodeIsolation.nodePool (eksAutoMode.nodePool) is required with a NodeClass: EKS Auto Mode isolation names both the NodePool and its NodeClass" }}{{ end -}}
{{- range $k, $v := $selector -}}
{{- if not (regexMatch `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$` $k) }}{{ fail (printf "preview.nodeIsolation.selector key %q is not a label key" $k) }}{{ end -}}
{{- if not (regexMatch `^([A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?)?$` (toString $v)) }}{{ fail (printf "preview.nodeIsolation.selector value %q for %s is not a label value" (toString $v) $k) }}{{ end -}}
{{- end -}}
{{- $pairs := list -}}
{{- if $pool -}}
{{- $sel := get $selector "karpenter.sh/nodepool" | default "" | toString -}}
{{- if and $sel (ne $sel $pool) }}{{ fail (printf "preview.nodeIsolation.selector's karpenter.sh/nodepool %q contradicts the NodePool %q" $sel $pool) }}{{ end -}}
{{- $sel = get $selector "eks.amazonaws.com/nodeclass" | default "" | toString -}}
{{- if and $sel (ne $sel $class) }}{{ fail (printf "preview.nodeIsolation.selector's eks.amazonaws.com/nodeclass %q contradicts the NodeClass %q" $sel $class) }}{{ end -}}
{{- $pairs = append $pairs (list "karpenter.sh/nodepool" $pool) -}}
{{- $pairs = append $pairs (list "eks.amazonaws.com/nodeclass" $class) -}}
{{- end -}}
{{- range $k := keys $selector | sortAlpha -}}
{{- if not (and $pool (has $k (list "karpenter.sh/nodepool" "eks.amazonaws.com/nodeclass"))) -}}
{{- $pairs = append $pairs (list $k (get $selector $k | toString)) -}}
{{- end -}}
{{- end -}}
{{- if eq $mode "required" -}}
{{- if not $pairs }}{{ fail "preview.nodeIsolation.nodePool and nodeClass (eksAutoMode), or preview.nodeIsolation.selector, are required when preview.enabled=true with nodeIsolation.mode required: the dedicated preview nodes every slot Pod selects (mode none is the explicit opt-out)" }}{{ end -}}
{{- if not $taint }}{{ fail "preview.nodeIsolation.taintKey is required when preview.enabled=true with nodeIsolation.mode required: the NoExecute taint that keeps every other Pod off the preview nodes" }}{{ end -}}
{{- if not (regexMatch `^([a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$` $taint) }}{{ fail (printf "preview.nodeIsolation.taintKey %q is not a taint key" $taint) }}{{ end -}}
{{- else if or $pairs $taint -}}
{{- fail "preview.nodeIsolation.mode none renders no node selector and no toleration: unset nodeIsolation.selector, eksAutoMode (and the legacy nodePool, nodeClass) and taintKey, or set mode required" -}}
{{- end -}}
{{- dict "mode" $mode "taintKey" $taint "nodePool" $pool "nodeClass" $class "eks" (and (ne $pool "") (ne $class "")) "selector" $selector "pairs" $pairs "legacy" $legacy | toJson -}}
{{- end }}
