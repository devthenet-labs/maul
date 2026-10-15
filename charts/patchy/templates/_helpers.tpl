{{/*
Copyright 2026 Bitwise Media Group Ltd.
SPDX-License-Identifier: MIT
*/}}

{{- define "patchy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end }}

{{- define "patchy.fullname" -}}
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
Per-controller ConfigMap name. Context: dict "root" $ "name" <component>.
*/}}
{{- define "patchy.configMapName" -}}
{{- printf "%s-%s-config" (include "patchy.fullname" .root) .name -}}
{{- end }}

{{/*
Per-controller ServiceAccount name: <controller>.serviceAccount.name, or
<fullname>-<component>. Context: dict "root" $ "name" <component> "vals"
<controller values>.
*/}}
{{- define "patchy.serviceAccountName" -}}
{{- .vals.serviceAccount.name | default (printf "%s-%s" (include "patchy.fullname" .root) .name) -}}
{{- end }}

{{/*
Selector labels for one component. Context: dict "root" $ "name" <component>.
app.kubernetes.io/name matches deploy/kustomize (source-controller, ...);
instance disambiguates releases.
*/}}
{{- define "patchy.selectorLabels" -}}
app.kubernetes.io/name: {{ .name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/part-of: patchy
{{- end }}

{{/*
Full label set. Context: dict "root" $ "name" <component> ["component" <role>].
*/}}
{{- define "patchy.labels" -}}
{{- /*
The bare `app` duplicates app.kubernetes.io/name for tooling that predates
the recommended-label set (kubescape C-0076 counts it, some dashboards group
by it). Not in selectorLabels: selectors are immutable on upgrade.
*/ -}}
app: {{ .name }}
helm.sh/chart: {{ printf "%s-%s" .root.Chart.Name .root.Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
{{ include "patchy.selectorLabels" . }}
{{- with .component }}
app.kubernetes.io/component: {{ . }}
{{- end }}
{{- with .root.Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Object annotations: commonAnnotations plus optional per-object extras, which
win key-by-key. Context: dict "root" $ ["extra" <map>]. Empty output when
both maps are empty — wrap the annotations: key in `with`.
*/}}
{{- define "patchy.annotations" -}}
{{- $a := merge (dict) (.extra | default dict) (.root.Values.commonAnnotations | default dict) -}}
{{- if $a -}}
{{- toYaml $a -}}
{{- end -}}
{{- end }}

{{/*
Image reference for one binary. Context: dict "root" $ "binary" <name>
["image" <per-component override map>]. The repository (registry included)
defaults to <image.repository>/<binary>; a digest pins (and beats the tag);
the tag defaults to v<appVersion>, which is how goreleaser tags the images.
*/}}
{{- define "patchy.image" -}}
{{- $img := .image | default dict -}}
{{- $g := .root.Values.image -}}
{{- $repository := $img.repository | default (printf "%s/%s" $g.repository .binary) -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $repository $img.digest -}}
{{- else -}}
{{- printf "%s:%s" $repository ($img.tag | default $g.tag | default (printf "v%s" .root.Chart.AppVersion)) -}}
{{- end -}}
{{- end }}

{{/*
The pod labels internal/jobs stamps on every agent Job pod. Fixed by the
controller, not by this chart — the sandbox NetworkPolicies select on them.
*/}}
{{- define "patchy.agentPodSelector" -}}
app.kubernetes.io/name: patchy-agent
app.kubernetes.io/managed-by: patchy
{{- end }}

{{/*
The edge IngressClass's name while edgeIngressClass.create is on, else empty:
the className the webhook and status-page Ingresses fall back to when their
own is empty, so with create off they render exactly as before.
*/}}
{{- define "patchy.edgeIngressClassName" -}}
{{- $e := .Values.edgeIngressClass | default dict -}}
{{- if $e.create -}}
{{- $e.name | default (printf "%s-edge" (include "patchy.fullname" .)) -}}
{{- end -}}
{{- end }}

{{/*
The node-local DNS egress rule (clusterDNSCIDR): UDP and TCP 53 to that one
address. Every NetworkPolicy that allows DNS to kube-system includes it right
after that rule, so it widens exactly the pods the chart already isolates and
selects no new ones (a separate podSelector-{} policy would egress-isolate any
pod no other policy selects, such as a controller whose
networkPolicy.create is false). EKS Auto Mode serves DNS from each node at
the cluster DNS Service address, not from kube-system pods, so once
NetworkPolicy is enforced the kube-system rule matches nothing there. Empty
while clusterDNSCIDR is unset, so the policies render as before. Context: the
root; include it with nindent 8 inside `with`.
*/}}
{{- define "patchy.clusterDNSEgress" -}}
{{- with .Values.clusterDNSCIDR -}}
# DNS at the node-local cluster resolver (clusterDNSCIDR).
- to:
      - ipBlock:
            cidr: {{ . }}
  ports:
      - protocol: UDP
        port: 53
      - protocol: TCP
        port: 53
{{- end -}}
{{- end }}

{{/*
Whether the egress credential broker deploys: exactly when a claude runner is
enabled anywhere (findings, evaluations or intents). There is deliberately no
separate enabled knob — claude runs proxy-only, so claude ⇒ broker, and a
fake-only dev/e2e values file renders none of it. intent-controller runs
brokered claude and nothing else, so enabling it enables claude whatever
agent.runners.claude says. Returns a non-empty string for yes.
*/}}
{{- define "patchy.brokerEnabled" -}}
{{- if or .Values.agent.runners.claude.enabled (and .Values.evaluationController.enabled .Values.evaluationController.runners.claude.enabled) .Values.intentController.enabled -}}
yes
{{- end -}}
{{- end }}

{{/*
Whether a harness runs anywhere (findings, evaluations or intents), by the
same rule as patchy.brokerEnabled. Evaluation and intent Jobs carry the same
harness label as finding Jobs, so the per-harness egress policies (Cilium,
GKE FQDN, Istio) render for a harness enabled on any fleet; hosts and
dnsPatterns still come from agent.runners.<id>. Intents run on claude only.
Takes (dict "root" $ "id" $id); returns a non-empty string for yes.
*/}}
{{- define "patchy.harnessEnabled" -}}
{{- $agent := index .root.Values.agent.runners .id | default dict -}}
{{- $eval := index .root.Values.evaluationController.runners .id | default dict -}}
{{- $intent := and .root.Values.intentController.enabled (eq .id "claude") -}}
{{- if or $agent.enabled (and .root.Values.evaluationController.enabled $eval.enabled) $intent -}}
yes
{{- end -}}
{{- end }}

{{/*
The egress broker's in-cluster base URL — what PATCHY_BROKER_URL is stamped
with and what the agent pods' gateway env points at.
*/}}
{{- define "patchy.brokerURL" -}}
http://{{ include "patchy.fullname" . }}-egress-broker.{{ .Release.Namespace }}.svc.cluster.local:8080
{{- end }}

{{/*
Stamp the brokered-claude configuration into a controller's ConfigMap data
dict: the broker URL and the PATCHY_CLAUDE_PROVIDER* keys the runnercfg flags
bind. Context: dict "root" $ "data" <dict>. The PATCHY_CLAUDE_SECRET* keys
are deliberately NOT stamped anymore — the claude model credential lives in
the broker, not the agent namespace.
*/}}
{{- define "patchy.claudeProviderData" -}}
{{- $p := .root.Values.agent.runners.claude.provider | default dict -}}
{{- $_ := set .data "PATCHY_BROKER_URL" (include "patchy.brokerURL" .root) -}}
{{- $_ := set .data "PATCHY_CLAUDE_PROVIDER" ($p.name | default "anthropic") -}}
{{- with $p.region -}}
{{- $_ := set $.data "PATCHY_CLAUDE_PROVIDER_REGION" . -}}
{{- end -}}
{{- with $p.regionPrefix -}}
{{- $_ := set $.data "PATCHY_CLAUDE_PROVIDER_REGION_PREFIX" . -}}
{{- end -}}
{{- with $p.projectID -}}
{{- $_ := set $.data "PATCHY_CLAUDE_PROVIDER_PROJECT_ID" . -}}
{{- end -}}
{{- with $p.modelMap -}}
{{- $_ := set $.data "PATCHY_CLAUDE_MODEL_MAP" (include "patchy.kvList" .) -}}
{{- end -}}
{{- with $p.env -}}
{{- $_ := set $.data "PATCHY_CLAUDE_PROVIDER_ENV" (include "patchy.kvList" .) -}}
{{- end -}}
{{- end }}

{{/*
The agent Jobs' default CPU and memory (agent.resources.default) as the four
PATCHY_AGENT_* keys every controller that launches agent Jobs binds
(investigation, remediation, intent and evaluation): set into .data, each only
when its quantity is set, so the default {} leaves every ConfigMap, and with it
every checksum/config, exactly as it was. Context: dict "root" $ "data" <dict>.
*/}}
{{- define "patchy.agentResourcesData" -}}
{{- $d := (.root.Values.agent.resources | default dict).default | default dict -}}
{{- range $side, $keys := dict "requests" (dict "cpu" "PATCHY_AGENT_CPU_REQUEST" "memory" "PATCHY_AGENT_MEMORY_REQUEST") "limits" (dict "cpu" "PATCHY_AGENT_CPU_LIMIT" "memory" "PATCHY_AGENT_MEMORY_LIMIT") -}}
{{- $q := index $d $side | default dict -}}
{{- range $name, $key := $keys -}}
{{- if hasKey $q $name -}}
{{- $_ := set $.data $key (index $q $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Whether the agent pods run without a resolver (agent.networkPolicy.dns: none),
with the render-time guards that setting needs; returns a non-empty string for
yes. Under none every runner but brokered claude is refused, on either fleet
(patchy.harnessEnabled): codex and copilot dial their vendor's API by name, and
an internet name cannot be pinned at launch the way a ClusterIP Service can.
So is mode istio: the agent pod's sidecar must resolve istiod before the Job
can run at all.
*/}}
{{- define "patchy.agentDNSNone" -}}
{{- if eq (.Values.agent.networkPolicy.dns | default "cluster") "none" -}}
{{- range $id, $r := .Values.agent.runners -}}
{{- if and (ne $id "claude") (include "patchy.harnessEnabled" (dict "root" $ "id" $id)) -}}
{{- fail (printf "agent.networkPolicy.dns none: the %s runner dials its model API by name and needs a resolver; disable it (agent.runners.%s.enabled, evaluationController.runners.%s.enabled) or keep dns: cluster" $id $id $id) -}}
{{- end -}}
{{- end -}}
{{- if eq (include "patchy.egressMode" .) "istio" -}}
{{- fail "agent.networkPolicy.dns none is incompatible with mode istio: the agent pod's sidecar must resolve istiod" -}}
{{- end -}}
yes
{{- end -}}
{{- end }}

{{/*
PATCHY_AGENT_DNS for every controller that launches agent Jobs (investigation,
remediation, intent and evaluation): set into .data only under dns: none, so
the default leaves every ConfigMap, and every checksum/config, as it was.
Context: dict "root" $ "data" <dict>.
*/}}
{{- define "patchy.agentDNSData" -}}
{{- if include "patchy.agentDNSNone" .root -}}
{{- $_ := set .data "PATCHY_AGENT_DNS" "none" -}}
{{- end -}}
{{- end }}

{{/*
PATCHY_AGENTS_PAUSED for every controller that launches agent Jobs
(investigation, remediation, intent and evaluation): agent.paused, the
cluster-wide pause. Set into .data only while paused, so unpausing leaves
every ConfigMap as it was; either way the change rolls those four
controllers, whose schedulers then grant no slot and launch no Job. Jobs
already running finish and are collected. Context: dict "root" $ "data"
<dict>.
*/}}
{{- define "patchy.agentsPausedData" -}}
{{- if .root.Values.agent.paused -}}
{{- $_ := set .data "PATCHY_AGENTS_PAUSED" "true" -}}
{{- end -}}
{{- end }}

{{/*
One container's operator env (<component>.extraEnv: EnvVar entries, valueFrom
allowed), rendered as list items for the caller to indent. The names patchy
itself sets are refused, so a Secret-backed variable can add credentials or
settings but never silently replace what the chart configured: every PATCHY_*
key (the chart's own config, overridable only through config.extra, where its
render-time guards still see it), POD_NAMESPACE and DOCKER_CONFIG (the
agent.repositoryImages.pullSecret mount). <component>.extraEnvFrom cannot be
judged at render time, so the Deployments list it BEFORE the chart's own
ConfigMap: a later envFrom source wins a shared key, and explicit env wins
over every envFrom source. Context: dict "path" "<values key>" "env" <list>.
*/}}
{{- define "patchy.extraEnv" -}}
{{- $path := .path -}}
{{- range .env -}}
{{- $name := toString .name -}}
{{- if or (hasPrefix "PATCHY_" $name) (has $name (list "POD_NAMESPACE" "DOCKER_CONFIG")) -}}
{{- fail (printf "%s.extraEnv: %s is set by the chart and cannot be overridden here (PATCHY_* keys go in %s.config.extra; DOCKER_CONFIG follows agent.repositoryImages.pullSecret)" $path $name $path) -}}
{{- end -}}
{{- end -}}
{{- with .env -}}
{{- toYaml . -}}
{{- end -}}
{{- end }}

{{/*
Render a map as the comma-joined key=value wire form the runnercfg flags
parse (--claude-model-map, --claude-provider-env). Keys sort deterministically.
*/}}
{{- define "patchy.kvList" -}}
{{- $pairs := list -}}
{{- range $k, $v := . -}}
{{- $pairs = append $pairs (printf "%s=%s" $k $v) -}}
{{- end -}}
{{- join "," $pairs -}}
{{- end }}

{{/*
The resolved hostname-enforcement mode for the agent sandbox — one of
`none`, `cilium`, `gke` or `istio`. Every egress template keys off this, so
there is exactly one place that decides which CNI dialect a cluster gets.

Precedence:

  1. agent.networkPolicy.mode, when it is not "auto".
  2. The legacy booleans (agent.networkPolicy.{cilium,istio}.enabled), so a
     values file written before `mode` existed keeps its behaviour.
  3. "auto" — capability detection against the cluster's discovery document.

Helm fills .Capabilities.APIVersions from the API server whenever it renders
against a cluster: `helm install/upgrade`, and every helm-controller
reconcile. Off-cluster `helm template` sees only Helm's built-in list, so auto
resolves to "none" in CI — deliberately, since a rendering that cannot see the
cluster must not claim an enforcement it cannot verify. Pin `mode` when you
need a deterministic render.

Two things auto will never do:

  * Select `istio`. The CRDs being installed says nothing about whether the
    namespace is injection-labelled or whether istiod runs with native
    sidecars — and without native sidecars the agent Job never completes.
    Istio stays opt-in.
  * Select `cilium` on GKE. GKE Dataplane V2 IS Cilium, and it does publish
    cilium.io CRDs, but it has not honoured the CiliumNetworkPolicy CRD since
    1.21.5-gke.1300 and rejects every L7 rule — the `toFQDNs` and `rules.dns`
    blocks this chart's CNP is made of. Rendering one there yields a policy
    that silently enforces nothing, which is strictly worse than rendering
    none: it reads as protection in `kubectl get`. GKE's own
    FQDNNetworkPolicy is the equivalent, so on GKE auto picks `gke` when that
    CRD is present and falls back to `none` when it is not.
*/}}
{{- define "patchy.egressMode" -}}
{{- $np := .Values.agent.networkPolicy -}}
{{- $mode := $np.mode | default "auto" -}}
{{- if not (has $mode (list "auto" "none" "cilium" "gke" "istio")) -}}
{{- fail (printf "agent.networkPolicy.mode: %q is not one of auto, none, cilium, gke, istio" $mode) -}}
{{- end -}}
{{- if ne $mode "auto" -}}
{{- $mode -}}
{{- else if $np.cilium.enabled -}}
cilium
{{- else if $np.istio.enabled -}}
istio
{{- else if .Capabilities.APIVersions.Has "networking.gke.io/v1alpha1/FQDNNetworkPolicy" -}}
gke
{{- else if and (not (contains "gke" (toString .Capabilities.KubeVersion.GitVersion))) (.Capabilities.APIVersions.Has "cilium.io/v2/CiliumNetworkPolicy") -}}
cilium
{{- else -}}
none
{{- end -}}
{{- end }}

{{/*
Whether the base NetworkPolicy keeps its "TCP 443 to anywhere outside the
cluster" egress rule. Returns a non-empty string for yes.

This is the rule that makes a hostname allowlist meaningful or meaningless.
Network policies are ADDITIVE — both Cilium and GKE Dataplane V2 allow a
packet that matches ANY policy selecting the pod, and neither has a way for
one policy to subtract from another. So a CiliumNetworkPolicy or an
FQDNNetworkPolicy naming api.anthropic.com, rendered alongside a plain
NetworkPolicy that already allows 443 to 0.0.0.0/0, changes nothing at all:
the union is still "443 to anywhere".

`auto` therefore drops the broad rule exactly when a hostname mode is doing
the work (cilium, gke) and keeps it when nothing else would replace it (none,
istio — the Istio sidecar enforces on SNI in a different plane, and the L3
floor is still wanted underneath it). `always` keeps it regardless, which is
the honest setting while soaking a new mode; `never` drops it regardless.
*/}}
{{- define "patchy.broadEgress" -}}
{{- $broad := .Values.agent.networkPolicy.broadEgress | default "auto" -}}
{{- if not (has $broad (list "auto" "always" "never")) -}}
{{- fail (printf "agent.networkPolicy.broadEgress: %q is not one of auto, always, never" $broad) -}}
{{- end -}}
{{- if eq $broad "always" -}}
yes
{{- else if eq $broad "never" -}}
{{- else if has (include "patchy.egressMode" .) (list "none" "istio") -}}
yes
{{- end -}}
{{- end }}

{{/*
Render-time guards for agent.repositoryImages; called once from
configmap.yaml, which always renders. Only an enabled block is judged, so
flipping the kill switch off never fails a render. Every guard fails the
render rather than leaving a controller to crash-loop, or a sandbox to run
untrusted images with broad egress:

  * registries empty — source-controller refuses to start without an
    allowlist, and an empty one would admit nothing anyway.
  * ephemeralStorage empty — both job controllers refuse to start without
    the wall on disk a repository image can fill.
  * cosignPublicKey empty without allowUnsigned — source-controller refuses
    to start without a key unless unsigned images are explicitly allowed.
  * cosignPublicKey without its PEM armour — source-controller parses a set
    key at startup, allowUnsigned or not. A template cannot parse the DER
    inside, so a well-armoured but corrupt key still stops it; the armour
    catches the pasted-the-wrong-thing case.
  * pullSecretData without pullSecret — the rendered Secret needs the name
    both namespaces share.
  * broad agent egress (the design's decision 2): no agent NetworkPolicy at
    all, or a base policy that keeps "TCP 443 to anywhere", would let a
    hostile image reach a model API with a key of its own. A NOTES warning
    is invisible in CI; a failed render is not.

The formats of a registries entry and of ephemeralStorage are
values.schema.json patterns instead, so helm lint judges them too. Each
pattern admits only values the binary accepts at startup, pinned by
TestChartRegistryPatternIsSound (cmd/source-controller) and
TestChartEphemeralStoragePatternIsSound (internal/runnercfg).
*/}}
{{- define "patchy.repositoryImagesGuard" -}}
{{- $ri := .Values.agent.repositoryImages | default dict -}}
{{- if $ri.enabled -}}
{{- if not $ri.registries -}}
{{- fail "agent.repositoryImages.enabled requires agent.repositoryImages.registries: list the registry path prefixes a declared image must sit under, e.g. 123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/ or ghcr.io/my-org/agent-images/" -}}
{{- end -}}
{{- if not $ri.ephemeralStorage -}}
{{- fail "agent.repositoryImages.enabled requires agent.repositoryImages.ephemeralStorage, a quantity such as 8Gi: the ephemeral-storage request and limit that bounds the disk a declared image can fill" -}}
{{- end -}}
{{- if and (not $ri.cosignPublicKey) (not $ri.allowUnsigned) -}}
{{- fail "agent.repositoryImages.enabled requires agent.repositoryImages.cosignPublicKey, the PEM public key declared images must be cosign-signed with; set agent.repositoryImages.allowUnsigned: true to admit unsigned images instead" -}}
{{- end -}}
{{- if and $ri.cosignPublicKey (not (and (contains "-----BEGIN PUBLIC KEY-----" $ri.cosignPublicKey) (contains "-----END PUBLIC KEY-----" $ri.cosignPublicKey))) -}}
{{- fail "agent.repositoryImages.cosignPublicKey is not a PEM public key: set it to the whole cosign.pub that cosign generate-key-pair writes, -----BEGIN PUBLIC KEY----- through -----END PUBLIC KEY----- (source-controller refuses anything else at startup)" -}}
{{- end -}}
{{- if not .Values.agent.networkPolicy.create -}}
{{- fail "agent.repositoryImages.enabled requires the agent sandbox NetworkPolicy, but agent.networkPolicy.create is false: an agent pod would have unrestricted egress. Set agent.networkPolicy.create: true" -}}
{{- end -}}
{{- if include "patchy.broadEgress" . -}}
{{- fail (printf "agent.repositoryImages.enabled requires narrow agent egress, but agent.networkPolicy.broadEgress (%q) resolves to broad under agent.networkPolicy.mode %q: the base policy would allow TCP 443 to anywhere. Set agent.networkPolicy.broadEgress: never (brokered claude runners only), or use agent.networkPolicy.mode cilium or gke." (.Values.agent.networkPolicy.broadEgress | default "auto") (include "patchy.egressMode" .)) -}}
{{- end -}}
{{- if and $ri.pullSecretData (not $ri.pullSecret) -}}
{{- fail "agent.repositoryImages.pullSecretData requires agent.repositoryImages.pullSecret, the name of the Secret it renders into both the release namespace (source-controller's mount) and agent.namespace (the kubelet's pull)" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The preview image prefix, <preview.imageRegistry>/<preview.imagePathPrefix>/:
the one string the slot admission policy (preview-admission.yaml) and the
preview-controller (PATCHY_PREVIEW_IMAGE_PREFIX) hold every preview image to,
and the path source-controller refuses agent images from, so none of them can
disagree. With the default path, patchy/previews, it is the prefix the chart
hard-coded before the path was configurable, so those renders are byte for
byte what they were. Context: the root. It fails the render when:

  * imagePathPrefix is empty, or not lowercase ECR path segments joined by
    "/" with no leading or trailing slash. values.schema.json says the same,
    but a render that skips schema validation still lands here.
  * agent.repositoryImages is enabled and one of its registries entries is
    this prefix, contains it or sits under it. A preview runtime image is
    built from an unreviewed same-repository pull request head, so it must
    never be admissible as an agent sandbox image, and an agent toolchain
    image must never be admissible as a preview. The comparison is on
    segment boundaries over patchy.registryPathKey, so no other spelling of
    the same ECR repository passes.
*/}}
{{- define "patchy.previewImagePrefix" -}}
{{- $p := .Values.preview -}}
{{- $path := $p.imagePathPrefix | default "" | toString -}}
{{- if not (regexMatch `^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$` $path) -}}
{{- fail (printf "preview.imagePathPrefix %q must be one or more lowercase registry path segments joined by '/', with no leading or trailing slash, such as patchy/previews (the default)" $path) -}}
{{- end -}}
{{- $prefix := printf "%s/%s/" $p.imageRegistry $path -}}
{{- $ri := .Values.agent.repositoryImages | default dict -}}
{{- if $ri.enabled -}}
{{- $previewKey := include "patchy.registryPathKey" $prefix -}}
{{- range $entry := $ri.registries | default list -}}
{{- $agentKey := include "patchy.registryPathKey" (printf "%s/" (trimSuffix "/" (toString $entry))) -}}
{{- if or (hasPrefix $agentKey $previewKey) (hasPrefix $previewKey $agentKey) -}}
{{- fail (printf "agent.repositoryImages.registries entry %q overlaps the preview image prefix %s (preview.imageRegistry/preview.imagePathPrefix): the two must be disjoint, neither equal to nor under the other, or an image built from an unreviewed pull request could run as an agent sandbox (and an agent toolchain image as a preview). Move the agent images, or set preview.imagePathPrefix to a path outside every registries entry" (toString $entry) $prefix) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $prefix -}}
{{- end }}

{{/*
patchy.registryPathKey: a "host/path/" registry prefix as the registry it is
pulled from sees it, for comparing two prefixes on segment boundaries.
Lowercased (source-controller lowercases allowlist entries and references), an
explicit :443 dropped (the same HTTPS endpoint), and ECR's dual-stack and
FIPS endpoint names (<account>.dkr-ecr.<region>.on.aws,
<account>.dkr.ecr-fips.<region>.amazonaws.com, ...) folded onto the plain
<account>.dkr.ecr.<region>.amazonaws.com, which serves the same repositories.
runnerimage.registryKey is the same fold on source-controller's side.
*/}}
{{- define "patchy.registryPathKey" -}}
{{- $key := lower . -}}
{{- $key = regexReplaceAll `^([^/]+):443/` $key "${1}/" -}}
{{- regexReplaceAll `^([0-9]{12})\.dkr[.-]ecr(-fips)?\.([a-z0-9-]+)\.(amazonaws\.com|on\.aws)/` $key "${1}.dkr.ecr.${3}.amazonaws.com/" -}}
{{- end }}

{{/*
Whether the egress proxy deploys (egressProxy.enabled); returns a non-empty
string for yes. Everything the feature renders, and every key it stamps,
hangs off this, so with it off every existing render is unchanged.
*/}}
{{- define "patchy.egressProxyEnabled" -}}
{{- if (.Values.egressProxy | default dict).enabled -}}
yes
{{- end -}}
{{- end }}

{{/*
The label prefix an agent Job and its pod carry, as <prefix><profile>: "true",
when a network profile is applied: the identity both NetworkPolicies on the
profile's port select, and what the profile-label admission policy guards.
The one spelling in the chart; TestChartLabelPrefix pins it to
netprofile.LabelPrefix.
*/}}
{{- define "patchy.networkProfileLabelPrefix" -}}
patchy.bitwisemedia.uk/network-
{{- end }}

{{/*
The egress proxy's in-cluster base URL, without a port: the job controllers
join each profile's port to it (PATCHY_EGRESS_PROXY_URL).
*/}}
{{- define "patchy.egressProxyURL" -}}
http://{{ include "patchy.fullname" . }}-egress-proxy.{{ .Release.Namespace }}.svc.cluster.local
{{- end }}

{{/*
agent.networkProfiles as the JSON array the egress proxy and both job
controllers parse (netprofile.Parse, PATCHY_NETWORK_PROFILES): sorted by
name, defaults applied (host github.com, bakedModCache /opt/go/pkg/mod,
allowPseudoVersions false), trailing slashes stripped from the module
prefixes (the matcher works on path-element boundaries anyway). One string,
so the three ConfigMaps that carry it cannot disagree.

A profile with registry grants also carries "registries": one object per
grant, sorted by registry name, holding the grant's policy plus what the
job controllers need of the registry (patchy.egressGrantJSON): they never
see egressProxy.registries. A profile without grants renders no such key.
*/}}
{{- define "patchy.networkProfilesJSON" -}}
{{- $profiles := .Values.agent.networkProfiles | default dict -}}
{{- $out := list -}}
{{- range $name := keys $profiles | sortAlpha -}}
{{- $p := index $profiles $name | default dict -}}
{{- $modules := list -}}
{{- range $p.modules | default list -}}
{{- $modules = append $modules (trimSuffix "/" (toString .)) -}}
{{- end -}}
{{- $baked := "/opt/go/pkg/mod" -}}
{{- if hasKey $p "bakedModCache" -}}
{{- $baked = $p.bakedModCache | default "" | toString -}}
{{- end -}}
{{- $out = append $out (dict
    "name" $name
    "route" ($p.route | default "")
    "port" (int ($p.port | default 0))
    "host" ($p.host | default "github.com")
    "repositories" ($p.repositories | default list)
    "modules" $modules
    "bakedModCache" $baked
    "allowPseudoVersions" ($p.allowPseudoVersions | default false)
) -}}
{{- with $p.registries -}}
{{- $grants := list -}}
{{- range $gname := keys . | sortAlpha -}}
{{- $grants = append $grants (include "patchy.egressGrantJSON" (dict "root" $ "name" $gname "grant" (index $p.registries $gname | default dict)) | fromJson) -}}
{{- end -}}
{{- $_ := set (last $out) "registries" $grants -}}
{{- end -}}
{{- end -}}
{{- toJson $out -}}
{{- end }}

{{/*
One registry grant as the profile JSON carries it (netprofile.Grant): the
grant's own policy and switches, each key only when set, plus the granted
registry's ecosystem, privateModules (go) and hosts (netprofile.Registry.Hosts:
the URL's host, then fileHosts, then redirectHosts), which the egress proxy
holds equal to the registry's at startup (netprofile.CheckGrants) and the
job controllers check against every Forge CR at launch. Go module prefixes
lose a trailing slash, as a profile's modules do. Context: dict "root" $
"name" <registry> "grant" <grant values>.
*/}}
{{- define "patchy.egressGrantJSON" -}}
{{- $g := .grant -}}
{{- $r := index ((.root.Values.egressProxy | default dict).registries | default dict) .name | default dict -}}
{{- $eco := $r.ecosystem | default "" -}}
{{- $out := dict "name" .name "ecosystem" $eco "hosts" (include "patchy.egressRegistryHosts" $r | fromJsonArray) -}}
{{- with $g.packages -}}
{{- $pkgs := list -}}
{{- range . -}}
{{- $pkgs = append $pkgs (ternary (trimSuffix "/" (toString .)) (toString .) (eq $eco "go")) -}}
{{- end -}}
{{- $_ := set $out "packages" $pkgs -}}
{{- end -}}
{{- with $g.packagePolicy }}{{ $_ := set $out "packagePolicy" . }}{{ end -}}
{{- if $g.allowPseudoVersions }}{{ $_ := set $out "allowPseudoVersions" true }}{{ end -}}
{{- if $g.allowPrerelease }}{{ $_ := set $out "allowPrerelease" true }}{{ end -}}
{{- if $g.scripts }}{{ $_ := set $out "scripts" true }}{{ end -}}
{{- if $g.sdists }}{{ $_ := set $out "sdists" true }}{{ end -}}
{{- with include "patchy.egressPrivateModules" $r | fromJsonArray -}}
{{- $_ := set $out "privateModules" . -}}
{{- end -}}
{{- toJson $out -}}
{{- end }}

{{/*
A registry's hosts as a JSON array: the URL's host (no port), then
fileHosts, then redirectHosts, as written. netprofile.Registry.Hosts is the
same list; the egress proxy refuses a grant whose copy differs.
*/}}
{{- define "patchy.egressRegistryHosts" -}}
{{- $hosts := list ((urlParse (.url | default "" | toString)).hostname) -}}
{{- range .fileHosts | default list }}{{ $hosts = append $hosts (toString .) }}{{ end -}}
{{- range .redirectHosts | default list }}{{ $hosts = append $hosts (toString .) }}{{ end -}}
{{- toJson $hosts -}}
{{- end }}

{{/*
A go registry's privateModules as a JSON array, trailing slashes stripped
(GONOSUMDB and the proxy's matcher work on path-element boundaries either
way): "[]" for a go registry that lists none, "null" for a registry that is
not go or does not say.
*/}}
{{- define "patchy.egressPrivateModules" -}}
{{- if and (eq (.ecosystem | default "") "go") (not (kindIs "invalid" .privateModules)) -}}
{{- $out := list -}}
{{- range .privateModules | default list }}{{ $out = append $out (trimSuffix "/" (toString .)) }}{{ end -}}
{{- toJson $out -}}
{{- else -}}
null
{{- end -}}
{{- end }}

{{/*
egressProxy.registries as the JSON array the egress proxy parses
(netprofile.ParseRegistries, PATCHY_EGRESS_REGISTRIES): sorted by name, each
key only when set. A credential is described by its type alone (plus the
CodeArtifact domain to mint for): a Secret's name never appears here, and
the proxy reads a bearer or basic credential from the mounted
registry-credentials volume. "ca": true says a CA bundle is mounted at
<registry>.pem in the registry-ca volume.
*/}}
{{- define "patchy.egressRegistriesJSON" -}}
{{- $regs := (.Values.egressProxy | default dict).registries | default dict -}}
{{- $out := list -}}
{{- range $name := keys $regs | sortAlpha -}}
{{- $r := index $regs $name | default dict -}}
{{- $auth := $r.auth | default dict -}}
{{- $a := dict "type" ($auth.type | default "") -}}
{{- with $auth.codeartifact -}}
{{- $ca := dict "domain" (.domain | default "" | toString) "domainOwner" (.domainOwner | default "" | toString) "region" (.region | default "" | toString) -}}
{{- with .durationSeconds }}{{ $_ := set $ca "durationSeconds" (int .) }}{{ end -}}
{{- $_ := set $a "codeartifact" $ca -}}
{{- end -}}
{{- $e := dict "name" $name "ecosystem" ($r.ecosystem | default "") "url" ($r.url | default "") "auth" $a -}}
{{- with $r.fileHosts }}{{ $_ := set $e "fileHosts" . }}{{ end -}}
{{- with $r.redirectHosts }}{{ $_ := set $e "redirectHosts" . }}{{ end -}}
{{- with $r.internal -}}
{{- $in := dict "cidrs" (.cidrs | default list) -}}
{{- with .podSelector }}{{ $_ := set $in "podSelector" . }}{{ end -}}
{{- with .targetPort }}{{ $_ := set $in "targetPort" (int .) }}{{ end -}}
{{- $_ := set $e "internal" $in -}}
{{- end -}}
{{- $pm := include "patchy.egressPrivateModules" $r | fromJsonArray -}}
{{- if and (eq ($r.ecosystem | default "") "go") (not (kindIs "invalid" $r.privateModules)) -}}
{{- $_ := set $e "privateModules" $pm -}}
{{- end -}}
{{- if kindIs "bool" $r.publicChecksumLookups }}{{ $_ := set $e "publicChecksumLookups" $r.publicChecksumLookups }}{{ end -}}
{{- with $r.maxArtifactBytes }}{{ $_ := set $e "maxArtifactBytes" (int64 .) }}{{ end -}}
{{- with $r.healthPackage }}{{ $_ := set $e "healthPackage" (toString .) }}{{ end -}}
{{- if $r.ca }}{{ $_ := set $e "ca" true }}{{ end -}}
{{- $out = append $out $e -}}
{{- end -}}
{{- toJson $out -}}
{{- end }}

{{/*
Stamp the network-profile keys into remediation- or intent-controller's
ConfigMap data: the kill switch (off under suspendProfiles), the profile set
and the proxy's base URL. Called only while the egress proxy is enabled.
Context: dict "root" $ "data" <dict>.
*/}}
{{- define "patchy.networkProfilesData" -}}
{{- $ep := .root.Values.egressProxy -}}
{{- $_ := set .data "PATCHY_NETWORK_PROFILES_ENABLED" (ternary "false" "true" ($ep.suspendProfiles | default false)) -}}
{{- $_ := set .data "PATCHY_NETWORK_PROFILES" (include "patchy.networkProfilesJSON" .root) -}}
{{- $_ := set .data "PATCHY_EGRESS_PROXY_URL" (include "patchy.egressProxyURL" .root) -}}
{{- end }}

{{/*
Render-time guards for egressProxy; called once from configmap.yaml beside
patchy.repositoryImagesGuard. Only an enabled block is judged, so turning it
off never fails a render. Each fails the render with a message naming the
value:

  1. egressProxy.enabled without agent.repositoryImages.enabled: a profile
     only applies to a Job running an accepted repository image (and that
     block's own guards bring narrow egress and the agent NetworkPolicy).
  2. no agent.networkProfiles entry: the proxy and both controllers refuse
     to start on an empty set.
  3. egressProxy.networkPolicy.create false: the per-profile ingress rules
     are the identity check.
  4. agent.networkPolicy.mode resolving to istio: not verified.
  5. two profiles on one port (the schema cannot say "unique").
  6. a module prefix that admits the Go toolchain modules: golang.org,
     golang.org/ or golang.org/toolchain...
  7. a bakedModCache with a . or .. element, an empty element or a trailing
     slash. values.schema.json refuses these too; this holds a render that
     skips schema validation to the validator's rule (path.Clean-stable).
  8. attribution.enabled without apiServerCIDRs, or with an entry that is
     not an IPv4 /32.

The job controllers and the proxy re-validate the whole set at startup
(netprofile.Validate), RE2 compilation included.
*/}}
{{- define "patchy.egressProxyGuard" -}}
{{- if include "patchy.egressProxyEnabled" . -}}
{{- $ep := .Values.egressProxy -}}
{{- if not (.Values.agent.repositoryImages | default dict).enabled -}}
{{- fail "egressProxy.enabled requires agent.repositoryImages.enabled: a network profile only ever applies to a Job running an accepted repository image" -}}
{{- end -}}
{{- $profiles := .Values.agent.networkProfiles | default dict -}}
{{- if not $profiles -}}
{{- fail "egressProxy.enabled requires at least one agent.networkProfiles entry: the egress proxy and the job controllers refuse to start without a profile" -}}
{{- end -}}
{{- if not ($ep.networkPolicy | default dict).create -}}
{{- fail "egressProxy.networkPolicy.create must stay true while egressProxy.enabled: the per-profile ingress rules are the identity check, and without them any pod could use any profile" -}}
{{- end -}}
{{- if eq (include "patchy.egressMode" .) "istio" -}}
{{- fail "egressProxy.enabled is not supported under agent.networkPolicy.mode istio in step 1: the Sidecar's REGISTRY_ONLY egress and port-based identity have not been verified together" -}}
{{- end -}}
{{- $ports := dict -}}
{{- range $name := keys $profiles | sortAlpha -}}
{{- $p := index $profiles $name | default dict -}}
{{- $port := toString (int ($p.port | default 0)) -}}
{{- if hasKey $ports $port -}}
{{- fail (printf "agent.networkProfiles: %s and %s both use port %s; each profile needs its own port" (get $ports $port) $name $port) -}}
{{- end -}}
{{- $_ := set $ports $port $name -}}
{{- range $m := $p.modules | default list -}}
{{- $t := trimSuffix "/" (toString $m) -}}
{{- if or (eq $t "golang.org") (hasPrefix "golang.org/toolchain" $t) -}}
{{- fail (printf "agent.networkProfiles.%s.modules: %q would admit the Go toolchain modules; name the golang.org paths needed (golang.org/x/...)" $name (toString $m)) -}}
{{- end -}}
{{- end -}}
{{- if hasKey $p "bakedModCache" -}}
{{- $baked := $p.bakedModCache | default "" | toString -}}
{{- if and $baked (or (not (hasPrefix "/" $baked)) (hasSuffix "/" $baked) (contains "//" $baked) (regexMatch `(^|/)\.\.?(/|$)` $baked)) -}}
{{- fail (printf "agent.networkProfiles.%s.bakedModCache: %q is not an absolute clean path: no . or .. element, no empty element and no trailing slash (or leave it empty for the network only)" $name $baked) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $attr := $ep.attribution | default dict -}}
{{- if $attr.enabled -}}
{{- if not $attr.apiServerCIDRs -}}
{{- fail "egressProxy.attribution.enabled requires egressProxy.attribution.apiServerCIDRs: the API server's endpoint IPs as IPv4 /32s" -}}
{{- end -}}
{{- range $attr.apiServerCIDRs -}}
{{- if not (regexMatch `^([0-9]{1,3}\.){3}[0-9]{1,3}/32$` (toString .)) -}}
{{- fail (printf "egressProxy.attribution.apiServerCIDRs entry %q is not an IPv4 /32: name each API server endpoint IP, never a subnet, which would open every pod and node in it to the proxy" (toString .)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- include "patchy.egressRegistriesGuard" . -}}
{{- end -}}
{{- end }}

{{/*
Render-time guards for egressProxy.registries and the profiles' grants
(step 2), continuing patchy.egressProxyGuard's numbering; called from it, so
only while egressProxy.enabled:

   9. a grant names a registry missing from egressProxy.registries;
  10. a profile holds two grants of one ecosystem;
  11. a grant has neither packages nor packagePolicy: registry, or both;
  12. a codeartifact registry with cloudCredentials none, on ecosystem go,
      or whose URL host is not its domain's endpoint;
  13. a registry URL host on the deny list (patchy.egressDeniedHost: forges,
      model APIs, the broker's overrides, every profile's forge host, the
      Kubernetes API; in-cluster names and IP literals only when internal);
  14. a port other than 443 without internal;
  15. internal.cidrs overlapping the always-denied set (the fixed ranges,
      clusterDNSCIDR, attribution.apiServerCIDRs), or a non-/32 entry inside
      agent.networkPolicy.clusterCIDRs (the Kubernetes Service IP is not
      known here; the binary refuses it);
  16. bearer or basic without auth.secretName, none or codeartifact with
      one; auth.codeartifact on any other type, or missing on codeartifact;
  17. privateModules/publicChecksumLookups on a non-go registry; scripts or
      allowPrerelease on a non-npm grant, sdists on a non-pypi grant,
      allowPseudoVersions on a non-go grant; internal.targetPort without
      internal.podSelector;
  18. a go registry without both privateModules (a list, possibly empty)
      and publicChecksumLookups;
  19. a fileHosts entry that is denied or not an exact host; a
      redirectHosts entry that is denied, or a *. pattern that overlaps the
      deny list, covers a shared cloud suffix or has fewer than three
      labels after the * (patchy.egressDeniedPattern).

Beside them: a pypi URL that does not end /simple/, a profile with neither
a route nor a grant, and cloudCredentials irsa on a chart-created
ServiceAccount without the eks.amazonaws.com/role-arn annotation. The
egress proxy re-validates all of it at startup (netprofile.ParseRegistries
and CheckGrants), and the job controllers check every grant's hosts
against each Forge CR at launch.
*/}}
{{- define "patchy.egressRegistriesGuard" -}}
{{- $ep := .Values.egressProxy -}}
{{- $regs := $ep.registries | default dict -}}
{{- $cloud := $ep.cloudCredentials | default "none" -}}
{{- $extra := splitList " " (include "patchy.egressExtraDenyHosts" .) -}}
{{- $always := splitList " " (include "patchy.egressAlwaysDeniedCIDRs" .) -}}
{{- $alwaysWhy := dict -}}
{{- range $always }}{{ $_ := set $alwaysWhy . "the always-denied range" }}{{ end -}}
{{- with .Values.clusterDNSCIDR -}}
{{- if regexMatch `^[0-9.]+/[0-9]+$` (toString .) -}}
{{- $always = append $always (toString .) -}}
{{- $_ := set $alwaysWhy (toString .) "clusterDNSCIDR" -}}
{{- end -}}
{{- end -}}
{{- $attr := $ep.attribution | default dict -}}
{{- if $attr.enabled -}}
{{- range $attr.apiServerCIDRs | default list -}}
{{- $always = append $always (toString .) -}}
{{- $_ := set $alwaysWhy (toString .) "attribution.apiServerCIDRs" -}}
{{- end -}}
{{- end -}}
{{- $clusterCIDRs := list -}}
{{- range .Values.agent.networkPolicy.clusterCIDRs | default list -}}
{{- if regexMatch `^[0-9.]+/[0-9]+$` (toString .) }}{{ $clusterCIDRs = append $clusterCIDRs (toString .) }}{{ end -}}
{{- end -}}
{{- range $name := keys $regs | sortAlpha -}}
{{- $r := index $regs $name | default dict -}}
{{- $at := printf "egressProxy.registries.%s" $name -}}
{{- $eco := $r.ecosystem | default "" | toString -}}
{{- $auth := $r.auth | default dict -}}
{{- $type := $auth.type | default "" | toString -}}
{{- $u := urlParse ($r.url | default "" | toString) -}}
{{- $host := $u.hostname -}}
{{- $port := "443" -}}
{{- with regexFind `:[0-9]+$` $u.host }}{{ $port = trimPrefix ":" . }}{{ end -}}
{{- $in := $r.internal -}}
{{- $cidrs := list -}}
{{- with $in }}{{ $cidrs = .cidrs | default list }}{{ end -}}
{{- /* 16 */ -}}
{{- if and (has $type (list "bearer" "basic")) (not $auth.secretName) -}}
{{- fail (printf "%s.auth.secretName is required with auth.type %s: the Secret in the release namespace the proxy reads %s from (you create it; the chart never does)" $at $type (ternary "key token" "keys username and password" (eq $type "bearer"))) -}}
{{- end -}}
{{- if and (has $type (list "none" "codeartifact")) $auth.secretName -}}
{{- fail (printf "%s.auth.secretName is set with auth.type %s, which reads no Secret: drop it" $at $type) -}}
{{- end -}}
{{- if and (eq $type "codeartifact") (not $auth.codeartifact) -}}
{{- fail (printf "%s.auth.codeartifact (domain, domainOwner, region) is required with auth.type codeartifact" $at) -}}
{{- end -}}
{{- if and (ne $type "codeartifact") $auth.codeartifact -}}
{{- fail (printf "%s.auth.codeartifact is set with auth.type %s: it belongs to auth.type codeartifact only" $at $type) -}}
{{- end -}}
{{- /* 12 */ -}}
{{- if eq $type "codeartifact" -}}
{{- if eq $cloud "none" -}}
{{- fail (printf "%s is a codeartifact registry but egressProxy.cloudCredentials is none: set irsa or podIdentity so the proxy can mint its token" $at) -}}
{{- end -}}
{{- if eq $eco "go" -}}
{{- fail (printf "%s: codeartifact has no Go module support; a codeartifact registry is npm or pypi" $at) -}}
{{- end -}}
{{- $ca := $auth.codeartifact -}}
{{- $prefix := printf "%s-%s.d.codeartifact.%s" (toString $ca.domain) (toString $ca.domainOwner) (toString $ca.region) -}}
{{- if not (has $host (list (printf "%s.amazonaws.com" $prefix) (printf "%s.on.aws" $prefix))) -}}
{{- fail (printf "%s.url host %s is not the CodeArtifact endpoint of its auth.codeartifact domain, owner and region (%s.amazonaws.com): a typo here would send the token to another host" $at $host $prefix) -}}
{{- end -}}
{{- end -}}
{{- /* 13 */ -}}
{{- with include "patchy.egressDeniedHost" (dict "host" $host "internal" (not (not $in)) "cidrs" $cidrs "extra" $extra) -}}
{{- fail (printf "%s.url host %s %s: a registry may never be a forge, a model API, the Kubernetes API, or (without internal) an in-cluster or private address" $at $host .) -}}
{{- end -}}
{{- /* 14 */ -}}
{{- if and (ne $port "443") (not $in) -}}
{{- fail (printf "%s.url names port %s: a public registry is reached on 443 only; set internal (cidrs) for a private registry" $at $port) -}}
{{- end -}}
{{- if and (eq $eco "pypi") (not (hasSuffix "/simple/" $u.path)) -}}
{{- fail (printf "%s.url must be the index URL, ending /simple/" $at) -}}
{{- end -}}
{{- /* 15 */ -}}
{{- range $c := $cidrs -}}
{{- $c = toString $c -}}
{{- $rng := splitList " " (include "patchy.ipv4Range" $c) -}}
{{- if or (eq (index $rng 2) "0") (gt (atoi (index $rng 1)) 4294967295) -}}
{{- fail (printf "%s.internal.cidrs entry %s is not a masked IPv4 CIDR (write the network address)" $at $c) -}}
{{- end -}}
{{- range $d := $always -}}
{{- if include "patchy.ipv4Overlap" (list $c $d) -}}
{{- fail (printf "%s.internal.cidrs entry %s overlaps %s %s, which the proxy never dials" $at $c (get $alwaysWhy $d) $d) -}}
{{- end -}}
{{- end -}}
{{- range $cc := $clusterCIDRs -}}
{{- if and (include "patchy.ipv4Overlap" (list $c $cc)) (not (hasSuffix "/32" $c)) -}}
{{- fail (printf "%s.internal.cidrs entry %s is inside agent.networkPolicy.clusterCIDRs %s: list the registry's ClusterIP as a /32, never a range that could admit kubernetes.default or its neighbours" $at $c $cc) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- /* 17 and 18 */ -}}
{{- $hasPM := not (kindIs "invalid" $r.privateModules) -}}
{{- $hasPCL := not (kindIs "invalid" $r.publicChecksumLookups) -}}
{{- if and (ne $eco "go") (or $hasPM $hasPCL) -}}
{{- fail (printf "%s: privateModules and publicChecksumLookups are for go registries only" $at) -}}
{{- end -}}
{{- if and (eq $eco "go") (not (and $hasPM $hasPCL)) -}}
{{- fail (printf "%s is a go registry and must state both privateModules (a list, possibly empty: module prefixes only it serves, never looked up in sum.golang.org) and publicChecksumLookups (may other modules it serves be checked against sum.golang.org?); there is no default" $at) -}}
{{- end -}}
{{- if and $in $in.targetPort (not $in.podSelector) -}}
{{- fail (printf "%s.internal.targetPort is the podSelector rule's port: it needs internal.podSelector" $at) -}}
{{- end -}}
{{- /* 19 */ -}}
{{- range $r.fileHosts | default list -}}
{{- $fh := toString . -}}
{{- if or (hasPrefix "*." $fh) (regexMatch `^[0-9.]+$` $fh) -}}
{{- fail (printf "%s.fileHosts entry %s is not an exact host name: the agent chooses file paths there, so only exact hosts are allowed" $at $fh) -}}
{{- end -}}
{{- with include "patchy.egressDeniedHost" (dict "host" $fh "internal" (not (not $in)) "cidrs" $cidrs "extra" $extra) -}}
{{- fail (printf "%s.fileHosts entry %s %s" $at $fh .) -}}
{{- end -}}
{{- end -}}
{{- range $r.redirectHosts | default list -}}
{{- $rh := toString . -}}
{{- $why := "" -}}
{{- if hasPrefix "*." $rh -}}
{{- $why = include "patchy.egressDeniedPattern" (dict "host" $rh "internal" (not (not $in)) "cidrs" $cidrs "extra" $extra) -}}
{{- else -}}
{{- $why = include "patchy.egressDeniedHost" (dict "host" $rh "internal" (not (not $in)) "cidrs" $cidrs "extra" $extra) -}}
{{- end -}}
{{- with $why -}}
{{- fail (printf "%s.redirectHosts entry %s %s: a redirect is followed without the credential, but never to a denied or shared host" $at $rh .) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- /* The profiles' grants: 9, 10, 11 and 17's grant half. */ -}}
{{- $profiles := .Values.agent.networkProfiles | default dict -}}
{{- range $pname := keys $profiles | sortAlpha -}}
{{- $p := index $profiles $pname | default dict -}}
{{- $grants := $p.registries | default dict -}}
{{- if and (not $p.route) (not $grants) -}}
{{- fail (printf "agent.networkProfiles.%s needs route: go or at least one registries grant" $pname) -}}
{{- end -}}
{{- $seen := dict -}}
{{- range $gname := keys $grants | sortAlpha -}}
{{- $g := index $grants $gname | default dict -}}
{{- $at := printf "agent.networkProfiles.%s.registries.%s" $pname $gname -}}
{{- if not (hasKey $regs $gname) -}}
{{- fail (printf "%s grants a registry that egressProxy.registries does not define" $at) -}}
{{- end -}}
{{- $eco := (index $regs $gname | default dict).ecosystem | default "" | toString -}}
{{- if hasKey $seen $eco -}}
{{- fail (printf "agent.networkProfiles.%s grants two %s registries (%s and %s): a profile holds at most one grant per ecosystem, since each package manager takes one index" $pname $eco (get $seen $eco) $gname) -}}
{{- end -}}
{{- $_ := set $seen $eco $gname -}}
{{- if not (or $g.packages $g.packagePolicy) -}}
{{- fail (printf "%s must say what it admits: packages (an allowlist) or packagePolicy: registry (the registry's own curation); there is no default" $at) -}}
{{- end -}}
{{- if and $g.packages $g.packagePolicy -}}
{{- fail (printf "%s sets both packages and packagePolicy: choose one" $at) -}}
{{- end -}}
{{- if and (or $g.scripts $g.allowPrerelease) (ne $eco "npm") -}}
{{- fail (printf "%s: scripts and allowPrerelease are for npm grants only" $at) -}}
{{- end -}}
{{- if and $g.sdists (ne $eco "pypi") -}}
{{- fail (printf "%s: sdists is for pypi grants only" $at) -}}
{{- end -}}
{{- if and $g.allowPseudoVersions (ne $eco "go") -}}
{{- fail (printf "%s: allowPseudoVersions is for go grants only" $at) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- /* The proxy's own AWS identity. */ -}}
{{- if and (eq $cloud "irsa") (($ep.serviceAccount | default dict).create) (not (hasKey (($ep.serviceAccount | default dict).annotations | default dict) "eks.amazonaws.com/role-arn")) -}}
{{- fail "egressProxy.cloudCredentials irsa needs the role on egressProxy.serviceAccount.annotations (eks.amazonaws.com/role-arn)" -}}
{{- end -}}
{{- end }}

{{/*
The egress proxy's hard-deny table, as literal lists (netprofile/denied.go is
the binary's copy; TestDeniedHostMatchesChart reads these bodies and holds
them equal to it). Each is a space-separated list.

  forge and model hosts: each denies the host and every name below it;
  cluster hosts: the Kubernetes API's in-cluster names, denied exactly;
  cluster suffixes: in-cluster names, denied unless the registry is internal;
  too-wide suffixes: shared multi-tenant suffixes no redirectHosts pattern
    may equal or cover;
  always-denied CIDRs: no internal registry may ever list these.
*/}}
{{- define "patchy.egressDeniedForgeHosts" -}}
github.com githubusercontent.com
{{- end }}

{{- define "patchy.egressDeniedModelHosts" -}}
anthropic.com openai.com openai.azure.com services.ai.azure.com cognitiveservices.azure.com aiplatform.googleapis.com generativelanguage.googleapis.com
{{- end }}

{{- define "patchy.egressDeniedClusterHosts" -}}
kubernetes kubernetes.default kubernetes.default.svc kubernetes.default.svc.cluster.local
{{- end }}

{{- define "patchy.egressClusterSuffixes" -}}
svc cluster.local
{{- end }}

{{- define "patchy.egressTooWideSuffixes" -}}
amazonaws.com s3.amazonaws.com cloudfront.net googleapis.com storage.googleapis.com appspot.com azure.com windows.net blob.core.windows.net azureedge.net githubusercontent.com github.io fastly.net akamaized.net r2.cloudflarestorage.com
{{- end }}

{{- define "patchy.egressAlwaysDeniedCIDRs" -}}
0.0.0.0/8 127.0.0.0/8 169.254.0.0/16 224.0.0.0/4 240.0.0.0/4
{{- end }}

{{- define "patchy.egressMinPatternLabels" -}}
3
{{- end }}

{{/*
The hosts of the egress broker's base-URL overrides
(PATCHY_{ANTHROPIC,BEDROCK,VERTEX,FOUNDRY}_BASE_URL in config.extra or
egressBroker.config.extra), sorted and space-separated: model API hosts the
fixed table cannot know, denied to every registry, file host and redirect
hop (guard 13, and PATCHY_DENY_HOSTS for the proxy's request-time check). An
override set through extraEnvFrom is invisible here.
*/}}
{{- define "patchy.egressBrokerOverrideHosts" -}}
{{- $hosts := list -}}
{{- $broker := (.Values.egressBroker | default dict).config | default dict -}}
{{- range $src := list (.Values.config.extra | default dict) ($broker.extra | default dict) -}}
{{- range $k := list "PATCHY_ANTHROPIC_BASE_URL" "PATCHY_BEDROCK_BASE_URL" "PATCHY_VERTEX_BASE_URL" "PATCHY_FOUNDRY_BASE_URL" -}}
{{- with index $src $k -}}
{{- with lower (urlParse (toString .)).hostname -}}
{{- $hosts = append $hosts . -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $hosts | uniq | sortAlpha | join " " -}}
{{- end }}

{{/*
Every further denied host the chart knows, space-separated: each profile's
forge host (default github.com) and the broker's base-URL overrides.
*/}}
{{- define "patchy.egressExtraDenyHosts" -}}
{{- $hosts := list -}}
{{- range $p := values (.Values.agent.networkProfiles | default dict) -}}
{{- $hosts = append $hosts (($p | default dict).host | default "github.com" | toString | lower) -}}
{{- end -}}
{{- with include "patchy.egressBrokerOverrideHosts" . -}}
{{- $hosts = concat $hosts (splitList " " .) -}}
{{- end -}}
{{- $hosts | uniq | sortAlpha | join " " -}}
{{- end }}

{{/*
An IPv4 address or CIDR as "<first> <last> <masked>" integers ("masked" is
1 when the address is the prefix's first, as netip.Prefix.Masked demands):
the arithmetic guard 15's overlap checks need. A plain address is a /32.
*/}}
{{- define "patchy.ipv4Range" -}}
{{- $parts := splitList "/" (toString .) -}}
{{- $bits := 32 -}}
{{- if gt (len $parts) 1 }}{{ $bits = atoi (index $parts 1) }}{{ end -}}
{{- $o := splitList "." (index $parts 0) -}}
{{- $n := add (mul (atoi (index $o 0)) 16777216) (mul (atoi (index $o 1)) 65536) (mul (atoi (index $o 2)) 256) (atoi (index $o 3)) -}}
{{- $size := 1 -}}
{{- range until (int (sub 32 $bits)) }}{{ $size = mul $size 2 }}{{ end -}}
{{- $start := mul (div $n $size) $size -}}
{{- printf "%d %d %d" $start (sub (add $start $size) 1) (ternary 1 0 (eq $start $n)) -}}
{{- end }}

{{/*
Whether two IPv4 CIDRs overlap: "yes" or "". Context: list a b.
*/}}
{{- define "patchy.ipv4Overlap" -}}
{{- $a := splitList " " (include "patchy.ipv4Range" (index . 0)) -}}
{{- $b := splitList " " (include "patchy.ipv4Range" (index . 1)) -}}
{{- if and (le (atoi (index $a 0)) (atoi (index $b 1))) (le (atoi (index $b 0)) (atoi (index $a 1))) -}}
yes
{{- end -}}
{{- end }}

{{/*
netprofile.DeniedHost in the chart: why a host may not be a registry, file
or redirect host, or "" when it may. Context: dict "host" <host> "internal"
<bool: the registry is internal> "cidrs" <its internal.cidrs> "extra"
<further denied hosts, a list>.
*/}}
{{- define "patchy.egressDeniedHost" -}}
{{- $h := trimSuffix "." (lower (toString .host)) -}}
{{- $why := "" -}}
{{- if not $h -}}
{{- $why = "is empty" -}}
{{- else if or (hasPrefix "[" $h) (contains ":" $h) -}}
{{- $why = "is an IPv6 literal" -}}
{{- else if regexMatch `^[0-9]{1,3}(\.[0-9]{1,3}){3}$` $h -}}
{{- range splitList " " (include "patchy.egressAlwaysDeniedCIDRs" .) -}}
{{- if and (not $why) (include "patchy.ipv4Overlap" (list . $h)) -}}
{{- $why = printf "is in the always-denied range %s" . -}}
{{- end -}}
{{- end -}}
{{- if not $why -}}
{{- $inside := "" -}}
{{- range .cidrs | default list -}}
{{- if include "patchy.ipv4Overlap" (list (toString .) $h) }}{{ $inside = "yes" }}{{ end -}}
{{- end -}}
{{- if not $inside }}{{ $why = "is an IP literal outside the registry's internal.cidrs" }}{{ end -}}
{{- end -}}
{{- else -}}
{{- $denied := concat (splitList " " (include "patchy.egressDeniedForgeHosts" .)) (splitList " " (include "patchy.egressDeniedModelHosts" .)) (.extra | default list) -}}
{{- range $denied -}}
{{- $d := trimSuffix "." (lower (toString .)) -}}
{{- if and (not $why) $d (or (eq $h $d) (hasSuffix (printf ".%s" $d) $h)) -}}
{{- $why = printf "is %s or below it (a forge, a model API or the broker's)" $d -}}
{{- end -}}
{{- end -}}
{{- if and (not $why) (has $h (splitList " " (include "patchy.egressDeniedClusterHosts" .))) -}}
{{- $why = "is the Kubernetes API" -}}
{{- end -}}
{{- if not $why -}}
{{- $labels := splitList "." $h -}}
{{- $n := len $labels -}}
{{- if hasSuffix ".amazonaws.com" $h -}}
{{- if and (ge $n 4) (hasPrefix "bedrock" (index $labels (sub $n 4))) -}}
{{- $why = "is a Bedrock endpoint (bedrock*.<region>.amazonaws.com)" -}}
{{- else if and (ge $n 5) (eq (index $labels (sub $n 3)) "vpce") (hasPrefix "bedrock" (index $labels (sub $n 5))) -}}
{{- $why = "is a Bedrock VPC endpoint" -}}
{{- end -}}
{{- end -}}
{{- if and (eq $n 3) (eq (index $labels 1) "googleapis") (eq (index $labels 2) "com") (hasSuffix "-aiplatform" (index $labels 0)) -}}
{{- $why = "is a regional Vertex AI endpoint (*-aiplatform.googleapis.com)" -}}
{{- end -}}
{{- end -}}
{{- if and (not $why) (not .internal) -}}
{{- range splitList " " (include "patchy.egressClusterSuffixes" .) -}}
{{- if and (not $why) (or (eq $h .) (hasSuffix (printf ".%s" .) $h)) -}}
{{- $why = "is an in-cluster name and the registry is not internal" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $why -}}
{{- end }}

{{/*
netprofile.DeniedPattern in the chart: why a redirectHosts wildcard
"*.suffix" may not be used, or "" when it may. Same context as
patchy.egressDeniedHost.
*/}}
{{- define "patchy.egressDeniedPattern" -}}
{{- $suffix := trimPrefix "*." (toString .host) -}}
{{- $why := "" -}}
{{- $labels := splitList "." $suffix -}}
{{- $n := len $labels -}}
{{- if or (not (regexMatch `^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$` $suffix)) (regexMatch `(^|\.)-|-(\.|$)|\.\.` $suffix) -}}
{{- $why = "is not *. followed by a lowercase host name" -}}
{{- else if lt $n (int (include "patchy.egressMinPatternLabels" .)) -}}
{{- $why = printf "has fewer than %s labels after the *" (include "patchy.egressMinPatternLabels" .) -}}
{{- else if regexMatch `^[0-9]{1,3}(\.[0-9]{1,3}){3}$` $suffix -}}
{{- $why = "wildcards an address" -}}
{{- end -}}
{{- if not $why -}}
{{- range splitList " " (include "patchy.egressTooWideSuffixes" .) -}}
{{- if and (not $why) (or (eq . $suffix) (hasSuffix (printf ".%s" $suffix) .)) -}}
{{- $why = printf "covers the shared suffix %s" . -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if not $why -}}
{{- $denied := concat (splitList " " (include "patchy.egressDeniedForgeHosts" .)) (splitList " " (include "patchy.egressDeniedModelHosts" .)) (.extra | default list) -}}
{{- range $denied -}}
{{- $d := trimSuffix "." (lower (toString .)) -}}
{{- if and (not $why) $d (or (eq $suffix $d) (hasSuffix (printf ".%s" $d) $suffix) (hasSuffix (printf ".%s" $suffix) $d)) -}}
{{- $why = printf "overlaps the denied %s" $d -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if not $why -}}
{{- range splitList " " (include "patchy.egressDeniedClusterHosts" .) -}}
{{- if and (not $why) (or (eq . $suffix) (hasSuffix (printf ".%s" $suffix) .)) -}}
{{- $why = printf "can match the Kubernetes API name %s" . -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if and (not $why) (not .internal) -}}
{{- range splitList " " (include "patchy.egressClusterSuffixes" .) -}}
{{- if and (not $why) (or (eq $suffix .) (hasSuffix (printf ".%s" .) $suffix)) -}}
{{- $why = "is an in-cluster name" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if and (not $why) (hasSuffix ".amazonaws.com" (printf ".%s" $suffix)) -}}
{{- if or (le $n 3) (hasPrefix "bedrock" (index $labels (sub $n 4))) -}}
{{- $why = "can match a Bedrock endpoint" -}}
{{- else if and (eq (index $labels (sub $n 3)) "vpce") (or (le $n 4) (hasPrefix "bedrock" (index $labels (sub $n 5)))) -}}
{{- $why = "can match a Bedrock VPC endpoint" -}}
{{- end -}}
{{- end -}}
{{- $why -}}
{{- end }}
