{{/*
Copyright 2026 Bitwise Media Group Ltd.
SPDX-License-Identifier: MIT
*/}}

{{- define "maul-config.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "maul-config.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Full label set. Context: dict "root" $ "name" <consuming controller>.
app.kubernetes.io/name matches the maul chart's component labels
(integration-controller, source-controller) so each CR groups with the
controller that reconciles it.
*/}}
{{- define "maul-config.labels" -}}
app: {{ .name }}
helm.sh/chart: {{ printf "%s-%s" .root.Chart.Name .root.Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/part-of: maul
{{- with .root.Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Object annotations: commonAnnotations plus optional per-object extras, which
win key-by-key. Context: dict "root" $ ["extra" <map>]. Empty output when
both maps are empty — wrap the annotations: key in `with`.
*/}}
{{- define "maul-config.annotations" -}}
{{- $a := merge (dict) (.extra | default dict) (.root.Values.commonAnnotations | default dict) -}}
{{- if $a -}}
{{- toYaml $a -}}
{{- end -}}
{{- end }}
