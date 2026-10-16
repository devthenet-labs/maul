#!/bin/sh
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Render assertions for charts/patchy: what hack/helm-lint.sh's plain renders
# cannot see. Which PATCHY_* keys land in which controller's ConfigMap, which
# Deployment mounts what, that each render-time guard fails with its message
# and passes once its value is fixed, and that the feature-off render carries
# none of it. A final section does the same for charts/patchy-config's
# Project CRs and their values schema. Fixtures live in
# hack/testdata/chart-render/; yq (pinned in
# mise.toml) reads the rendered documents. Runs as part of `mise run
# helm-lint`. Every assertion runs; the exit status is the failure count.
set -eu
# Byte-order collation: some assertions compare a yq key list with a list
# sorted by sort(1), and under a UTF-8 locale sort ignores punctuation
# (PATCHY_INTENT_PR_POLL_INTERVAL vs PATCHY_INTENT_PREVIEWS_ENABLED), so the
# test failed on developer machines while passing in CI's C locale.
export LC_ALL=C

chart=charts/patchy
fixtures=hack/testdata/chart-render
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
failures=0

fail() {
  echo "chart-render-test: FAIL: $*" >&2
  failures=$((failures + 1))
}

# render NAME [helm args...]: render into $out/NAME.yaml; a failed render is
# itself a failure (and leaves an empty file, so later assertions fail too).
render() {
  name=$1
  shift
  if ! helm template patchy "$chart" --namespace patchy "$@" >"$out/$name.yaml" 2>"$out/$name.err"; then
    fail "$name: render failed: $(cat "$out/$name.err")"
    : >"$out/$name.yaml"
  fi
}

# get NAME EXPR: evaluate a yq expression against every document of a render.
get() {
  yq eval "$2" "$out/$1.yaml" | grep -v '^---$' || true
}

# expect NAME EXPR WANT: the expression yields exactly WANT ("null" for an
# absent key, "" for no matching document).
expect() {
  got=$(get "$1" "$2")
  if [ "$got" != "$3" ]; then
    fail "$1: $2 = '$got', want '$3'"
  fi
}

# cm NAME CONFIGMAP KEY WANT: one ConfigMap data key.
cm() {
  expect "$1" "select(.kind == \"ConfigMap\" and .metadata.name == \"patchy-$2-config\") | .data.$3" "$4"
}

# expect_fail DESC NEEDLE [helm args...]: the render fails and its error
# contains NEEDLE verbatim.
expect_fail() {
  desc=$1
  needle=$2
  shift 2
  if helm template patchy "$chart" --namespace patchy "$@" >/dev/null 2>"$out/guard.err"; then
    fail "guard $desc: render succeeded, want a failure containing: $needle"
  elif ! grep -qF -- "$needle" "$out/guard.err"; then
    fail "guard $desc: error lacks '$needle': $(cat "$out/guard.err")"
  fi
}

# same_render WANT GOT: GOT is the render WANT records, whichever helm
# rendered either. Every line counts byte for byte, comments included, except
# blank ones: helm 4.3 writes two blank lines before each document a
# {{- range }} emits where 4.2 wrote none, and changed nothing else, so a
# toolchain bump must not read as a changed policy. A blank line inside a
# block scalar is content, though, so the parsed documents must be equal too.
# On a difference, $out/same.diff says what differs.
same_render() {
  grep -v '^[[:space:]]*$' "$1" >"$out/same.want" || true
  grep -v '^[[:space:]]*$' "$2" >"$out/same.got" || true
  if ! cmp -s "$out/same.want" "$out/same.got"; then
    { echo "(blank lines left out)"; diff "$out/same.want" "$out/same.got"; } >"$out/same.diff" || true
    return 1
  fi
  if ! yq -o=json -I=0 '.' "$1" >"$out/same.want.json" 2>"$out/same.diff" ||
    ! yq -o=json -I=0 '.' "$2" >"$out/same.got.json" 2>"$out/same.diff"; then
    return 1
  fi
  if ! cmp -s "$out/same.want.json" "$out/same.got.json"; then
    { echo "the parsed documents differ (a blank line inside a block scalar?)"; diff "$out/same.want.json" "$out/same.got.json"; } >"$out/same.diff" || true
    return 1
  fi
}

# golden_render NAME FILE [helm args...]: the render (pass --show-only for one
# template) is the committed golden FILE, as same_render compares them.
golden_render() {
  name=$1
  file=$2
  shift 2
  if ! helm template patchy "$chart" --namespace patchy "$@" >"$out/$name.golden" 2>"$out/$name.err"; then
    fail "$name: render failed: $(cat "$out/$name.err")"
  elif ! same_render "$file" "$out/$name.golden"; then
    fail "$name: render differs from $file: $(head -20 "$out/same.diff")"
  fi
}

# same_render_is DESC WANT FILE: same_render's verdict on FILE against the
# default golden is WANT (same or differs).
same_render_is() {
  got=differs
  if same_render "$golden/preview-admission.default.yaml" "$3"; then
    got=same
  fi
  if [ "$got" != "$2" ]; then
    fail "golden comparison, $1: $got, want $2"
  fi
}

# notes NAME [helm args...]: render the install NOTES into $out/NAME.notes.
# helm template never renders NOTES.txt; a client-side dry-run install does,
# without a cluster. A failed render is itself a failure.
notes() {
  name=$1
  shift
  if helm install patchy "$chart" --namespace patchy --dry-run=client "$@" >"$out/$name.install" 2>"$out/$name.err"; then
    sed -n '/^NOTES:$/,$p' "$out/$name.install" >"$out/$name.notes"
  else
    fail "$name: dry-run install failed: $(cat "$out/$name.err")"
    : >"$out/$name.notes"
  fi
}

# notes_has NAME NEEDLE WANT: whether the NOTES of a notes render contain
# NEEDLE verbatim is WANT (yes or no).
notes_has() {
  got=no
  if grep -qF -- "$2" "$out/$1.notes"; then
    got=yes
  fi
  if [ "$got" != "$3" ]; then
    fail "$1: NOTES contain '$2': $got, want $3"
  fi
}

# ---- feature off: the default render carries none of it --------------------
render default
expect default 'select(.metadata.name == "alb-preview" or (.metadata.name | test("^patchy-preview-")) or (.metadata.namespace | test("^patchy-preview-"))) | .kind' ""
expect default 'select(.metadata.name == "patchy-preview-controller") | .kind' ""
for key in PATCHY_REPOSITORY_IMAGES PATCHY_REPOSITORY_IMAGE_REGISTRIES PATCHY_REPOSITORY_IMAGE_ON_REJECT \
  PATCHY_REPOSITORY_IMAGE_COSIGN_KEY_FILE PATCHY_AGENT_EPHEMERAL_STORAGE PATCHY_CHANGESET_MAX_ENTRIES DOCKER_CONFIG \
  PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES; do
  expect default "select(.kind == \"ConfigMap\") | .data.$key | select(. != null)" ""
done
expect default 'select(.metadata.name == "patchy-repository-image-key") | .kind' ""
expect default 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-agent") | .imagePullSecrets' "null"
expect default 'select(.kind == "Secret") | .metadata.name' ""
expect default 'select(.kind == "Deployment") | .spec.template.spec.volumes[].name | select(. == "registry" or . == "repository-image-key")' ""
expect default 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-source-controller") | .spec.egress[].to[].ipBlock.cidr | select(. != null)' ""
for key in PATCHY_REQUESTS_PER_POD PATCHY_CONCURRENT_PER_POD PATCHY_CONCURRENCY_WAIT PATCHY_TOKENS_PER_POD \
  PATCHY_TOKENS_PER_HOUR PATCHY_MAX_TOKENS_CEILING PATCHY_MODEL_ALLOWLIST PATCHY_BETA_DENYLIST \
  PATCHY_MAX_ANTHROPIC_REQUEST_BYTES PATCHY_MAX_REQUEST_BYTES PATCHY_PREAUTH_REQUESTS_PER_SECOND \
  PATCHY_PREAUTH_BURST PATCHY_TOKEN_REVIEWS_PER_SECOND PATCHY_LIMIT_UNIT PATCHY_COST_PER_POD \
  PATCHY_COST_PER_HOUR; do
  cm default egress-broker "$key" null
done
expect default 'select(.metadata.name == "patchy-egress-broker-budgets") | .kind' ""
render default-cilium --set agent.networkPolicy.mode=cilium
expect default-cilium 'select(.metadata.name == "patchy-source-controller-cloud-credentials") | .kind' ""

# ---- preview security foundation (no workloads or ALB until later) ---------
render preview -f "$fixtures/preview-foundation.yaml"
render preview-guardrails -f "$fixtures/preview-foundation.yaml" --set preview.placeholder.enabled=false
render preview-runtime -f "$fixtures/preview-foundation.yaml" \
  -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml"
expect preview-guardrails 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-ingresses") | .metadata.name' patchy-preview-ingresses
expect preview-guardrails 'select(.metadata.name == "patchy-preview-placeholder") | .kind' ""
expect preview-guardrails 'select(.kind == "Ingress" and (.metadata.namespace | test("^patchy-preview-"))) | .metadata.name' ""
expect preview 'select(.kind == "Service" and .metadata.name == "patchy-preview-placeholder") | .metadata.namespace' patchy-preview-0
expect preview 'select(.kind == "Service" and .metadata.name == "patchy-preview-placeholder") | .spec.type' ClusterIP
expect preview 'select(.kind == "Service" and .metadata.name == "patchy-preview-placeholder") | .spec | has("selector")' false
expect preview 'select(.kind == "Service" and .metadata.name == "patchy-preview-placeholder") | .metadata.annotations."helm.sh/resource-policy"' keep
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .metadata.namespace' patchy-preview-0
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .spec.ingressClassName' alb-preview
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .spec.rules[0].host' placeholder.preview.patchy.devthe.net
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .spec.rules[0].http.paths[0].backend.service.name' patchy-preview-placeholder
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .metadata.annotations."helm.sh/resource-policy"' keep
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .spec | has("tls")' false
expect preview 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .spec | has("defaultBackend")' false
expect preview 'select(.kind == "Pod" and (.metadata.namespace | test("^patchy-preview-"))) | .metadata.name' ""
expect preview-runtime 'select(.kind == "Deployment" and .metadata.name == "patchy-preview-controller") | .spec.template.spec.containers[0].image | split(":") | .[0]' \
  ghcr.io/devthenet-labs/patchy/preview-controller
expect preview-runtime 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-preview-controller") | .metadata.namespace' patchy
expect preview-runtime 'select(.kind == "Role" and .metadata.name == "patchy-preview-controller") | .metadata.namespace' \
  'patchy
patchy-preview-0
patchy-preview-1'
expect preview-runtime 'select(.kind == "ClusterRole" and .metadata.name == "patchy-preview-controller") | .kind' ""
expect preview-runtime 'select(.kind == "Role" and .metadata.namespace == "patchy" and .metadata.name == "patchy-preview-controller") | .rules[] | select(.resources[] == "projects") | .verbs[]' get
expect preview-runtime 'select(.kind == "Role" and .metadata.name == "patchy-preview-controller") | .rules[].resources[] | select(. == "secrets" or . == "namespaces" or . == "networkpolicies")' ""
# It records Events (leader election, reconciles) in its own namespace like every other controller; without this its
# event recorder is refused at startup.
expect preview-runtime 'select(.kind == "Role" and .metadata.namespace == "patchy" and .metadata.name == "patchy-preview-controller") | .rules[] | select(.resources[] == "events") | .verbs | join(",")' create,patch
expect preview-runtime 'select(.kind == "Role" and .metadata.namespace == "patchy" and .metadata.name == "patchy-preview-controller") | .rules[] | select(.resources[] == "events") | .apiGroups | join(",")' ',events.k8s.io'
expect preview-runtime 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-preview-controller") | .spec.egress[].to[].ipBlock.cidr | select(. != null)' 172.20.0.1/32
cm preview-runtime preview-controller PATCHY_PREVIEW_SLOT_COUNT 2
cm preview-runtime preview-controller PATCHY_PREVIEW_IMAGE_PREFIX 111122223333.dkr.ecr.us-east-1.amazonaws.com/patchy/previews/
cm preview-runtime preview-controller PATCHY_PREVIEW_HOST_SUFFIX preview.patchy.devthe.net
cm preview-runtime preview-controller PATCHY_PREVIEW_NODE_POOL patchy-preview
cm preview-runtime preview-controller PATCHY_PREVIEW_NODE_CLASS patchy-preview
cm preview-runtime preview-controller PATCHY_PREVIEW_TAINT_KEY patchy.devthe.net/preview-only
cm preview-runtime intent-controller PATCHY_INTENT_PREVIEWS_ENABLED true
# Ready waits for the load balancer's target health by default (previews run
# only on EKS Auto Mode, which was seen injecting the gate on a live preview):
# the controller is told to, and the slot namespaces opt into Auto Mode's
# readiness-gate injection. Opting out drops both, so the slots run ungated
# exactly as slice 2 ran them.
cm preview-runtime preview-controller PATCHY_PREVIEW_TARGET_HEALTH true
expect preview-runtime 'select(.kind == "Namespace" and (.metadata.name | test("^patchy-preview-"))) | .metadata.labels."eks.amazonaws.com/pod-readiness-gate-inject"' 'enabled
enabled'
render preview-runtime-ungated -f "$fixtures/preview-foundation.yaml" \
  -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml" \
  --set previewController.config.targetHealth=false
cm preview-runtime-ungated preview-controller PATCHY_PREVIEW_TARGET_HEALTH false
# The legacy slot Ingress is on unless the operator turns it off for the
# preview gateway; the value only reaches the controller.
cm preview-runtime preview-controller PATCHY_PREVIEW_LEGACY_INGRESS true
render preview-runtime-gateway -f "$fixtures/preview-foundation.yaml" \
  -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml" \
  --set previewController.config.legacyIngress=false
cm preview-runtime-gateway preview-controller PATCHY_PREVIEW_LEGACY_INGRESS false
# preview.legacyIngress is the value now; previewController.config.legacyIngress
# (its spelling before the move, above) still wins while set, and NOTES ask
# to move it. Off the legacy path the slot namespaces drop the readiness-gate
# label (no load balancer target), and the ALB objects render only while
# albName is still set.
render preview-runtime-nolegacy -f "$fixtures/preview-foundation.yaml" \
  -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml" --set preview.legacyIngress=false
cm preview-runtime-nolegacy preview-controller PATCHY_PREVIEW_LEGACY_INGRESS false
render preview-runtime-alias-wins -f "$fixtures/preview-foundation.yaml" \
  -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml" --set preview.legacyIngress=false \
  --set previewController.config.legacyIngress=true
cm preview-runtime-alias-wins preview-controller PATCHY_PREVIEW_LEGACY_INGRESS true
expect preview-runtime-nolegacy 'select(.kind == "Namespace" and (.metadata.name | test("^patchy-preview-"))) | .metadata.labels."eks.amazonaws.com/pod-readiness-gate-inject"' 'null
null'
for kind in IngressClass IngressClassParams; do
  expect preview-runtime-nolegacy "select(.kind == \"$kind\") | .metadata.name" alb-preview
done
expect preview-runtime-nolegacy 'select(.metadata.name == "patchy-preview-placeholder") | .kind + "/" + .metadata.namespace' 'Service/patchy-preview-0
Ingress/patchy-preview-0'
notes notes-preview-nolegacy -f "$fixtures/preview-foundation.yaml" --set preview.legacyIngress=false
notes_has notes-preview-nolegacy "DEPRECATED: preview.legacyIngress is false, so the preview ALB serves nothing," yes
notes_has notes-preview-nolegacy "but preview.albName, preview.certificateARN, preview.albSubnetCIDRs are still set" yes
notes_has notes-preview-nolegacy "at the preview gateway's edge" yes
notes notes-preview-legacy -f "$fixtures/preview-foundation.yaml"
notes_has notes-preview-legacy "preview ALB serves nothing" no
notes_has notes-preview-legacy "previewController.config.legacyIngress moved" no
notes notes-preview-alias -f "$fixtures/preview-foundation.yaml" --set previewController.config.legacyIngress=false
notes_has notes-preview-alias "DEPRECATED: previewController.config.legacyIngress moved to preview.legacyIngress." yes
# Off the legacy path with every ALB value unset: no ALB object at all, no
# placeholder needed by the controller, and with nothing to admit (no ALB
# subnets, no gateway) the slot policy admits no ingress at all.
noalb=$out/no-alb.yaml
printf 'preview:\n    legacyIngress: false\n    albSubnetCIDRs: []\n    inboundCIDRs: []\n    certificateARN: ""\n    albName: ""\n    placeholder:\n        enabled: false\n' >"$noalb"
render preview-noalb -f "$fixtures/preview-foundation.yaml" -f "$fixtures/intent-controller.yaml" \
  -f "$fixtures/preview-controller.yaml" -f "$noalb"
expect preview-noalb 'select(.kind == "IngressClass" or .kind == "IngressClassParams" or .kind == "Ingress" or .metadata.name == "patchy-preview-placeholder") | .kind' ''
expect preview-noalb 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.ingress | length' '0
0'
expect preview-noalb 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.policyTypes | join(",")' 'Ingress,Egress
Ingress,Egress'
notes notes-preview-noalb -f "$fixtures/preview-foundation.yaml" -f "$noalb"
notes_has notes-preview-noalb "1-2. No preview ALB" yes
notes_has notes-preview-noalb "COST: the preview ALB" no
notes_has notes-preview-noalb "DEPRECATED: preview.legacyIngress" no
expect_fail 'legacy path without the ALB name' 'preview.albName is required when preview.enabled=true with the preview ALB' \
  -f "$fixtures/preview-foundation.yaml" -f "$noalb" --set preview.legacyIngress=true \
  --set-json 'preview.albSubnetCIDRs=["10.40.128.0/24"]' --set-json 'preview.inboundCIDRs=["203.0.113.10/32"]' \
  --set preview.certificateARN=arn:aws:acm:us-east-1:111122223333:certificate/a885812f-7587-4150-9196-5b160567cf54
expect_fail 'legacy path without ALB subnets' 'preview.albSubnetCIDRs is required' \
  -f "$fixtures/preview-foundation.yaml" -f "$noalb" --set preview.legacyIngress=true
# Any registry host: preview images need not come from ECR.
for registry in ghcr.io registry.example.com:5000; do
  render "preview-registry-$(echo "$registry" | tr -c 'a-z0-9\n' '-')" -f "$fixtures/preview-foundation.yaml" \
    -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml" --set preview.imageRegistry=$registry
done
cm preview-registry-ghcr-io preview-controller PATCHY_PREVIEW_IMAGE_PREFIX ghcr.io/patchy/previews/
cm preview-registry-registry-example-com-5000 preview-controller PATCHY_PREVIEW_IMAGE_PREFIX registry.example.com:5000/patchy/previews/
expect preview-registry-ghcr-io 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-pods") | .spec.validations[0].expression' "variables.pod.containers.all(c,
  c.image.startsWith('ghcr.io/patchy/previews/') &&
  c.image.substring(24).matches('^[a-z0-9-]+:sha-[0-9a-f]{40}\$'))"
expect_fail 'preview registry with a path' "'/preview/imageRegistry'" -f "$fixtures/preview-foundation.yaml" \
  --set preview.imageRegistry=ghcr.io/acme
expect_fail 'preview registry uppercase' "'/preview/imageRegistry'" -f "$fixtures/preview-foundation.yaml" \
  --set preview.imageRegistry=GHCR.io
# Off EKS Auto Mode, slot DNS needs no address: the kube-system selector,
# as every other chart policy has. On Auto Mode an address stays required.
render preview-dns-selector -f "$fixtures/preview-foundation.yaml" --set preview.dnsCIDR= \
  --set preview.nodeIsolation.nodePool= --set preview.nodeIsolation.nodeClass= \
  --set-json 'preview.nodeIsolation.selector={"patchy.example.com/previews":"true"}'
expect preview-dns-selector 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.egress[0].to[] | .namespaceSelector.matchLabels."kubernetes.io/metadata.name" + " " + (has("ipBlock") | tostring)' 'kube-system false
kube-system false'
expect preview-runtime-ungated 'select(.kind == "Namespace" and (.metadata.name | test("^patchy-preview-"))) | .metadata.labels."eks.amazonaws.com/pod-readiness-gate-inject"' 'null
null'
expect_fail 'preview controller without slots' 'requires preview.enabled' \
  -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml"
expect_fail 'preview controller without stable placeholder' 'requires preview.placeholder.enabled' \
  -f "$fixtures/preview-foundation.yaml" -f "$fixtures/intent-controller.yaml" \
  -f "$fixtures/preview-controller.yaml" --set preview.placeholder.enabled=false
expect_fail 'preview controller without intent writer' 'requires intentController.enabled' \
  -f "$fixtures/preview-foundation.yaml" -f "$fixtures/preview-controller.yaml"
expect_fail 'preview controller without API CIDR' 'apiServerCIDR is required' \
  -f "$fixtures/preview-foundation.yaml" -f "$fixtures/intent-controller.yaml" \
  --set previewController.enabled=true
expect_fail 'preview controller with broad API CIDR' 'does not match pattern' \
  -f "$fixtures/preview-foundation.yaml" -f "$fixtures/intent-controller.yaml" \
  --set previewController.enabled=true --set previewController.config.apiServerCIDR=0.0.0.0/0
expect preview 'select(.kind == "Namespace" and (.metadata.name | test("^patchy-preview-"))) | .metadata.name' 'patchy-preview-0
patchy-preview-1'
expect preview 'select(.kind == "Namespace" and (.metadata.name | test("^patchy-preview-"))) | .metadata.labels."pod-security.kubernetes.io/enforce"' 'restricted
restricted'
for kind in Namespace NetworkPolicy ResourceQuota LimitRange ValidatingAdmissionPolicy ValidatingAdmissionPolicyBinding IngressClass IngressClassParams; do
  expect preview "select(.kind == \"$kind\" and ((.metadata.name | test(\"preview\")) or (.metadata.namespace | test(\"preview\")))) | .metadata.annotations.\"helm.sh/resource-policy\" | select(. != \"keep\")" ""
done
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .metadata.namespace' 'patchy-preview-0
patchy-preview-1'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.policyTypes | join(",")' 'Ingress,Egress
Ingress,Egress'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.podSelector | length' '0
0'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.egress | length' '1
1'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.egress[0].to | length' '1
1'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.egress[0].ports | map(.protocol + "/" + (.port | tostring)) | join(",")' 'UDP/53,TCP/53
UDP/53,TCP/53'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.egress[].to[].ipBlock.cidr' '172.20.0.10/32
172.20.0.10/32'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.ingress[].from[].ipBlock.cidr' '10.40.128.0/24
10.40.129.0/24
10.40.128.0/24
10.40.129.0/24'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.ingress[].ports[].port' 'http
http'
expect preview 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.ingress | length' '1
1'
# The slot quota holds one Preview of up to four components: a Service each
# plus slot 0's placeholder, and every component's Pod replaced at once.
expect preview 'select(.kind == "ResourceQuota" and .metadata.name == "preview-quota") | .spec.hard.services + "/" + .spec.hard.pods' '5/8
5/8'
expect preview 'select(.kind == "ResourceQuota" and .metadata.name == "preview-quota") | .spec.hard."services.loadbalancers" + "/" + .spec.hard."services.nodeports"' '0/0
0/0'
# The multi-component admission rules are rendered into the slot policies.
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name == "patchy-preview-pods" or .metadata.name == "patchy-preview-deployments")) | .spec.validations[].expression | select(. == "size(variables.pod.containers) == 1")' 'size(variables.pod.containers) == 1
size(variables.pod.containers) == 1'
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-ingresses") | .spec.validations[].message | select(test("one rule of at most 4 Prefix paths"))' \
  'preview Ingresses have one rule of at most 4 Prefix paths in the component path grammar, each backed by a preview- Service on port 80'
expect preview 'select(.kind == "IngressClass" and .metadata.name == "alb-preview") | .metadata.annotations."ingressclass.kubernetes.io/is-default-class"' 'false'
expect preview 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.namespaceSelector.matchExpressions[0].values | join(",")' 'patchy-preview-0,patchy-preview-1'
expect preview 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.inboundCIDRs | join(",")' '203.0.113.10/32'
expect preview 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.certificateARNs | length' '1'
expect preview 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.sslPolicy' 'ELBSecurityPolicy-TLS13-1-2-2021-06'
expect preview 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.listeners[0].protocol + "/" + (.spec.listeners[0].port | tostring)' 'HTTPS/443'
# albSubnetIDs pins the preview ALB to the subnets whose CIDRs the slots
# admit; unset, placement stays with Auto Mode's discovery, as before.
expect preview 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec | has("subnets")' 'false'
render preview-subnets -f "$fixtures/preview-foundation.yaml" \
  --set 'preview.albSubnetIDs={subnet-0123456789abcdef0,subnet-0123456789abcdef1}'
expect preview-subnets 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.subnets.ids | join(",")' 'subnet-0123456789abcdef0,subnet-0123456789abcdef1'
expect preview-subnets 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.subnets | keys | join(",")' 'ids'
expect_fail 'preview subnet IDs not one per CIDR' 'must name one subnet per preview.albSubnetCIDRs entry' \
  -f "$fixtures/preview-foundation.yaml" --set 'preview.albSubnetIDs={subnet-0123456789abcdef0}'
expect_fail 'preview subnet ID that is not one' 'does not match pattern' \
  -f "$fixtures/preview-foundation.yaml" --set 'preview.albSubnetIDs={sg-0123456789abcdef0,subnet-0123456789abcdef1}'
# prefixListsIDs admits managed prefix lists beside inboundCIDRs; unset, the
# class renders exactly as before.
expect preview 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec | has("prefixListsIDs")' 'false'
render preview-prefix-lists -f "$fixtures/preview-foundation.yaml" \
  --set 'preview.prefixListsIDs={pl-0123456789abcdef0,pl-01234567}'
expect preview-prefix-lists 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.prefixListsIDs | join(",")' 'pl-0123456789abcdef0,pl-01234567'
expect preview-prefix-lists 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec.inboundCIDRs | join(",")' '203.0.113.10/32'
expect_fail 'preview prefix list ID that is not one' 'does not match pattern' \
  -f "$fixtures/preview-foundation.yaml" --set 'preview.prefixListsIDs={sg-0123456789abcdef0}'
printf 'preview:\n    inboundCIDRs: []\n    prefixListsIDs: [pl-0123456789abcdef0]\n' >"$out/prefix-lists-only.yaml"
expect_fail 'preview prefix lists without inbound CIDRs' 'preview.inboundCIDRs is required' \
  -f "$fixtures/preview-foundation.yaml" -f "$out/prefix-lists-only.yaml"
# No slot workload may read a Secret through its environment (kept, its own
# policy), and the slot Service and Ingress policies never evaluate an UPDATE
# that is a deletion's or changes neither spec nor annotations, so Auto Mode
# can always remove its finalizer from an Ingress that no longer conforms.
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-secret-refs") | .metadata.annotations."helm.sh/resource-policy"' 'keep'
expect preview 'select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == "patchy-preview-all-slots-secret-refs") | .spec.policyName + "/" + (.spec.validationActions | join(","))' 'patchy-preview-secret-refs/Deny'
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and .spec.matchConditions[].name == "not-metadata-only") | .metadata.name' 'patchy-preview-services
patchy-preview-ingresses'
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("^patchy-preview-"))) | .spec.failurePolicy' 'Fail
Fail
Fail
Fail
Fail
Fail
Fail
Fail'
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("^patchy-preview-outside-"))) | .metadata.name' 'patchy-preview-outside-pods
patchy-preview-outside-deployments
patchy-preview-outside-ingresses'
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("^patchy-preview-outside-"))) | .spec.matchConditions[0].expression' '!(request.namespace in ["patchy-preview-0","patchy-preview-1"])
!(request.namespace in ["patchy-preview-0","patchy-preview-1"])
!(request.namespace in ["patchy-preview-0","patchy-preview-1"])'
expect preview 'select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name | test("^patchy-preview-")) and (.metadata.name | test("^patchy-preview-outside-") | not)) | .spec.matchConditions[0].expression' 'request.namespace in ["patchy-preview-0","patchy-preview-1","patchy-preview-2","patchy-preview-3"]
request.namespace in ["patchy-preview-0","patchy-preview-1","patchy-preview-2","patchy-preview-3"]
request.namespace in ["patchy-preview-0","patchy-preview-1","patchy-preview-2","patchy-preview-3"]
request.namespace in ["patchy-preview-0","patchy-preview-1","patchy-preview-2","patchy-preview-3"]
request.namespace in ["patchy-preview-0","patchy-preview-1","patchy-preview-2","patchy-preview-3"]'
expect preview 'select(.kind == "ValidatingAdmissionPolicyBinding" and (.metadata.name | test("^patchy-preview-all-slots-"))) | has("spec")' 'true
true
true
true
true'
expect preview 'select(.kind == "ValidatingAdmissionPolicyBinding" and (.metadata.name | test("^patchy-preview-0|^patchy-preview-1"))) | .spec.matchResources.namespaceSelector.matchLabels."kubernetes.io/metadata.name"' 'patchy-preview-0
patchy-preview-1
patchy-preview-0
patchy-preview-1
patchy-preview-0
patchy-preview-1
patchy-preview-0
patchy-preview-1'
render preview-one -f "$fixtures/preview-foundation.yaml" --set preview.slotCount=1
expect preview-one 'select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == "patchy-preview-0-pods") | .spec.matchResources.namespaceSelector.matchLabels."kubernetes.io/metadata.name"' 'patchy-preview-0'
expect preview-one 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-outside-pods") | .spec.matchConditions[0].expression' '!(request.namespace in ["patchy-preview-0"])'
expect preview 'select(.kind == "Deployment" and (.metadata.namespace | test("preview"))) | .metadata.name' ""
expect_fail 'preview missing image registry' 'preview.imageRegistry' --set preview.enabled=true
expect_fail 'preview missing node pool' 'preview.nodeIsolation.nodePool' -f "$fixtures/preview-foundation.yaml" --set preview.nodeIsolation.nodePool=
expect_fail 'preview missing node class' 'preview.nodeIsolation.nodeClass' -f "$fixtures/preview-foundation.yaml" --set preview.nodeIsolation.nodeClass=
expect_fail 'preview missing node taint key' 'preview.nodeIsolation.taintKey' -f "$fixtures/preview-foundation.yaml" --set preview.nodeIsolation.taintKey=
expect_fail 'preview zero slots' "'/preview/slotCount': minimum" -f "$fixtures/preview-foundation.yaml" --set preview.slotCount=0
expect_fail 'preview zero slots, schema skipped' 'preview.slotCount must be between 1 and 4' \
  -f "$fixtures/preview-foundation.yaml" --set preview.slotCount=0 --skip-schema-validation
expect_fail 'preview missing cert' 'preview.certificateARN' -f "$fixtures/preview-foundation.yaml" --set preview.certificateARN=

# ---- generic node isolation (preview.nodeIsolation) --------------------------
# eksAutoMode.{nodePool,nodeClass} is the new spelling of the legacy
# nodePool/nodeClass: the whole render is byte for byte the legacy one. A
# generic selector joins the EKS labels in the slot admission policy and the
# controller's ConfigMap; mode none renders neither selector nor toleration.
eksiso=$out/eks-isolation.yaml
printf 'preview:\n    nodeIsolation:\n        nodePool: ""\n        nodeClass: ""\n        eksAutoMode:\n            nodePool: patchy-preview\n            nodeClass: patchy-preview\n' >"$eksiso"
render preview-runtime-eks -f "$fixtures/preview-foundation.yaml" \
  -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml" -f "$eksiso"
if ! cmp -s "$out/preview-runtime.yaml" "$out/preview-runtime-eks.yaml"; then
  fail "preview-runtime-eks: preview.nodeIsolation.eksAutoMode renders differently from the legacy nodePool/nodeClass"
fi
notes notes-preview-legacy-nodes -f "$fixtures/preview-foundation.yaml"
notes_has notes-preview-legacy-nodes "DEPRECATED: preview.nodeIsolation.nodePool and preview.nodeIsolation.nodeClass still work" yes
notes notes-preview-eks-nodes -f "$fixtures/preview-foundation.yaml" -f "$eksiso"
notes_has notes-preview-eks-nodes "DEPRECATED" no
notes_has notes-preview-eks-nodes "NodeClass patchy-preview and NodePool patchy-preview must" yes
generic=$out/generic-isolation.yaml
printf 'preview:\n    nodeIsolation:\n        nodePool: ""\n        nodeClass: ""\n' >"$generic"
render preview-generic -f "$fixtures/preview-foundation.yaml" -f "$fixtures/intent-controller.yaml" \
  -f "$fixtures/preview-controller.yaml" -f "$generic" \
  --set-json 'preview.nodeIsolation.selector={"patchy.example.com/previews":"true","node.example.com/pool":"previews"}'
podsel='select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-pods") | .spec.validations[] | select(.message | test("select the dedicated"))'
expect preview-generic "$podsel | .expression" "has(variables.pod.nodeSelector) && 'node.example.com/pool' in variables.pod.nodeSelector && variables.pod.nodeSelector['node.example.com/pool'] == \"previews\" && 'patchy.example.com/previews' in variables.pod.nodeSelector && variables.pod.nodeSelector['patchy.example.com/previews'] == \"true\""
expect preview-generic "$podsel | .message" 'preview pods must select the dedicated preview nodes'
expect preview "$podsel | .message" 'preview pods must select the dedicated preview node pool and class'
cm preview-generic preview-controller PATCHY_PREVIEW_NODE_SELECTOR '{"node.example.com/pool":"previews","patchy.example.com/previews":"true"}'
cm preview-generic preview-controller PATCHY_PREVIEW_NODE_POOL null
cm preview-generic preview-controller PATCHY_PREVIEW_NODE_CLASS null
cm preview-generic preview-controller PATCHY_PREVIEW_TAINT_KEY patchy.devthe.net/preview-only
cm preview-generic preview-controller PATCHY_PREVIEW_NODE_ISOLATION null
cm preview-runtime preview-controller PATCHY_PREVIEW_NODE_SELECTOR null
cm preview-runtime preview-controller PATCHY_PREVIEW_NODE_ISOLATION null
# Both: the EKS labels first, then the generic ones, in the one rule.
render preview-generic-eks -f "$fixtures/preview-foundation.yaml" \
  --set-json 'preview.nodeIsolation.selector={"patchy.example.com/previews":"true"}'
expect preview-generic-eks "$podsel | .expression" "has(variables.pod.nodeSelector) && 'karpenter.sh/nodepool' in variables.pod.nodeSelector && variables.pod.nodeSelector['karpenter.sh/nodepool'] == \"patchy-preview\" && 'eks.amazonaws.com/nodeclass' in variables.pod.nodeSelector && variables.pod.nodeSelector['eks.amazonaws.com/nodeclass'] == \"patchy-preview\" && 'patchy.example.com/previews' in variables.pod.nodeSelector && variables.pod.nodeSelector['patchy.example.com/previews'] == \"true\""
render preview-iso-none -f "$fixtures/preview-foundation.yaml" -f "$fixtures/intent-controller.yaml" \
  -f "$fixtures/preview-controller.yaml" -f "$generic" --set preview.nodeIsolation.taintKey= --set preview.nodeIsolation.mode=none
expect preview-iso-none "$podsel | .message" ''
expect preview-iso-none 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-pods") | .spec.validations[] | select(.message | test("tolerate")) | .message' ''
expect preview-iso-none 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-outside-pods") | .spec.validations[0].expression' "!has(variables.pod.tolerations) || variables.pod.tolerations.all(t,
  !(has(t.operator) && t.operator == 'Exists' &&
    (!has(t.key) || t.key == '')))"
cm preview-iso-none preview-controller PATCHY_PREVIEW_NODE_ISOLATION none
cm preview-iso-none preview-controller PATCHY_PREVIEW_TAINT_KEY null
notes notes-preview-iso-none -f "$fixtures/preview-foundation.yaml" -f "$generic" --set preview.nodeIsolation.taintKey= \
  --set preview.nodeIsolation.mode=none
notes_has notes-preview-iso-none "preview Pods share nodes with everything else" yes
notes_has notes-preview-iso-none "hack/preview-isolation-probe/run.sh" no
expect_fail 'preview isolation without nodes' 'or preview.nodeIsolation.selector, are required' \
  -f "$fixtures/preview-foundation.yaml" -f "$generic"
expect_fail 'preview isolation none with a selector' 'preview.nodeIsolation.mode none renders no node selector' \
  -f "$fixtures/preview-foundation.yaml" --set preview.nodeIsolation.mode=none
expect_fail 'preview isolation none with a taint key' 'preview.nodeIsolation.mode none renders no node selector' \
  -f "$fixtures/preview-foundation.yaml" -f "$generic" --set preview.nodeIsolation.mode=none
expect_fail 'preview isolation mode unknown' "'/preview/nodeIsolation/mode'" \
  -f "$fixtures/preview-foundation.yaml" --set preview.nodeIsolation.mode=optional
expect_fail 'preview isolation selector contradicting the NodePool' 'contradicts the NodePool "patchy-preview"' \
  -f "$fixtures/preview-foundation.yaml" --set-json 'preview.nodeIsolation.selector={"karpenter.sh/nodepool":"other"}'
expect_fail 'preview isolation selector key with a space' "invalid propertyName 'bad key'" \
  -f "$fixtures/preview-foundation.yaml" --set-json 'preview.nodeIsolation.selector={"bad key":"x"}'
expect_fail 'preview isolation selector key, schema skipped' 'is not a label key' --skip-schema-validation \
  -f "$fixtures/preview-foundation.yaml" --set-json 'preview.nodeIsolation.selector={"bad key":"x"}'
expect_fail 'preview isolation NodeClass without NodePool' 'preview.nodeIsolation.nodePool (eksAutoMode.nodePool) is required' \
  -f "$fixtures/preview-foundation.yaml" -f "$generic" --set preview.nodeIsolation.eksAutoMode.nodeClass=patchy-preview
expect_fail 'preview nodes without EKS names' 'preview.nodeIsolation.create requires preview.nodeIsolation.eksAutoMode.nodePool' \
  -f "$fixtures/preview-foundation.yaml" -f "$fixtures/auto-mode.yaml" -f "$generic" \
  --set-json 'preview.nodeIsolation.selector={"patchy.example.com/previews":"true"}'
render preview-nodes-generic -f "$fixtures/preview-foundation.yaml" -f "$fixtures/auto-mode.yaml" \
  --set-json 'preview.nodeIsolation.selector={"patchy.example.com/previews":"true"}'
expect preview-nodes-generic 'select(.kind == "NodePool") | .spec.template.metadata.labels."patchy.example.com/previews"' true

# ---- the preview image prefix (preview.imagePathPrefix) ---------------------
# One helper (patchy.previewImagePrefix) feeds the slot admission policy, the
# preview-controller and source-controller's denied path. With the default
# path the admission policies are byte for byte what main rendered before the
# path was configurable, blank lines aside (same_render: helm versions differ
# in those alone): preview-admission.default.yaml is that render (helm 4.2.3),
# and is never regenerated to make this pass; only a deliberate, reviewed
# policy change regenerates it (the preview sign-in phase 0 did: the
# secret-refs policy and the metadata-only exemption). A custom path
# changes only the prefix and its length (preview-admission.custom.yaml;
# regenerate it with the same helm template command and review the diff
# against the default).
golden=$fixtures/golden
pv=$fixtures/preview-foundation.yaml
reg=111122223333.dkr.ecr.us-east-1.amazonaws.com
# The comparison itself, on edits of the default golden, so no helm version
# decides whether it is exercised. Helm 4.3's shape (two blank lines before
# every document but the first) is the same render; a changed rule, a changed
# comment or a blank line inside a folded CEL expression is not.
awk 'NR > 1 && $0 == "---" { print ""; print "" } { print }' \
  "$golden/preview-admission.default.yaml" >"$out/golden-helm43.yaml"
same_render_is 'helm 4.3 blank lines between documents' same "$out/golden-helm43.yaml"
sed 's/substring(61)/substring(60)/' "$golden/preview-admission.default.yaml" >"$out/golden-rule.yaml"
same_render_is 'a changed rule' differs "$out/golden-rule.yaml"
sed 's/^    # for this security decision\.$/    # for this decision./' \
  "$golden/preview-admission.default.yaml" >"$out/golden-comment.yaml"
same_render_is 'a changed comment' differs "$out/golden-comment.yaml"
awk '{ print } !done && /variables\.pod\.containers\.all\(c,$/ { print ""; done = 1 }' \
  "$golden/preview-admission.default.yaml" >"$out/golden-scalar.yaml"
same_render_is 'a blank line inside a block scalar' differs "$out/golden-scalar.yaml"
golden_render preview-vap-default "$golden/preview-admission.default.yaml" -f "$pv" \
  --show-only templates/preview-admission.yaml
golden_render preview-vap-custom "$golden/preview-admission.custom.yaml" -f "$pv" \
  --set preview.imagePathPrefix=acme/runtime.images --show-only templates/preview-admission.yaml
render preview-prefix-custom -f "$pv" -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml" \
  --set preview.imagePathPrefix=acme/runtime.images
cm preview-prefix-custom preview-controller PATCHY_PREVIEW_IMAGE_PREFIX "$reg/acme/runtime.images/"
notes notes-preview-prefix -f "$pv" --set preview.imagePathPrefix=acme/runtime.images
notes_has notes-preview-prefix "--image $reg/acme/runtime.images/<app>" yes
expect_fail 'preview image path prefix empty' "'/preview/imagePathPrefix': '' does not match pattern" \
  -f "$pv" --set preview.imagePathPrefix=
expect_fail 'preview image path prefix trailing slash' "'patchy/previews/' does not match pattern" \
  -f "$pv" --set preview.imagePathPrefix=patchy/previews/
expect_fail 'preview image path prefix leading slash' "'/patchy/previews' does not match pattern" \
  -f "$pv" --set preview.imagePathPrefix=/patchy/previews
expect_fail 'preview image path prefix uppercase' "does not match pattern" \
  -f "$pv" --set preview.imagePathPrefix=Patchy/previews
expect_fail 'preview image path prefix unset' "missing property 'imagePathPrefix'" \
  -f "$pv" --set preview.imagePathPrefix=null
# A render that skips the schema still meets the template's own check.
expect_fail 'preview image path prefix empty, schema skipped' 'preview.imagePathPrefix "" must be' \
  -f "$pv" --set preview.imagePathPrefix= --skip-schema-validation
expect_fail 'preview image path prefix trailing slash, schema skipped' \
  'preview.imagePathPrefix "patchy/previews/" must be' \
  -f "$pv" --set preview.imagePathPrefix=patchy/previews/ --skip-schema-validation

# Disjoint from the agent allowlist: an agent.repositoryImages.registries
# entry that is the preview prefix, contains it or sits under it fails the
# render, compared on segment boundaries after the fold source-controller
# applies (case, an explicit :443, ECR's dual-stack and FIPS endpoint names).
# A runtime image is built from an unreviewed pull request head and must
# never be admissible as an agent sandbox image, nor a toolchain image as a
# preview.
ri=$fixtures/repository-images.yaml
for entry in "$reg/patchy/" "$reg/patchy/previews" "$reg/patchy/previews/" "$reg/patchy/previews/agents/" \
  "111122223333.DKR.ECR.us-east-1.amazonaws.com/Patchy/" "$reg:443/patchy/" \
  "111122223333.dkr-ecr.us-east-1.on.aws/patchy/" "111122223333.dkr.ecr-fips.us-east-1.amazonaws.com/patchy/previews/x"; do
  expect_fail "agent registry $entry overlaps the preview prefix" \
    "agent.repositoryImages.registries entry \"$entry\" overlaps the preview image prefix $reg/patchy/previews/" \
    -f "$pv" -f "$ri" --set "agent.repositoryImages.registries={ghcr.io/example/agent-images/,$entry}"
done
expect_fail 'custom preview prefix under an agent registry' \
  "agent.repositoryImages.registries entry \"$reg/apps/\" overlaps the preview image prefix $reg/apps/previews/" \
  -f "$pv" -f "$ri" --set "agent.repositoryImages.registries={$reg/apps/}" --set preview.imagePathPrefix=apps/previews
# Disjoint entries render: a sibling sharing the prefix's leading
# characters, another region's registry, a disjoint custom pair; and with
# repository images off nothing is judged (the kill switch never fails a
# render).
for entry in "$reg/patchy/previews-agents/" "111122223333.dkr.ecr.us-west-2.amazonaws.com/patchy/" "$reg/patchy/app-envs/"; do
  render "preview-disjoint-$(echo "$entry" | tr -c 'a-z0-9\n' '-')" -f "$pv" -f "$ri" \
    --set "agent.repositoryImages.registries={$entry}"
done
render preview-disjoint-custom -f "$pv" -f "$ri" --set "agent.repositoryImages.registries={$reg/apps/agents/}" \
  --set preview.imagePathPrefix=apps/previews
render preview-overlap-images-off -f "$pv" -f "$ri" --set agent.repositoryImages.enabled=false \
  --set "agent.repositoryImages.registries={$reg/patchy/}"
cm preview-overlap-images-off source-controller PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES null
# source-controller refuses agent images from the preview prefix on its own
# too (runnerimage.Policy.Deny), whatever reaches its registries key.
render preview-ri -f "$pv" -f "$ri"
cm preview-ri source-controller PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES "$reg/patchy/previews/"
cm preview-disjoint-custom source-controller PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES "$reg/apps/previews/"
for c in investigation-controller remediation-controller integration-controller; do
  cm preview-ri "$c" PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES null
done

# ---- EKS Auto Mode toggles: off by default, and off means absent ------------
# clusterDNSCIDR, preview.nodeIsolation.create and edgeIngressClass.create are
# opt-in: the default render has none of their objects or rules, and setting
# every one of their other values without the switch renders the default byte
# for byte, so an install that never sets them never changes.
pf=$fixtures/preview-foundation.yaml
am=$fixtures/auto-mode.yaml
unswitched=$fixtures/auto-mode-unswitched.yaml
dnsnp='select(.kind == "NetworkPolicy") | .spec.egress[] | select(.ports | map(.port) | contains([53])) | .to[] | select(has("ipBlock")) | .ipBlock.cidr'
expect default "$dnsnp" ""
expect default 'select(.kind == "NodeClass" or .kind == "NodePool" or .kind == "IngressClass" or .kind == "IngressClassParams") | .kind' ""
# same BASE OTHER: the two renders are byte-identical.
same() {
  if ! cmp -s "$out/$1.yaml" "$out/$2.yaml"; then
    fail "$2: values under preview.nodeIsolation and edgeIngressClass changed the $1 render without their create switch"
  fi
}
render toggles-off -f "$unswitched"
same default toggles-off
# With previews on too, the live shape of an install whose NodePool and
# NodeClass were applied by hand: the chart must not start rendering its own
# (an ownership conflict on upgrade) because a sub-value such as subnetIDs got
# filled in before create. The edge class must not appear beside alb-preview.
nodesoredge='select(.kind == "NodeClass" or .kind == "NodePool" or ((.kind == "IngressClass" or .kind == "IngressClassParams") and .metadata.name != "alb-preview")) | .kind + "/" + .metadata.name'
render preview-toggles-off -f "$pf" -f "$unswitched"
same preview preview-toggles-off
expect preview "$nodesoredge" ""
render preview-runtime-toggles-off -f "$pf" -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml" \
  -f "$unswitched"
same preview-runtime preview-runtime-toggles-off
expect preview-runtime "$nodesoredge" ""
render toggles-off-ingress --set webhook.host=patchy.example.com --set webhook.ingress.enabled=true \
  --set statusServer.host=status.patchy.example.com --set statusServer.ingress.enabled=true \
  --set edgeIngressClass.loadBalancerName=example-patchy
expect toggles-off-ingress 'select(.kind == "Ingress") | .spec.ingressClassName' "null
null"

# ---- clusterDNSCIDR: node-local DNS beside every kube-system DNS rule --------
# EKS Auto Mode serves DNS at the cluster DNS address on each node, not from
# kube-system pods. The rule lands in exactly the policies that already allow
# DNS to kube-system, UDP and TCP 53 only, and selects no new pod.
render dns --set clusterDNSCIDR=10.100.0.10/32
dnsrule='.spec.egress[] | select(.to // [] | any_c(.ipBlock.cidr == "10.100.0.10/32"))'
kubedns='select(.kind == "NetworkPolicy" and (.spec.egress // [] | any_c(.to // [] | any_c(.namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "kube-system")))) | .metadata.namespace + "/" + .metadata.name'
nodedns='select(.kind == "NetworkPolicy" and (.spec.egress // [] | any_c(.to // [] | any_c(.ipBlock.cidr == "10.100.0.10/32")))) | .metadata.namespace + "/" + .metadata.name'
expect dns "$nodedns" "patchy/patchy-egress-broker
patchy/patchy-integration-controller
patchy/patchy-source-controller
patchy/patchy-context-controller
patchy/patchy-investigation-controller
patchy/patchy-remediation-controller
patchy-agents/patchy-agents-egress
patchy/patchy-status-server"
expect dns "select(.kind == \"NetworkPolicy\") | $dnsrule | .ports | map(.protocol + \"/\" + (.port | tostring)) | join(\",\")" \
  "$(for i in 1 2 3 4 5 6 7 8; do echo UDP/53,TCP/53; done)"
expect dns "select(.kind == \"NetworkPolicy\") | $dnsrule | .to | length" "$(for i in 1 2 3 4 5 6 7 8; do echo 1; done)"
npsel='select(.kind == "NetworkPolicy") | .metadata.namespace + "/" + .metadata.name + " " + (.spec.podSelector | to_json(0)) + " " + (.spec.policyTypes | join(","))'
if [ "$(get default "$npsel")" != "$(get dns "$npsel")" ]; then
  fail "dns: clusterDNSCIDR changed which pods a NetworkPolicy selects"
fi
# Every component that can run, on: the two lists stay equal.
render dns-all -f "$pf" -f "$fixtures/intent-controller.yaml" -f "$fixtures/preview-controller.yaml" \
  -f "$fixtures/evaluation-controller.yaml" --set clusterDNSCIDR=10.100.0.10/32
if [ -z "$(get dns-all "$kubedns")" ] || [ "$(get dns-all "$kubedns")" != "$(get dns-all "$nodedns")" ]; then
  fail "dns-all: policies with kube-system DNS ($(get dns-all "$kubedns" | tr '\n' ' ')) != policies with node-local DNS ($(get dns-all "$nodedns" | tr '\n' ' '))"
fi
expect dns-all "$nodedns | select(test(\"intent-controller|evaluation-controller|preview-controller\"))" "patchy/patchy-evaluation-controller
patchy/patchy-intent-controller
patchy/patchy-preview-controller"
# The preview slot policy's DNS address defaults to it; an explicit
# preview.dnsCIDR wins; with neither, enabling previews fails.
render preview-dns-default -f "$pf" --set preview.dnsCIDR= --set clusterDNSCIDR=10.100.0.10/32
expect preview-dns-default 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.egress[].to[].ipBlock.cidr' '10.100.0.10/32
10.100.0.10/32'
render preview-dns-explicit -f "$pf" --set clusterDNSCIDR=10.100.0.10/32
expect preview-dns-explicit 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.egress[].to[].ipBlock.cidr' '172.20.0.10/32
172.20.0.10/32'
expect_fail 'preview without any DNS address' 'preview.dnsCIDR is required when preview.enabled=true' -f "$pf" --set preview.dnsCIDR=
expect_fail 'clusterDNSCIDR wider than one address' "'/clusterDNSCIDR'" --set clusterDNSCIDR=172.20.0.0/16

# ---- preview.nodeIsolation.create: the DefaultDeny NodeClass and NodePool -----
render am -f "$pf" -f "$am"
nc='select(.kind == "NodeClass")'
np='select(.kind == "NodePool")'
expect am "$nc | .apiVersion + \" \" + .metadata.name" "eks.amazonaws.com/v1 patchy-preview"
expect am "$nc | .spec.networkPolicy + \" \" + .spec.networkPolicyEventLogs" "DefaultDeny Disabled"
expect am "$nc | .spec.role" example-preview-node
expect am "$nc | .spec.subnetSelectorTerms[].id" "subnet-0123456789abcdef0
subnet-0fedcba9876543210"
expect am "$nc | .spec.securityGroupSelectorTerms[].id" sg-0123456789abcdef0
expect am "$nc | .spec.ephemeralStorage | .size + \" \" + (.iops | tostring) + \" \" + (.throughput | tostring)" "20Gi 3000 125"
expect am "$np | .apiVersion + \" \" + .metadata.name" "karpenter.sh/v1 patchy-preview"
expect am "$np | .spec.template.spec.nodeClassRef | .group + \"/\" + .kind + \"/\" + .name" eks.amazonaws.com/NodeClass/patchy-preview
expect am "$np | .spec.template.spec.taints[] | .key + \"=\" + .value + \":\" + .effect" "patchy.devthe.net/preview-only=true:NoExecute"
expect am "$np | .spec.template.spec.requirements[] | .key + \" \" + .operator + \" \" + (.values | join(\",\"))" \
  "node.kubernetes.io/instance-type In t3a.medium
kubernetes.io/arch In amd64
kubernetes.io/os In linux
karpenter.sh/capacity-type In on-demand"
expect am "$np | .spec.disruption | .consolidationPolicy + \" \" + .consolidateAfter + \" \" + .budgets[0].nodes" "WhenEmpty 30s 1"
expect am "$np | .spec.limits | .cpu + \"/\" + .nodes" "4/2"
for kind in NodeClass NodePool IngressClass IngressClassParams; do
  expect am "select(.kind == \"$kind\") | .metadata.annotations.\"helm.sh/resource-policy\" | select(. != \"keep\")" ""
done
render am-tuned -f "$pf" -f "$am" --set-json 'preview.nodeIsolation.instanceTypes=["m7g.large","c7g.large"]' \
  --set preview.nodeIsolation.arch=arm64 --set preview.nodeIsolation.cpuLimit=8 --set preview.nodeIsolation.nodeLimit=3 \
  --set preview.nodeIsolation.ephemeralStorage.size=40Gi
expect am-tuned "$np | .spec.template.spec.requirements[0:2][] | .values | join(\",\")" "m7g.large,c7g.large
arm64"
expect am-tuned "$np | .spec.limits | .cpu + \"/\" + .nodes" "8/3"
expect am-tuned "$nc | .spec.ephemeralStorage.size" 40Gi
expect_fail 'nodes without previews' 'preview.nodeIsolation.create requires preview.enabled' -f "$am"
expect_fail 'nodes without a role' 'preview.nodeIsolation.create requires preview.nodeIsolation.role' \
  -f "$pf" -f "$am" --set preview.nodeIsolation.role=
expect_fail 'nodes without subnets' 'preview.nodeIsolation.create requires preview.nodeIsolation.subnetIDs' \
  -f "$pf" -f "$am" --set-json 'preview.nodeIsolation.subnetIDs=[]'
expect_fail 'nodes without security groups' 'preview.nodeIsolation.create requires preview.nodeIsolation.securityGroupIDs' \
  -f "$pf" -f "$am" --set-json 'preview.nodeIsolation.securityGroupIDs=[]'
expect_fail 'node role as an ARN' "'/preview/nodeIsolation/role'" \
  -f "$pf" -f "$am" --set preview.nodeIsolation.role=arn:aws:iam::123456789012:role/preview
expect_fail 'node subnet as a CIDR' "'/preview/nodeIsolation/subnetIDs/0'" \
  -f "$pf" -f "$am" --set-json 'preview.nodeIsolation.subnetIDs=["10.0.0.0/24"]'
# AWS IDs are 8 or 17 hex digits, as preview.albSubnetIDs already holds them.
expect_fail 'node subnet of neither ID length' "'/preview/nodeIsolation/subnetIDs/0'" \
  -f "$pf" -f "$am" --set-json 'preview.nodeIsolation.subnetIDs=["subnet-0123456789ab"]'
expect_fail 'node security group of neither ID length' "'/preview/nodeIsolation/securityGroupIDs/0'" \
  -f "$pf" -f "$am" --set-json 'preview.nodeIsolation.securityGroupIDs=["sg-0123456789ab"]'
# DefaultDeny is fixed: there is no value to turn it into DefaultAllow.
expect_fail 'node class network policy as a value' "additional properties 'networkPolicy' not allowed" \
  -f "$pf" -f "$am" --set preview.nodeIsolation.networkPolicy=DefaultAllow

# ---- edgeIngressClass.create: the edge class the Ingresses fall back to -------
ec='select(.kind == "IngressClass" and .metadata.name == "patchy-edge")'
ep='select(.kind == "IngressClassParams" and .metadata.name == "patchy-edge")'
expect am "$ec | .spec.controller" eks.amazonaws.com/alb
expect am "$ec | .spec.parameters | .apiGroup + \"/\" + .kind + \"/\" + .name" eks.amazonaws.com/IngressClassParams/patchy-edge
expect am "$ec | .metadata.annotations.\"ingressclass.kubernetes.io/is-default-class\"" false
expect am "$ep | .spec | .scheme + \" \" + .loadBalancerName + \" \" + .group.name" "internet-facing example-patchy example-patchy"
expect am "$ep | .spec.certificateARNs | join(\",\")" \
  arn:aws:acm:us-east-1:123456789012:certificate/00000000-0000-0000-0000-000000000000
expect am "$ep | .spec.namespaceSelector.matchLabels.\"kubernetes.io/metadata.name\"" patchy
expect am 'select(.kind == "Ingress" and .metadata.namespace == "patchy") | .metadata.name + " " + .spec.ingressClassName' \
  "patchy-webhook patchy-edge
patchy-status-server patchy-edge"
# An explicit className wins; the name, scheme and certificates are values;
# the namespace pin follows the release namespace.
render am-edge-tuned -f "$am" --set preview.nodeIsolation.create=false --namespace platform --set webhook.ingress.className=nginx \
  --set edgeIngressClass.name=public-edge --set edgeIngressClass.scheme=internal --set-json 'edgeIngressClass.certificateARNs=[]'
expect am-edge-tuned 'select(.kind == "Ingress") | .metadata.name + " " + .spec.ingressClassName' \
  "patchy-webhook nginx
patchy-status-server public-edge"
expect am-edge-tuned 'select(.kind == "IngressClassParams") | .metadata.name + " " + .spec.scheme + " " + (.spec | has("certificateARNs") | tostring)' \
  "public-edge internal false"
expect am-edge-tuned 'select(.kind == "IngressClassParams") | .spec.namespaceSelector.matchLabels."kubernetes.io/metadata.name"' platform
expect_fail 'edge without a load balancer name' 'edgeIngressClass.create requires edgeIngressClass.loadBalancerName' \
  --set edgeIngressClass.create=true
expect_fail 'edge on the preview ALB' 'edgeIngressClass.loadBalancerName must differ from preview.albName' \
  -f "$pf" -f "$am" --set edgeIngressClass.loadBalancerName=devthenet-dev-preview
expect_fail 'edge named like the preview class' 'edgeIngressClass.name must not be alb-preview' \
  -f "$am" --set preview.nodeIsolation.create=false --set edgeIngressClass.name=alb-preview
# The name is the ALB group's name too, and a group name is lowercase.
expect_fail 'edge load balancer name with capitals' "'/edgeIngressClass/loadBalancerName'" \
  --set edgeIngressClass.create=true --set edgeIngressClass.loadBalancerName=Example-Patchy
expect_fail 'edge load balancer name over 32 characters' "'/edgeIngressClass/loadBalancerName'" \
  --set edgeIngressClass.create=true --set edgeIngressClass.loadBalancerName=abcdefghijklmnopqrstuvwxyz0123456

# ---- the install NOTES walk previews through what is left -----------------
notes notes-preview -f "$pf"
notes_has notes-preview "kubectl get ingress patchy-preview-placeholder -n patchy-preview-0" yes
notes_has notes-preview "point *.preview.patchy.devthe.net at that hostname" yes
notes_has notes-preview "NodeClass patchy-preview and NodePool patchy-preview must" yes
notes_has notes-preview "bash hack/preview-isolation-probe/run.sh" yes
notes_has notes-preview "--taint-key patchy.devthe.net/preview-only" yes
notes_has notes-preview "COST: the preview ALB bills from the moment the placeholder creates it" yes
notes_has notes-preview "(even with previewController off)" yes
# The NodePool is the operator's, so its disruption policy and limits are
# theirs: no promise of the chart's 30s consolidation or a cap.
notes_has notes-preview "A preview node bills from launch until NodePool patchy-preview's own" yes
notes_has notes-preview "30s after" no
notes_has notes-preview "CPUs" no
notes notes-preview-nodes -f "$pf" -f "$am"
notes_has notes-preview-nodes "this release renders NodeClass patchy-preview" yes
notes_has notes-preview-nodes "A preview node (t3a.medium) bills from launch until it" yes
notes_has notes-preview-nodes "is consolidated, 30s after its last preview Pod. Budget by CPU: the pool" yes
notes_has notes-preview-nodes "stops launching nodes at 4 CPUs." yes
# CPU is the bound; the node limit is not promised (Auto Mode may not
# enforce limits.nodes).
notes_has notes-preview-nodes "so do not count on its limit of 2 nodes." yes
notes_has notes-preview-nodes "and 2 nodes" no
notes_has notes-preview-nodes "disruption policy removes it" no
notes notes-preview-guardrails -f "$pf" --set preview.placeholder.enabled=false
notes_has notes-preview-guardrails "preview.placeholder.enabled is false" yes
notes_has notes-preview-guardrails "kubectl get ingress patchy-preview-placeholder" no
notes notes-preview-off
notes_has notes-preview-off "Previews:" no
notes_has notes-preview-off "COST:" no

# ---- preview sign-in (previewAuth): off is absent, permit then require -------
# Off (the default) nothing of it renders: the admission goldens above, the
# whole default render and the preview renders carry none of its objects or
# keys. The keys Secret is generated per render here (no cluster to look it up
# in, so the fixture sets keys.renderOffline), so no assertion reads a key value; the envtest suite
# (preview_auth_upgrade_envtest_test.go) upgrades a real release through both
# stages and a rotation, and checks the client secrets against the relay's
# own derivation.
pav=$fixtures/preview-auth.yaml
pc=$fixtures/preview-controller.yaml
ic=$fixtures/intent-controller.yaml
pafull="-f $pf -f $ic -f $pc -f $pav"
for r in default preview preview-runtime; do
  expect "$r" 'select((.metadata.name | test("preview-auth|preview-viewer|preview-oidc")) or .kind == "Lease" or .kind == "PodDisruptionBudget") | .kind + "/" + .metadata.name' ""
  expect "$r" 'select(.kind == "ConfigMap") | .data | keys | .[] | select(test("^PATCHY_PREVIEW_AUTH_"))' ""
  expect "$r" 'select(.kind == "ValidatingAdmissionPolicy") | .metadata.annotations."patchy.bitwisemedia.uk/preview-auth-admits" | select(. != null)' ""
  expect "$r" 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .metadata.annotations | keys | .[] | select(test("auth-"))' ""
done
# shellcheck disable=SC2086 # $pafull is a list of helm arguments.
render pa-permit $pafull
# shellcheck disable=SC2086
render pa-require $pafull --set previewAuth.stage=require --set previewAuth.permitConfirmedGeneration=1
golden_render pa-vap-permit "$golden/preview-admission.auth-permit.yaml" -f "$pf" -f "$pav" \
  --show-only templates/preview-admission.yaml
# shellcheck disable=SC2086
golden_render pa-vap-require "$golden/preview-admission.auth-require.yaml" $pafull \
  --set previewAuth.stage=require --set previewAuth.permitConfirmedGeneration=1 --show-only templates/preview-admission.yaml
# The relay: two replicas spread over nodes and zones behind a PDB, its own
# component label (never component=server, which the kustomize status
# NetworkPolicy selects), its keys and Dex secret mounted, no Secret RBAC.
padep='select(.kind == "Deployment" and .metadata.name == "patchy-preview-auth")'
expect pa-permit "$padep | .spec.replicas" 2
expect pa-permit "$padep | .spec.template.metadata.labels.\"app.kubernetes.io/component\"" preview-auth
expect pa-permit "$padep | .spec.template.spec.containers[0].image | split(\":\") | .[0]" ghcr.io/devthenet-labs/patchy/preview-auth
expect pa-permit "$padep | .spec.template.spec.topologySpreadConstraints[].topologyKey" 'topology.kubernetes.io/zone
kubernetes.io/hostname'
expect pa-permit "$padep | .spec.template.spec.volumes[] | .name + \" \" + (.secret.secretName // \"-\")" 'keys patchy-preview-auth-keys
dex patchy-preview-auth-dex
tmp -'
expect pa-permit "$padep | .spec.template.spec.volumes[] | select(.name == \"dex\") | .secret.items[0].key + \" \" + .secret.items[0].path" 'clientSecret clientSecret'
expect pa-permit "$padep | .spec.template.metadata.annotations | has(\"checksum/keys\")" true
expect pa-permit 'select(.kind == "PodDisruptionBudget") | .metadata.name + " " + (.spec.minAvailable | tostring)' 'patchy-preview-auth 1'
render pa-one -f "$pf" -f "$pav" --set previewAuth.replicas=1
expect pa-one 'select(.kind == "PodDisruptionBudget") | .metadata.name' ""
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_ISSUER https://preview-auth.patchy.devthe.net
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_HOST_SUFFIX preview.patchy.devthe.net
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_SLOT_COUNT 2
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_LEDGER_LEASE patchy-preview-auth-codes
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_FORWARDED_HOPS 1
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_DEX_ISSUER_URL https://dex.patchy.devthe.net
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_DEX_CLIENT_ID patchy-preview-auth
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_DEX_CLIENT_SECRET_FILE /etc/patchy/preview-auth/dex/clientSecret
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_KEYS_DIR /etc/patchy/preview-auth/keys
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_USERNAME_PREFIX github:
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_GROUPS_PREFIX github:
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_RATE_PER_SECOND 5
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_RATE_BURST 30
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_ACCESS_TOKEN_TTL 10m
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_SESSION_MAX_AGE 12h
cm pa-permit preview-auth PATCHY_PREVIEW_AUTH_PROBE_INTERVAL 1m
# Every key the relay's ConfigMap carries is one of its flags.
for key in $(get pa-permit 'select(.kind == "ConfigMap" and .metadata.name == "patchy-preview-auth-config") | .data | keys | .[]'); do
  flag=$(echo "$key" | sed 's/^PATCHY_//' | tr 'A-Z_' 'a-z-')
  case "$flag" in
  listen-addr | log-level) continue ;;
  esac
  if ! grep -q "\"$flag\"" cmd/preview-auth/serve.go; then
    fail "pa-permit: ConfigMap key $key is no preview-auth flag ($flag)"
  fi
done
expect pa-permit 'select(.kind == "ConfigMap" and .metadata.name == "patchy-preview-auth-config") | .metadata.annotations."patchy.bitwisemedia.uk/preview-auth-key-generation"' 1
# The keys Secret: generated, kept, generation 1, never a previous one yet.
pakeys='select(.kind == "Secret" and .metadata.name == "patchy-preview-auth-keys")'
expect pa-permit "$pakeys | .data | keys | join(\",\")" 'generation,master,signingKey'
expect pa-permit "$pakeys | .data.generation | @base64d" 1
expect pa-permit "$pakeys | .data.master | @base64d | length" 64
expect pa-permit "$pakeys | .metadata.annotations.\"helm.sh/resource-policy\"" keep
# The relay's grants: Previews read, its one Lease, access reviews. No
# Secret, no Intent, no write to a patchy kind, nothing in a slot.
expect pa-permit 'select(.kind == "Role" and .metadata.name == "patchy-preview-auth") | .rules[] | (.resources | join(",")) + " " + (.verbs | join(",")) + " " + ((.resourceNames // []) | join(","))' \
  'previews get,list,watch 
leases get,update patchy-preview-auth-codes'
expect pa-permit 'select(.kind == "ClusterRole" and .metadata.name == "patchy-preview-auth-authz") | .rules[] | (.resources | join(",")) + " " + (.verbs | join(","))' \
  'subjectaccessreviews create'
expect pa-permit 'select((.kind == "RoleBinding" or .kind == "ClusterRoleBinding") and .subjects[].name == "patchy-preview-auth") | .kind + "/" + .metadata.name + " " + (.metadata.namespace // "-")' \
  'ClusterRoleBinding/patchy-preview-auth-authz -
RoleBinding/patchy-preview-auth patchy'
expect pa-permit 'select(.kind == "Lease") | .metadata.name + " " + .metadata.namespace' 'patchy-preview-auth-codes patchy'
# The preview-viewer role and the viewers, prefixed as the relay maps claims.
expect pa-permit 'select(.kind == "ClusterRole" and .metadata.name == "patchy-preview-viewer") | .rules[] | (.resources | join(",")) + " " + (.verbs | join(","))' \
  'projects/previews get'
expect pa-permit 'select(.kind == "RoleBinding" and .metadata.name == "patchy-preview-viewers") | .metadata.namespace + " " + .roleRef.kind + "/" + .roleRef.name' \
  'patchy ClusterRole/patchy-preview-viewer'
expect pa-permit 'select(.kind == "RoleBinding" and .metadata.name == "patchy-preview-viewers") | .subjects[] | .kind + " " + .name' \
  'Group github:devthenet-labs:reviewers
User github:octocat'
render pa-noviewers -f "$pf" -f "$pav" --set-json 'previewAuth.viewers={"teams":[],"users":[]}'
expect pa-noviewers 'select(.kind == "RoleBinding" and .metadata.name == "patchy-preview-viewers") | .kind' ""
expect pa-noviewers 'select(.kind == "ClusterRole" and .metadata.name == "patchy-preview-viewer") | .kind' ClusterRole
# Each slot's ALB client Secret (generation in the name), read by Auto Mode
# alone, by name: get, exact resourceNames, Group eks:managed. All kept.
expect pa-permit 'select(.kind == "Secret" and .metadata.name == "patchy-preview-oidc-g1") | .metadata.namespace + " " + .stringData.clientID' \
  'patchy-preview-0 patchy-preview-s0
patchy-preview-1 patchy-preview-s1'
expect pa-permit 'select(.kind == "Secret" and .metadata.name == "patchy-preview-oidc-g1") | .stringData.clientSecret | test("^[0-9a-f]{64}$")' 'true
true'
expect pa-permit 'select(.kind == "Role" and .metadata.name == "patchy-preview-oidc-reader") | .metadata.namespace + " " + (.rules | to_json(0))' \
  'patchy-preview-0 [{"apiGroups":[""],"resources":["secrets"],"resourceNames":["patchy-preview-oidc-g1"],"verbs":["get"]}]
patchy-preview-1 [{"apiGroups":[""],"resources":["secrets"],"resourceNames":["patchy-preview-oidc-g1"],"verbs":["get"]}]'
expect pa-permit 'select(.kind == "RoleBinding" and .metadata.name == "patchy-preview-oidc-reader") | .subjects | to_json(0)' \
  '[{"kind":"Group","apiGroup":"rbac.authorization.k8s.io","name":"eks:managed"}]
[{"kind":"Group","apiGroup":"rbac.authorization.k8s.io","name":"eks:managed"}]'
expect pa-permit 'select((.metadata.name | test("preview-oidc")) and (.metadata.annotations."helm.sh/resource-policy" != "keep")) | .metadata.name' ""
# The preview-controller still reads no Secret.
expect pa-require 'select(.kind == "Role" and .metadata.name == "patchy-preview-controller") | .rules[].resources[] | select(. == "secrets")' ""
# Permit (B1): the slot Ingress policy admits generation 1 and records it;
# nothing requires sign-in yet: no required policy, the controller and the
# placeholder exactly as before.
expect pa-permit 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-ingresses") | .metadata.annotations."patchy.bitwisemedia.uk/preview-auth-admits"' g1
expect pa-permit 'select(.metadata.name | test("ingress-auth")) | .kind' ""
render pa-permit-runtime -f "$pf" -f "$ic" -f "$pc" -f "$pav"
for q in 'select(.kind == "ConfigMap" and .metadata.name == "patchy-preview-controller-config")' \
  'select(.kind == "Deployment" and .metadata.name == "patchy-preview-controller")' \
  'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder")' \
  'select(.kind == "IngressClassParams")'; do
  if [ "$(get preview-runtime "$q")" != "$(get pa-permit-runtime "$q")" ]; then
    fail "pa-permit-runtime: the permit stage changed $q"
  fi
done
# Require (B2): the controller and the placeholder carry generation 1's sets,
# the very JSON both policies compare, and the kept required policy is live.
pcm='select(.kind == "ConfigMap" and .metadata.name == "patchy-preview-controller-config")'
cm pa-require preview-controller PATCHY_PREVIEW_AUTH_REQUIRED true
cm pa-require preview-controller PATCHY_PREVIEW_AUTH_PREVIOUS_ANNOTATIONS null
expect pa-require "$pcm | .data.PATCHY_PREVIEW_AUTH_ANNOTATIONS | from_json | keys | join(\",\")" 'patchy-preview-0,patchy-preview-1'
expect pa-require "$pcm | .data.PATCHY_PREVIEW_AUTH_ANNOTATIONS | from_json | .\"patchy-preview-1\" | .\"alb.ingress.kubernetes.io/auth-session-cookie\"" patchy-preview-s1
expect pa-require "$pcm | .data.PATCHY_PREVIEW_AUTH_ANNOTATIONS | from_json | .\"patchy-preview-0\" | .\"alb.ingress.kubernetes.io/auth-idp-oidc\" | from_json | .secretName" patchy-preview-oidc-g1
placeholder_auth=$(get pa-require 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .metadata.annotations | with_entries(select(.key | test("auth-"))) | to_json(0)')
cm_slot0=$(get pa-require "$pcm | .data.PATCHY_PREVIEW_AUTH_ANNOTATIONS | from_json | .\"patchy-preview-0\" | to_json(0)")
if [ -z "$cm_slot0" ] || [ "$placeholder_auth" != "$cm_slot0" ]; then
  fail "pa-require: the placeholder's sign-in annotations ($placeholder_auth) are not slot 0's set ($cm_slot0)"
fi
expect pa-require 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-ingress-auth") | .metadata.annotations."helm.sh/resource-policy" + " " + .spec.failurePolicy' 'keep Fail'
expect pa-require 'select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == "patchy-preview-all-slots-ingress-auth") | .spec.policyName + " " + (.spec.validationActions | join(",")) + " " + .metadata.annotations."helm.sh/resource-policy"' \
  'patchy-preview-ingress-auth Deny keep'
# Switching to require rolls the preview-controller (its ConfigMap changed).
pccsum='select(.kind == "Deployment" and .metadata.name == "patchy-preview-controller") | .spec.template.metadata.annotations["checksum/config"]'
if [ "$(get pa-permit-runtime "$pccsum")" = "$(get pa-require "$pccsum")" ]; then
  fail "pa-require: the preview-controller's checksum/config did not change"
fi
# The order Helm applies a revision in (helm template prints it): Secrets,
# RBAC and the controller before the placeholder Ingress, and every
# admission policy after it. So the require stage's placeholder and
# controller writes meet the PREVIOUS revision's policies, which is why the
# require stage refuses a generation the live policy does not admit yet.
order=$(get pa-require '.kind + "/" + .metadata.name' | grep -n -E '^(Secret/patchy-preview-oidc-g1|Role/patchy-preview-oidc-reader|Deployment/patchy-preview-controller|Ingress/patchy-preview-placeholder|ValidatingAdmissionPolicy/patchy-preview-ingresses|ValidatingAdmissionPolicy/patchy-preview-ingress-auth)$' |
  awk -F: '!seen[$2]++ { print $2 }' | tr '\n' ' ')
if [ "$order" != "Secret/patchy-preview-oidc-g1 Role/patchy-preview-oidc-reader Deployment/patchy-preview-controller Ingress/patchy-preview-placeholder ValidatingAdmissionPolicy/patchy-preview-ingresses ValidatingAdmissionPolicy/patchy-preview-ingress-auth " ]; then
  fail "pa-require: manifest order is '$order'"
fi
# The relay's Ingress: the edge class (falls back like the others), the host.
render pa-edge -f "$pf" -f "$am" -f "$pav"
expect pa-edge 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-auth") | .spec.ingressClassName + " " + .spec.rules[0].host + " " + .spec.rules[0].http.paths[0].backend.service.name' \
  'patchy-edge preview-auth.patchy.devthe.net patchy-preview-auth'
expect pa-permit 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-auth") | .spec | has("ingressClassName")' false
# The relay's ALB settings are explicit, not the IngressClass's: pod IP
# targets and a health check on / (the relay answers it 200 without
# credentials), plus internet facing (the preview ALB's token calls come from
# the internet) off the chart's edge class; previewAuth.ingress.annotations
# overrides each one.
paing='select(.kind == "Ingress" and .metadata.name == "patchy-preview-auth") | .metadata.annotations'
expect pa-permit "$paing | to_entries | map(select(.key | test(\"^alb\\.ingress\\.kubernetes\\.io/\")) | .key + \"=\" + .value) | .[]" \
  'alb.ingress.kubernetes.io/healthcheck-path=/
alb.ingress.kubernetes.io/scheme=internet-facing
alb.ingress.kubernetes.io/target-type=ip'
render pa-ingress-annotations -f "$pf" -f "$pav" \
  --set-json 'previewAuth.ingress.annotations={"alb.ingress.kubernetes.io/healthcheck-path":"/.well-known/openid-configuration","alb.ingress.kubernetes.io/ssl-redirect":"443"}'
expect pa-ingress-annotations "$paing | .\"alb.ingress.kubernetes.io/healthcheck-path\" + \" \" + .\"alb.ingress.kubernetes.io/ssl-redirect\" + \" \" + .\"alb.ingress.kubernetes.io/scheme\"" \
  '/.well-known/openid-configuration 443 internet-facing'
# On the chart's edge class the class's scheme governs the one shared ALB, so
# the relay declares none: it follows edgeIngressClass.scheme like the
# webhook and status-page Ingresses instead of contradicting their group.
expect pa-edge "$paing | to_entries | map(select(.key | test(\"^alb\\.ingress\\.kubernetes\\.io/\")) | .key + \"=\" + .value) | .[]" \
  'alb.ingress.kubernetes.io/healthcheck-path=/
alb.ingress.kubernetes.io/target-type=ip'
render pa-edge-internal -f "$pf" -f "$am" -f "$pav" --set edgeIngressClass.scheme=internal
expect pa-edge-internal "$paing | has(\"alb.ingress.kubernetes.io/scheme\")" false
expect pa-edge-internal 'select(.kind == "IngressClassParams" and .metadata.name == "patchy-edge") | .spec.scheme' internal
# NetworkPolicy: 8080 from anywhere (or ingressFrom) and probes; DNS, 443 and
# 6443 out.
panp='select(.kind == "NetworkPolicy" and .metadata.name == "patchy-preview-auth")'
expect pa-permit "$panp | .spec.ingress[] | (.ports[0].port | tostring) + \" \" + (has(\"from\") | tostring)" '8080 false
8081 false'
expect pa-permit "$panp | .spec.egress[-1].ports | map(.port) | join(\",\")" '443,6443'
render pa-from -f "$pf" -f "$pav" --set 'previewAuth.ingressFrom={10.40.0.0/24}'
expect pa-from "$panp | .spec.ingress[0].from[0].ipBlock.cidr" 10.40.0.0/24
# Chart-managed keys need a cluster to keep them in: a render without one
# (helm template, GitOps) is refused unless it says its keys are throwaway,
# and an operator-owned keys Secret needs no lookup at all.
expect_fail 'managed keys without a cluster' 'previewAuth with chart-managed keys (no previewAuth.keys.existingSecret) needs a render that reaches the cluster' \
  -f "$pf" -f "$pav" --set previewAuth.keys.renderOffline=false
# shellcheck disable=SC2086
expect_fail 'managed keys without a cluster, require stage' 'set previewAuth.keys.existingSecret and supply the slot Secrets' \
  $pafull --set previewAuth.keys.renderOffline=false --set previewAuth.stage=require --set previewAuth.permitConfirmedGeneration=1
render pa-existing-offline -f "$pf" -f "$pav" --set previewAuth.keys.renderOffline=false \
  --set previewAuth.keys.existingSecret=my-keys
expect pa-existing-offline "$padep | .spec.template.spec.volumes[0].secret.secretName" my-keys
# An operator-owned keys Secret: the chart renders neither it nor the slot
# client Secrets, and names the generations it is told.
render pa-existing -f "$pf" -f "$pav" --set previewAuth.keys.existingSecret=my-keys \
  --set previewAuth.keys.generation=3 --set previewAuth.keys.previousGeneration=2
expect pa-existing 'select(.kind == "Secret") | .metadata.name' ""
expect pa-existing "$padep | .spec.template.spec.volumes[0].secret.secretName" my-keys
expect pa-existing "$padep | .spec.template.metadata.annotations | has(\"checksum/keys\")" false
expect pa-existing 'select(.kind == "Role" and .metadata.name == "patchy-preview-oidc-reader") | .rules[0].resourceNames | join(",")' \
  'patchy-preview-oidc-g3,patchy-preview-oidc-g2
patchy-preview-oidc-g3,patchy-preview-oidc-g2'
expect pa-existing 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-preview-ingresses") | .metadata.annotations."patchy.bitwisemedia.uk/preview-auth-admits"' g2,g3
expect pa-existing 'select(.kind == "ConfigMap" and .metadata.name == "patchy-preview-auth-config") | .metadata.annotations."patchy.bitwisemedia.uk/preview-auth-key-generation"' null
# shellcheck disable=SC2086
render pa-existing-require $pafull --set previewAuth.keys.existingSecret=my-keys --set previewAuth.keys.generation=3 \
  --set previewAuth.keys.previousGeneration=2 --set previewAuth.stage=require --set previewAuth.permitConfirmedGeneration=2
# A rotation's first upgrade: generation 3 admitted, 2 still applied.
expect pa-existing-require "$pcm | .data.PATCHY_PREVIEW_AUTH_ANNOTATIONS | from_json | .\"patchy-preview-0\" | .\"alb.ingress.kubernetes.io/auth-idp-oidc\" | from_json | .secretName" patchy-preview-oidc-g2
expect pa-existing-require "$pcm | .data.PATCHY_PREVIEW_AUTH_PREVIOUS_ANNOTATIONS | from_json | .\"patchy-preview-0\" | .\"alb.ingress.kubernetes.io/auth-idp-oidc\" | from_json | .secretName" patchy-preview-oidc-g3
# shellcheck disable=SC2086
notes notes-pa-rotating $pafull --set previewAuth.keys.existingSecret=my-keys --set previewAuth.keys.generation=3 \
  --set previewAuth.keys.previousGeneration=2 --set previewAuth.stage=require --set previewAuth.permitConfirmedGeneration=2
notes_has notes-pa-rotating "ROTATION IN PROGRESS" yes
# The install NOTES print the one IdP setup: Dex's redirect URI.
notes notes-pa-permit -f "$pf" -f "$pav"
notes_has notes-pa-permit "https://preview-auth.patchy.devthe.net/dex/callback" yes
notes_has notes-pa-permit "upgrade again with previewAuth.stage=require" yes
notes_has notes-pa-permit "NO VIEWERS" no
# shellcheck disable=SC2086
notes notes-pa-require $pafull --set previewAuth.stage=require --set previewAuth.permitConfirmedGeneration=1
notes_has notes-pa-require "Every preview Ingress requires sign-in" yes
notes notes-pa-noviewers -f "$pf" -f "$pav" --set-json 'previewAuth.viewers={"teams":[],"users":[]}'
notes_has notes-pa-noviewers "NO VIEWERS" yes
notes_has notes-preview-off "Preview sign-in" no
# Guards.
expect_fail 'sign-in without preview slots' 'previewAuth.enabled requires preview.enabled' -f "$pav"
expect_fail 'sign-in without a host' 'previewAuth.host is required' -f "$pf" -f "$pav" --set previewAuth.host=
expect_fail 'sign-in host under the preview suffix' 'must not be under preview.hostSuffix' \
  -f "$pf" -f "$pav" --set previewAuth.host=auth.preview.patchy.devthe.net
expect_fail 'sign-in without a Dex secret' 'previewAuth.dex.existingSecret is required' \
  -f "$pf" -f "$pav" --set previewAuth.dex.existingSecret=
expect_fail 'sign-in without a Dex issuer' 'previewAuth.dex.issuerURL is required' \
  -f "$pf" -f "$pav" --set previewAuth.dex.issuerURL=
expect_fail 'sign-in with an http Dex issuer' "'/previewAuth/dex/issuerURL'" \
  -f "$pf" -f "$pav" --set previewAuth.dex.issuerURL=http://dex.example.com
expect_fail 'sign-in without a username prefix' "'/previewAuth/claims/usernamePrefix'" \
  -f "$pf" -f "$pav" --set previewAuth.claims.usernamePrefix=
expect_fail 'sign-in relay on the preview class' 'must not be alb-preview' \
  -f "$pf" -f "$pav" --set previewAuth.ingress.className=alb-preview
expect_fail 'require without the preview-controller' 'previewAuth.stage require needs previewController.enabled' \
  -f "$pf" -f "$pav" --set previewAuth.stage=require --set previewAuth.permitConfirmedGeneration=1
# shellcheck disable=SC2086
expect_fail 'require before the permit stage is live' 'does not admit key generation 1 yet' $pafull --set previewAuth.stage=require
# shellcheck disable=SC2086
expect_fail 'require confirming another generation' 'does not admit key generation 1 yet' $pafull \
  --set previewAuth.stage=require --set previewAuth.permitConfirmedGeneration=2
expect_fail 'an unknown stage' "'/previewAuth/stage'" -f "$pf" -f "$pav" --set previewAuth.stage=enforce
expect_fail 'a viewer login with a space' "'/previewAuth/viewers/users/0'" -f "$pf" -f "$pav" \
  --set-json 'previewAuth.viewers.users=["octo cat"]'
expect_fail 'a viewer team without its org' "'/previewAuth/viewers/teams/0'" -f "$pf" -f "$pav" \
  --set-json 'previewAuth.viewers.teams=["reviewers"]'
expect_fail 'an existing keys Secret with equal generations' 'previousGeneration must differ' -f "$pf" -f "$pav" \
  --set previewAuth.keys.existingSecret=my-keys --set previewAuth.keys.generation=2 --set previewAuth.keys.previousGeneration=2
# Rev C: the allowlist goes only once sign-in is required; with no prefix
# list either, only on the explicit confirmation and a short ALB session.
printf 'preview:\n    inboundCIDRs: []\n' >"$out/no-allowlist.yaml"
expect_fail 'no allowlist in the permit stage' 'unless preview sign-in is required' \
  -f "$pf" -f "$pav" -f "$out/no-allowlist.yaml"
# shellcheck disable=SC2086
expect_fail 'no allowlist, not confirmed' 'set preview.allowPublicWithAuth=confirmed' $pafull -f "$out/no-allowlist.yaml" \
  --set previewAuth.stage=require --set previewAuth.permitConfirmedGeneration=1
# shellcheck disable=SC2086
expect_fail 'no allowlist, a long ALB session' 'previewAuth.sessionTimeout of at most 900 (it is 3600)' $pafull \
  -f "$out/no-allowlist.yaml" --set previewAuth.stage=require --set previewAuth.permitConfirmedGeneration=1 \
  --set preview.allowPublicWithAuth=confirmed
expect_fail 'an unknown confirmation' "'/preview/allowPublicWithAuth'" -f "$pf" --set preview.allowPublicWithAuth=yes
# shellcheck disable=SC2086
render pa-public $pafull -f "$out/no-allowlist.yaml" --set previewAuth.stage=require \
  --set previewAuth.permitConfirmedGeneration=1 --set preview.allowPublicWithAuth=confirmed --set previewAuth.sessionTimeout=900
expect pa-public 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | .spec | has("inboundCIDRs")' false
cm pa-public preview-controller PATCHY_PREVIEW_AUTH_REQUIRED true
expect pa-public "$pcm | .data.PATCHY_PREVIEW_AUTH_ANNOTATIONS | from_json | .\"patchy-preview-0\" | .\"alb.ingress.kubernetes.io/auth-session-timeout\"" 900
# sessionTimeout is a ceiling the slot Ingress policies admit up to, recorded
# on patchy-preview-ingresses for the next upgrade's lookup (both policies
# carry it as a variable). A render without a cluster sees no live policy, so
# it applies sessionTimeout itself: placeholder, controller and policy sets.
paceil='select(.kind == "ValidatingAdmissionPolicy" and (.metadata.name == "patchy-preview-ingresses" or .metadata.name == "patchy-preview-ingress-auth"))'
expect pa-public "$paceil | .metadata.name + \" \" + (.metadata.annotations.\"patchy.bitwisemedia.uk/preview-auth-session-timeout\" // \"-\") + \" \" + (.spec.variables[] | select(.name == \"authTimeoutCeiling\") | .expression)" \
  'patchy-preview-ingresses 900 900
patchy-preview-ingress-auth - 900'
expect pa-require "$paceil | .metadata.annotations.\"patchy.bitwisemedia.uk/preview-auth-session-timeout\" // \"-\"" '3600
-'
expect pa-permit "$paceil | .metadata.annotations.\"patchy.bitwisemedia.uk/preview-auth-session-timeout\"" 3600
expect pa-public 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-placeholder") | .metadata.annotations."alb.ingress.kubernetes.io/auth-session-timeout"' 900
# Both rules judge the timeout as a ceiling, every other pinned key exactly.
expect pa-public "$paceil | .metadata.name + \" \" + (.spec.validations[].expression | select(test(\"auth-session-timeout' [?]\")) | \"ceiling\")" \
  'patchy-preview-ingresses ceiling
patchy-preview-ingress-auth ceiling'
notes_has notes-pa-require "SESSION TIMEOUT CHANGE" no
# shellcheck disable=SC2086
render pa-prefix-only $pafull -f "$out/no-allowlist.yaml" --set previewAuth.stage=require \
  --set previewAuth.permitConfirmedGeneration=1 --set 'preview.prefixListsIDs={pl-0123456789abcdef0}'
expect pa-prefix-only 'select(.kind == "IngressClassParams" and .metadata.name == "alb-preview") | (.spec | has("inboundCIDRs") | tostring) + " " + (.spec.prefixListsIDs | join(","))' \
  'false pl-0123456789abcdef0'

# ---- the preview gateway (previewAuth.gateway): off is absent ---------------
# Off (every render above), no gateway object, label, key or slot peer
# exists. On, the relay's Pods carry the stable gateway label the slot
# NetworkPolicy admits beside the ALB's subnets, their own policy reaches
# the slots' port http, and one exposure routes *.<hostSuffix> (and a
# sign-in host of its own) to Service <fullname>-preview-gateway.
pgw=$fixtures/preview-gateway.yaml
for r in default preview pa-permit pa-require; do
  expect "$r" 'select(.metadata.name == "patchy-preview-gateway") | .kind' ""
  expect "$r" 'select(.kind == "Deployment") | .spec.template.metadata.labels."patchy.bitwisemedia.uk/preview-gateway" | select(. != null)' ""
  expect "$r" 'select(.kind == "ConfigMap") | (.data // {}) | keys | .[] | select(test("GATEWAY"))' ""
  expect "$r" 'select(.kind == "NetworkPolicy") | .spec.ingress[]?.from[]? | select(has("podSelector")) | (.podSelector.matchLabels // {}) | keys | .[] | select(. == "patchy.bitwisemedia.uk/preview-gateway")' ""
done
render pg -f "$pf" -f "$pav" -f "$pgw"
pgsel='select(.metadata.name == "patchy-preview-gateway" or .metadata.name == "patchy-preview-auth-config" or (.kind == "NetworkPolicy" and (.metadata.name == "patchy-preview-auth" or (.metadata.name == "preview-isolation" and .metadata.namespace == "patchy-preview-0"))))'
# golden_select NAME FILE [helm args...]: the gateway's objects (pgsel) of a
# render, the chart and app versions written VERSION (a release bumps them),
# are the committed golden FILE, as same_render compares them.
golden_select() {
  name=$1
  file=$2
  shift 2
  versions="s/$(yq eval '.version' "$chart/Chart.yaml" | sed 's/[.]/\\./g')/VERSION/g;s/$(yq eval '.appVersion' "$chart/Chart.yaml" | sed 's/[.]/\\./g')/VERSION/g"
  if ! helm template patchy "$chart" --namespace patchy "$@" >"$out/$name.full" 2>"$out/$name.err"; then
    fail "$name: render failed: $(cat "$out/$name.err")"
  elif ! yq eval "$pgsel" "$out/$name.full" | sed "$versions" >"$out/$name.golden"; then
    fail "$name: yq failed"
  elif ! same_render "$file" "$out/$name.golden"; then
    fail "$name: render differs from $file: $(head -20 "$out/same.diff")"
  fi
}
golden_select pg-golden-ingress "$golden/preview-gateway.ingress.yaml" -f "$pf" -f "$pav" -f "$pgw" \
  --set previewAuth.gateway.signInHost=signin.patchy.devthe.net --set 'previewAuth.gateway.inboundCIDRs={203.0.113.0/24}' \
  --set previewAuth.gateway.ingress.inboundCIDRsAnnotation=nginx.ingress.kubernetes.io/whitelist-source-range \
  --set previewAuth.gateway.ingress.className=nginx
golden_select pg-golden-httproute "$golden/preview-gateway.httproute.yaml" -f "$pf" -f "$pav" -f "$pgw" \
  --set previewAuth.gateway.ingress.enabled=false --set-json 'previewAuth.gateway.httpRoute.parentRefs=[{"name":"edge","namespace":"gateways"}]'
golden_select pg-golden-loadbalancer "$golden/preview-gateway.loadbalancer.yaml" -f "$pf" -f "$pav" -f "$pgw" \
  --set previewAuth.gateway.ingress.enabled=false --set previewAuth.gateway.service.type=LoadBalancer \
  --set previewAuth.gateway.forwardedHops=0 --set 'previewAuth.gateway.inboundCIDRs={203.0.113.0/24,2001:db8::/32}'
golden_select pg-golden-none "$golden/preview-gateway.none.yaml" -f "$pf" -f "$pav" -f "$pgw" \
  --set previewAuth.gateway.ingress.enabled=false --set previewAuth.gateway.forwardedHops=null \
  --set 'previewAuth.gateway.publishedAddresses={100.64.0.7}' --set preview.legacyIngress=false
pgdep='select(.kind == "Deployment" and .metadata.name == "patchy-preview-auth")'
expect pg "$pgdep | .spec.template.metadata.labels.\"patchy.bitwisemedia.uk/preview-gateway\"" true
cm pg preview-auth PATCHY_PREVIEW_AUTH_GATEWAY true
cm pg preview-auth PATCHY_PREVIEW_AUTH_GATEWAY_SIGN_IN_HOST preview-auth.patchy.devthe.net
cm pg preview-auth PATCHY_PREVIEW_AUTH_GATEWAY_FORWARDED_HOPS 1
cm pg preview-auth PATCHY_PREVIEW_AUTH_GATEWAY_INBOUND_CIDRS null
# The host probe's mode: either path's redirect while the legacy slot
# Ingress still serves (derived, no key), the gateway's alone without it.
cm pg preview-auth PATCHY_PREVIEW_AUTH_PROBE_MODE null
render pg-nolegacy -f "$pf" -f "$pav" -f "$pgw" --set preview.legacyIngress=false
cm pg-nolegacy preview-auth PATCHY_PREVIEW_AUTH_PROBE_MODE gateway
# Off the legacy path the slots admit the gateway's Pods alone, though the
# ALB values stay set (release N keeps the idle ALB as a way back): no
# Ingress puts a target in a slot, so no subnet is admitted to one.
expect pg-nolegacy 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.ingress[0].from[] | select(has("ipBlock")) | .ipBlock.cidr' ""
expect pg-nolegacy 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.ingress[0].from | length' '1
1'
# Without the gateway either, nothing reaches a slot.
render pf-nolegacy -f "$pf" --set preview.legacyIngress=false
expect pf-nolegacy 'select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation") | .spec.ingress | length' '0
0'
render pa-nolegacy -f "$pf" -f "$pav" --set preview.legacyIngress=false
cm pa-nolegacy preview-auth PATCHY_PREVIEW_AUTH_PROBE_MODE null
if ! grep -q '"preview-auth-probe-mode"' cmd/preview-auth/serve.go; then
  fail "pg-nolegacy: PATCHY_PREVIEW_AUTH_PROBE_MODE is no preview-auth flag"
fi
# Every gateway key is a preview-auth flag.
for key in $(get pg 'select(.kind == "ConfigMap" and .metadata.name == "patchy-preview-auth-config") | .data | keys | .[] | select(test("GATEWAY"))'); do
  flag=$(echo "${key#PATCHY_}" | tr 'A-Z_' 'a-z-')
  if ! grep -q "\"$flag\"" cmd/preview-auth/serve.go; then
    fail "pg: ConfigMap key $key is no preview-auth flag ($flag)"
  fi
done
# The sign-in host is the relay's, which its own Ingress routes, so the
# gateway's Ingress carries only the wildcard; health check on the reserved
# path, never forwarded.
pging='select(.kind == "Ingress" and .metadata.name == "patchy-preview-gateway")'
expect pg "$pging | .spec.rules[].host" '*.preview.patchy.devthe.net'
expect pg "$pging | .spec.rules[0].http.paths[0].backend.service.name + \":\" + .spec.rules[0].http.paths[0].backend.service.port.name" 'patchy-preview-gateway:http'
expect pg "$pging | .metadata.annotations.\"alb.ingress.kubernetes.io/healthcheck-path\"" /.patchy/healthz
expect pg "$pging | .metadata.annotations.\"alb.ingress.kubernetes.io/target-type\"" ip
expect pg "$pging | .spec | has(\"tls\")" false
expect pg 'select(.kind == "Service" and .metadata.name == "patchy-preview-gateway") | .spec.type + " " + (.spec.ports[0].port | tostring) + " " + .spec.ports[0].targetPort' 'ClusterIP 80 gateway'
# The exposure targets the gateway's own listener (8082), where even the
# relay's endpoints are limited behind the gateway's hops; the relay's policy
# admits it, and the relay's Service keeps the relay's port.
cm pg preview-auth PATCHY_PREVIEW_AUTH_GATEWAY_LISTEN_ADDR :8082
expect pg "$pgdep | .spec.template.spec.containers[0].ports[] | select(.name == \"gateway\") | .containerPort" 8082
expect pg 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-preview-auth") | .spec.ingress[0].ports | map(.port | tostring) | join(",")' '8080,8082'
expect pg 'select(.kind == "Service" and .metadata.name == "patchy-preview-auth") | .spec.ports | map(.name + ":" + (.port | tostring) + ">" + .targetPort) | join(",")' 'http:8080>http'
expect pg 'select(.kind == "Service" and .metadata.name == "patchy-preview-gateway") | .spec.selector."app.kubernetes.io/name"' preview-auth
# The slot policy: the ALB's subnets (while legacy) and the gateway's Pods,
# port http only; DNS by both the address and the kube-system selector.
pgslot='select(.kind == "NetworkPolicy" and .metadata.name == "preview-isolation")'
expect pg "$pgslot | .spec.ingress | length" '1
1'
expect pg "$pgslot | .spec.ingress[0].from[] | select(has(\"podSelector\")) | .namespaceSelector.matchLabels.\"kubernetes.io/metadata.name\" + \" \" + (.podSelector.matchLabels | to_entries | map(.key + \"=\" + .value) | join(\",\"))" 'patchy patchy.bitwisemedia.uk/preview-gateway=true
patchy patchy.bitwisemedia.uk/preview-gateway=true'
expect pg "$pgslot | .spec.ingress[0].from[] | select(has(\"ipBlock\")) | .ipBlock.cidr" '10.40.128.0/24
10.40.129.0/24
10.40.128.0/24
10.40.129.0/24'
expect pg "$pgslot | .spec.ingress[0].ports | map(.protocol + \"/\" + .port) | join(\",\")" 'TCP/http
TCP/http'
expect pg "$pgslot | .spec.egress[0].to | length" '2
2'
# The gateway's own policy: DNS, 443/6443 as before, and the slots' http.
expect pg 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-preview-auth") | .spec.egress[] | select(.to // [] | any_c(has("namespaceSelector") and .namespaceSelector | has("matchExpressions"))) | .to[0].namespaceSelector.matchExpressions[0].values | join(",")' 'patchy-preview-0,patchy-preview-1'
expect pg 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-preview-auth") | .spec.egress[] | select(.to // [] | any_c(has("namespaceSelector") and .namespaceSelector | has("matchExpressions"))) | .ports | map(.protocol + "/" + .port) | join(",")' 'TCP/http'
# Off the legacy path with no ALB values, the gateway is the only peer.
render pg-only -f "$pf" -f "$pav" -f "$pgw" -f "$noalb"
expect pg-only "$pgslot | .spec.ingress[0].from | length" '1
1'
# A sign-in host of its own: routed beside the wildcard, Dex told in NOTES.
render pg-signin -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.signInHost=signin.patchy.devthe.net
expect pg-signin "$pging | .spec.rules[].host" '*.preview.patchy.devthe.net
signin.patchy.devthe.net'
cm pg-signin preview-auth PATCHY_PREVIEW_AUTH_GATEWAY_SIGN_IN_HOST signin.patchy.devthe.net
notes notes-pg-signin -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.signInHost=signin.patchy.devthe.net
notes_has notes-pg-signin "https://signin.patchy.devthe.net/dex/callback" yes
notes_has notes-pg-signin "Preview gateway: on. Viewers sign in at https://signin.patchy.devthe.net/start" yes
notes_has notes-pg-signin "the preview ALB keeps serving" yes
notes notes-pg -f "$pf" -f "$pav" -f "$pgw"
notes_has notes-pg "also needs the redirect URI" no
notes_has notes-pg "ALLOWLIST AT THE GATEWAY ONLY" no
# The allowlist: at the edge where the exposure can hold it, at the gateway
# always.
render pg-cidrs -f "$pf" -f "$pav" -f "$pgw" --set 'previewAuth.gateway.inboundCIDRs={203.0.113.0/24,198.51.100.7/32}' \
  --set previewAuth.gateway.ingress.inboundCIDRsAnnotation=nginx.ingress.kubernetes.io/whitelist-source-range
cm pg-cidrs preview-auth PATCHY_PREVIEW_AUTH_GATEWAY_INBOUND_CIDRS 203.0.113.0/24,198.51.100.7/32
expect pg-cidrs "$pging | .metadata.annotations.\"nginx.ingress.kubernetes.io/whitelist-source-range\"" 203.0.113.0/24,198.51.100.7/32
notes notes-pg-cidrs-gw-only -f "$pf" -f "$pav" -f "$pgw" --set 'previewAuth.gateway.inboundCIDRs={203.0.113.0/24}'
notes_has notes-pg-cidrs-gw-only "ALLOWLIST AT THE GATEWAY ONLY" yes
render pg-lb -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.ingress.enabled=false \
  --set previewAuth.gateway.service.type=LoadBalancer --set previewAuth.gateway.forwardedHops=0 \
  --set 'previewAuth.gateway.inboundCIDRs={203.0.113.0/24}' --set-json 'previewAuth.gateway.service.annotations={"service.beta.kubernetes.io/aws-load-balancer-type":"external"}'
expect pg-lb 'select(.kind == "Service" and .metadata.name == "patchy-preview-gateway") | .spec.type + " " + (.spec.loadBalancerSourceRanges | join(","))' 'LoadBalancer 203.0.113.0/24'
expect pg-lb 'select(.kind == "Service" and .metadata.name == "patchy-preview-gateway") | .metadata.annotations."service.beta.kubernetes.io/aws-load-balancer-type"' external
expect pg-lb 'select((.kind == "Ingress" or .kind == "HTTPRoute") and .metadata.name == "patchy-preview-gateway") | .kind' ''
cm pg-lb preview-auth PATCHY_PREVIEW_AUTH_GATEWAY_FORWARDED_HOPS 0
# Local, so the gateway's peer is the client that forwardedHops 0 and
# inboundCIDRs judge: under Cluster kube-proxy hands it a node address.
expect pg-lb 'select(.kind == "Service" and .metadata.name == "patchy-preview-gateway") | .spec.externalTrafficPolicy' Local
expect pg 'select(.kind == "Service" and .metadata.name == "patchy-preview-gateway") | .spec | has("externalTrafficPolicy")' false
notes notes-pg-lb -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.ingress.enabled=false \
  --set previewAuth.gateway.service.type=LoadBalancer --set previewAuth.gateway.forwardedHops=0 \
  --set 'previewAuth.gateway.inboundCIDRs={203.0.113.0/24}'
notes_has notes-pg-lb "is a LoadBalancer (externalTrafficPolicy Local)" yes
notes_has notes-pg-lb "CLIENT ADDRESS" no
render pg-lb-cluster -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.ingress.enabled=false \
  --set previewAuth.gateway.service.type=LoadBalancer --set previewAuth.gateway.forwardedHops=0 \
  --set previewAuth.gateway.service.externalTrafficPolicy=Cluster
expect pg-lb-cluster 'select(.kind == "Service" and .metadata.name == "patchy-preview-gateway") | .spec.externalTrafficPolicy' Cluster
notes notes-pg-lb-cluster -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.ingress.enabled=false \
  --set previewAuth.gateway.service.type=LoadBalancer --set previewAuth.gateway.forwardedHops=0 \
  --set previewAuth.gateway.service.externalTrafficPolicy=Cluster --set 'previewAuth.gateway.inboundCIDRs={203.0.113.0/24}'
notes_has notes-pg-lb-cluster "CLIENT ADDRESS: previewAuth.gateway.service.externalTrafficPolicy is Cluster" yes
notes_has notes-pg-lb-cluster "inboundCIDRs then refuses every viewer" yes
expect_fail 'gateway externalTrafficPolicy that is not one' "'/previewAuth/gateway/service/externalTrafficPolicy'" \
  -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.service.externalTrafficPolicy=Nearest
render pg-route -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.ingress.enabled=false \
  --set previewAuth.gateway.signInHost=signin.patchy.devthe.net \
  --set-json 'previewAuth.gateway.httpRoute.parentRefs=[{"name":"edge","namespace":"gateways"}]'
pgroute='select(.kind == "HTTPRoute" and .metadata.name == "patchy-preview-gateway")'
expect pg-route "$pgroute | .spec.hostnames | join(\",\")" '*.preview.patchy.devthe.net,signin.patchy.devthe.net'
expect pg-route "$pgroute | .spec.parentRefs[0].name + \"/\" + .spec.parentRefs[0].namespace" edge/gateways
expect pg-route "$pgroute | .spec.rules[0].backendRefs[0].name + \":\" + (.spec.rules[0].backendRefs[0].port | tostring)" patchy-preview-gateway:80
expect pg-route 'select(.kind == "Ingress" and .metadata.name == "patchy-preview-gateway") | .kind' ''
render pg-none -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.ingress.enabled=false \
  --set previewAuth.gateway.forwardedHops=null --set 'previewAuth.gateway.publishedAddresses={100.64.0.7,fd7a:115c:a1e0::7}'
cm pg-none preview-auth PATCHY_PREVIEW_AUTH_GATEWAY_FORWARDED_HOPS null
expect pg-none 'select((.kind == "Ingress" or .kind == "HTTPRoute") and .metadata.name == "patchy-preview-gateway") | .kind' ''
expect pg-none 'select(.kind == "ConfigMap" and .metadata.name == "patchy-preview-auth-config") | .metadata.annotations."patchy.bitwisemedia.uk/preview-gateway-published-addresses"' '100.64.0.7,fd7a:115c:a1e0::7'
expect pg 'select(.kind == "ConfigMap" and .metadata.name == "patchy-preview-auth-config") | .metadata.annotations."patchy.bitwisemedia.uk/preview-gateway-published-addresses"' null
notes notes-pg-none -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.ingress.enabled=false --set previewAuth.gateway.forwardedHops=null
notes_has notes-pg-none "none rendered. Route *.preview.patchy.devthe.net to" yes
# publishedAddresses is never routed by, and patchy check project compares DNS
# with it (cmd/patchy/internal/projectcheck/gateway.go reads the annotation),
# so no doc may still say nothing reads it.
if ! grep -q 'publishedAnnotation = "patchy.bitwisemedia.uk/preview-gateway-published-addresses"' \
  cmd/patchy/internal/projectcheck/gateway.go; then
  fail "publishedAddresses: patchy check project no longer reads the annotation the chart writes"
fi
if grep -rn -i 'published' charts/patchy/values.yaml charts/patchy/values.schema.json charts/patchy/templates \
  docs/configuration/preview-auth.md docs/configuration/preview-gateway.md docs/deployment/helm.md HANDOFF.md |
  grep -i -e 'nothing reads' -e 'a later .*check project'; then
  fail "publishedAddresses: a doc still says nothing reads it, but patchy check project does"
fi
expect_fail 'gateway without the relay' 'previewAuth.gateway.enabled requires previewAuth.enabled' \
  -f "$pf" -f "$pgw" --set previewAuth.enabled=false
expect_fail 'gateway sign-in host under the preview suffix' 'must not be under preview.hostSuffix' \
  -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.signInHost=signin.preview.patchy.devthe.net
expect_fail 'gateway with two exposures' 'exposes the gateway ingress and httpRoute at once' \
  -f "$pf" -f "$pav" -f "$pgw" --set-json 'previewAuth.gateway.httpRoute.parentRefs=[{"name":"edge"}]'
expect_fail 'gateway with three exposures' 'exposes the gateway ingress and httpRoute and loadBalancer at once' \
  -f "$pf" -f "$pav" -f "$pgw" --set-json 'previewAuth.gateway.httpRoute.parentRefs=[{"name":"edge"}]' \
  --set previewAuth.gateway.service.type=LoadBalancer
for exposure in "--set previewAuth.gateway.ingress.enabled=true" \
  "--set previewAuth.gateway.ingress.enabled=false --set previewAuth.gateway.service.type=LoadBalancer"; do
  # shellcheck disable=SC2086
  expect_fail "gateway exposure without forwarded hops ($exposure)" 'previewAuth.gateway.forwardedHops is required with the' \
    -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.forwardedHops=null $exposure
done
expect_fail 'gateway route without forwarded hops' 'previewAuth.gateway.forwardedHops is required with the httpRoute exposure' \
  -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.forwardedHops=null --set previewAuth.gateway.ingress.enabled=false \
  --set-json 'previewAuth.gateway.httpRoute.parentRefs=[{"name":"edge"}]'
expect_fail 'gateway on the preview ALB class' 'previewAuth.gateway.ingress.className must not be alb-preview' \
  -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.ingress.className=alb-preview
expect_fail 'gateway inbound CIDR that is not one' "'/previewAuth/gateway/inboundCIDRs/0'" \
  -f "$pf" -f "$pav" -f "$pgw" --set 'previewAuth.gateway.inboundCIDRs={203.0.113.7}'
expect_fail 'gateway forwarded hops negative' "'/previewAuth/gateway/forwardedHops'" \
  -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.forwardedHops=-1
expect_fail 'gateway Service of type NodePort' "'/previewAuth/gateway/service/type'" \
  -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.service.type=NodePort
# The gateway's own TLS (tls.secrets), for a LoadBalancer or no exposure:
# each Secret one projected subdirectory, the listener on 8443 behind the
# Service's 443, admitted by the relay's policy beside 8080. Refused where
# an edge terminates TLS, and unless *.<hostSuffix> and the sign-in host
# are both covered.
pgtls='previewAuth.gateway.tls.secrets=[{"secretName":"preview-wildcard","hosts":["*.preview.patchy.devthe.net"]},{"secretName":"preview-signin","hosts":["preview-auth.patchy.devthe.net"]}]'
pglb=$out/gateway-lb.yaml
printf 'previewAuth:\n    gateway:\n        forwardedHops: 0\n        service:\n            type: LoadBalancer\n        ingress:\n            enabled: false\n' >"$pglb"
golden_select pg-golden-loadbalancer-tls "$golden/preview-gateway.loadbalancer-tls.yaml" -f "$pf" -f "$pav" -f "$pgw" \
  -f "$pglb" --set-json "$pgtls"
render pg-tls -f "$pf" -f "$pav" -f "$pgw" -f "$pglb" --set-json "$pgtls"
cm pg-tls preview-auth PATCHY_PREVIEW_AUTH_GATEWAY_TLS_DIR /etc/patchy/preview-gateway/tls
cm pg-tls preview-auth PATCHY_PREVIEW_AUTH_GATEWAY_TLS_LISTEN_ADDR :8443
cm pg preview-auth PATCHY_PREVIEW_AUTH_GATEWAY_TLS_DIR null
for key in $(get pg-tls 'select(.kind == "ConfigMap" and .metadata.name == "patchy-preview-auth-config") | .data | keys | .[] | select(test("GATEWAY_TLS"))'); do
  flag=$(echo "${key#PATCHY_}" | tr 'A-Z_' 'a-z-')
  if ! grep -q "\"$flag\"" cmd/preview-auth/serve.go; then
    fail "pg-tls: ConfigMap key $key is no preview-auth flag ($flag)"
  fi
done
expect pg-tls "$pgdep | .spec.template.spec.containers[0].ports[] | select(.name == \"https\") | .containerPort" 8443
expect pg "$pgdep | .spec.template.spec.containers[0].ports[] | select(.name == \"https\") | .containerPort" ''
expect pg-tls "$pgdep | .spec.template.spec.containers[0].volumeMounts[] | select(.name == \"gateway-tls\") | .mountPath + \" \" + (.readOnly | tostring)" '/etc/patchy/preview-gateway/tls true'
expect pg-tls "$pgdep | .spec.template.spec.volumes[] | select(.name == \"gateway-tls\") | .projected.sources[] | .secret.name + \" \" + (.secret.items | map(.key + \"=\" + .path) | join(\",\"))" 'preview-wildcard tls.crt=0/tls.crt,tls.key=0/tls.key
preview-signin tls.crt=1/tls.crt,tls.key=1/tls.key'
expect pg-tls 'select(.kind == "Service" and .metadata.name == "patchy-preview-gateway") | .spec.ports | map(.name + ":" + (.port | tostring) + ">" + .targetPort) | join(",")' 'http:80>gateway,https:443>https'
expect pg 'select(.kind == "Service" and .metadata.name == "patchy-preview-gateway") | .spec.ports | map(.name) | join(",")' http
expect pg-tls 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-preview-auth") | .spec.ingress[0].ports | map(.port | tostring) | join(",")' '8080,8082,8443'
render pg-tls-none -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.ingress.enabled=false --set-json "$pgtls"
cm pg-tls-none preview-auth PATCHY_PREVIEW_AUTH_GATEWAY_TLS_DIR /etc/patchy/preview-gateway/tls
expect_fail 'gateway TLS with an Ingress' 'previewAuth.gateway.tls.secrets is refused with the ingress exposure' \
  -f "$pf" -f "$pav" -f "$pgw" --set-json "$pgtls"
expect_fail 'gateway TLS with an HTTPRoute' 'previewAuth.gateway.tls.secrets is refused with the httpRoute exposure' \
  -f "$pf" -f "$pav" -f "$pgw" --set previewAuth.gateway.ingress.enabled=false --set-json "$pgtls" \
  --set-json 'previewAuth.gateway.httpRoute.parentRefs=[{"name":"edge"}]'
expect_fail 'gateway TLS for another host' 'host other.example.com (Secret x) is neither' \
  -f "$pf" -f "$pav" -f "$pgw" -f "$pglb" \
  --set-json 'previewAuth.gateway.tls.secrets=[{"secretName":"x","hosts":["*.preview.patchy.devthe.net","other.example.com"]}]'
expect_fail 'gateway TLS without the sign-in host' 'covers no preview-auth.patchy.devthe.net' \
  -f "$pf" -f "$pav" -f "$pgw" -f "$pglb" \
  --set-json 'previewAuth.gateway.tls.secrets=[{"secretName":"x","hosts":["*.preview.patchy.devthe.net"]}]'
expect_fail 'gateway TLS without the preview hosts' 'covers no *.preview.patchy.devthe.net' \
  -f "$pf" -f "$pav" -f "$pgw" -f "$pglb" \
  --set-json 'previewAuth.gateway.tls.secrets=[{"secretName":"x","hosts":["preview-auth.patchy.devthe.net"]}]'
expect_fail 'gateway TLS Secret without hosts' "missing property 'hosts'" \
  -f "$pf" -f "$pav" -f "$pgw" -f "$pglb" --set-json 'previewAuth.gateway.tls.secrets=[{"secretName":"x"}]'

# ---- feature on: keys on the right controllers ------------------------------
render on -f "$fixtures/repository-images.yaml"
cm on source-controller PATCHY_REPOSITORY_IMAGES true
cm on source-controller PATCHY_REPOSITORY_IMAGE_REGISTRIES \
  123456789012.dkr.ecr.us-east-1.amazonaws.com/patchy/,ghcr.io/example/agent-images/
cm on source-controller PATCHY_REPOSITORY_IMAGE_MAX_BYTES 4294967296
cm on source-controller PATCHY_REPOSITORY_IMAGE_ON_REJECT default
cm on source-controller PATCHY_REPOSITORY_IMAGE_ALLOW_UNSIGNED false
cm on source-controller PATCHY_REPOSITORY_IMAGE_COSIGN_KEY_FILE /etc/patchy/repository-image/cosign.pub
cm on source-controller DOCKER_CONFIG /etc/patchy/registry
cm on source-controller PATCHY_AGENT_EPHEMERAL_STORAGE null
# No preview prefix to deny without previews.
cm on source-controller PATCHY_REPOSITORY_IMAGE_DENIED_REGISTRIES null
for c in investigation-controller remediation-controller; do
  cm on "$c" PATCHY_REPOSITORY_IMAGES true
  cm on "$c" PATCHY_AGENT_EPHEMERAL_STORAGE 8Gi
  cm on "$c" PATCHY_REPOSITORY_IMAGE_REGISTRIES null
  cm on "$c" DOCKER_CONFIG null
done
cm on remediation-controller PATCHY_CHANGESET_MAX_ENTRIES 500
cm on investigation-controller PATCHY_CHANGESET_MAX_ENTRIES null
cm on integration-controller PATCHY_REPOSITORY_IMAGES true
cm on context-controller PATCHY_REPOSITORY_IMAGES null
# the runner-image comment reads Repositories: integration-controller's Role
# grants get/list/watch on them (read-only, as in the kustomize base)
for r in default on; do
  expect "$r" 'select(.kind == "Role" and .metadata.name == "patchy-integration-controller") | .rules[] | select((.resources | length) == 1 and .resources[0] == "repositories") | .verbs | join(",")' "get,list,watch"
done

# ---- feature on: source-controller's mounts, and only its -------------------
src='select(.kind == "Deployment" and .metadata.name == "patchy-source-controller") | .spec.template.spec'
expect on "$src | .containers[0].volumeMounts[] | select(.name == \"repository-image-key\") | .mountPath + \" \" + (.readOnly | tostring)" \
  "/etc/patchy/repository-image true"
expect on "$src | .containers[0].volumeMounts[] | select(.name == \"registry\") | .mountPath + \" \" + (.readOnly | tostring)" \
  "/etc/patchy/registry true"
expect on "$src | .volumes[] | select(.name == \"repository-image-key\") | .configMap.name" patchy-repository-image-key
expect on "$src | .volumes[] | select(.name == \"registry\") | .secret.secretName" patchy-registry
expect on "$src | .volumes[] | select(.name == \"registry\") | .secret.items[] | .key + \" -> \" + .path" \
  ".dockerconfigjson -> config.json"
# Optional: a missing Secret, or one without the .dockerconfigjson key, must
# not hold source-controller (and with it the artifact server) in
# ContainerCreating; resolution degrades to anonymous and rejects per image.
expect on "$src | .volumes[] | select(.name == \"registry\") | .secret.optional" true
expect on 'select(.kind == "Deployment" and .metadata.name == "patchy-source-controller") | .spec.template.metadata.annotations["checksum/repository-image-key"] | length' 64
expect on 'select(.kind == "Deployment" and .metadata.name != "patchy-source-controller") | .spec.template.spec.volumes[].name | select(. == "registry" or . == "repository-image-key")' ""
expect on 'select(.kind == "ConfigMap" and .metadata.name == "patchy-repository-image-key") | .data["cosign.pub"]' \
  "$(yq eval '.agent.repositoryImages.cosignPublicKey' "$fixtures/repository-images.yaml")"

# ---- feature on: the agent namespace's pull credential ----------------------
expect on 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-agent") | .imagePullSecrets[].name' patchy-registry
# pullSecretData renders the Secret into both namespaces: agent.namespace for
# the kubelet, the release namespace for source-controller's mount.
expect on 'select(.kind == "Secret" and .metadata.name == "patchy-registry") | .metadata.namespace + " " + .type' \
  "patchy kubernetes.io/dockerconfigjson
patchy-agents kubernetes.io/dockerconfigjson"
expect on 'select(.kind == "Secret" and .metadata.name == "patchy-registry") | .data[".dockerconfigjson"] | @base64d' \
  '{"auths":{"ghcr.io":{"auth":"cGxhY2Vob2xkZXI6cGxhY2Vob2xkZXI="}}}
{"auths":{"ghcr.io":{"auth":"cGxhY2Vob2xkZXI6cGxhY2Vob2xkZXI="}}}'
expect on 'select(.kind == "Secret" and .metadata.namespace == "patchy") | .metadata.labels["app.kubernetes.io/name"]' \
  source-controller
render on-one-namespace -f "$fixtures/repository-images.yaml" --set agent.namespace=patchy
expect on-one-namespace 'select(.kind == "Secret" and .metadata.name == "patchy-registry") | .metadata.namespace' patchy
render on-no-data -f "$fixtures/repository-images.yaml" --set agent.repositoryImages.pullSecretData=
expect on-no-data 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-agent") | .imagePullSecrets[].name' patchy-registry
expect on-no-data 'select(.kind == "Secret") | .metadata.name' ""
expect on-no-data "$src | .volumes[] | select(.name == \"registry\") | .secret.optional" true
render on-no-secret -f "$fixtures/repository-images.yaml" \
  --set agent.repositoryImages.pullSecretData= --set agent.repositoryImages.pullSecret=
expect on-no-secret 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-agent") | .imagePullSecrets' null
cm on-no-secret source-controller DOCKER_CONFIG null
expect on-no-secret "$src | .volumes[] | select(.name == \"registry\") | .name" ""

# ---- feature on: the EKS Pod Identity agent for source-controller -----------
srcnp='select(.kind == "NetworkPolicy" and .metadata.name == "patchy-source-controller") | .spec.egress[] | select(.to[].ipBlock.cidr == "169.254.170.23/32")'
expect on "$srcnp | .to[].ipBlock.cidr" "169.254.170.23/32
fd00:ec2::23/128"
expect on "$srcnp | .ports[] | .protocol + \"/\" + (.port | tostring)" "TCP/80"
render on-cilium -f "$fixtures/repository-images.yaml" --set agent.networkPolicy.mode=cilium --set agent.networkPolicy.broadEgress=auto
expect on-cilium 'select(.kind == "CiliumNetworkPolicy" and .metadata.name == "patchy-source-controller-cloud-credentials") | .spec.egress[0].toEntities[0] + " " + .spec.egress[0].toPorts[0].ports[0].port' \
  "host 80"
expect on-cilium 'select(.kind == "CiliumNetworkPolicy" and .metadata.name == "patchy-source-controller-cloud-credentials") | .spec.endpointSelector.matchLabels["app.kubernetes.io/name"]' \
  source-controller

# ---- feature on: the other values reach their keys --------------------------
render on-tuned -f "$fixtures/repository-images.yaml" \
  --set agent.repositoryImages.onReject=handoff --set agent.repositoryImages.maxBytes=1073741824 \
  --set agent.repositoryImages.changesetMaxEntries=2000 --set agent.repositoryImages.ephemeralStorage=20Gi \
  --set agent.repositoryImages.cosignPublicKey= --set agent.repositoryImages.allowUnsigned=true
cm on-tuned source-controller PATCHY_REPOSITORY_IMAGE_ON_REJECT handoff
cm on-tuned source-controller PATCHY_REPOSITORY_IMAGE_MAX_BYTES 1073741824
cm on-tuned source-controller PATCHY_REPOSITORY_IMAGE_ALLOW_UNSIGNED true
cm on-tuned source-controller PATCHY_REPOSITORY_IMAGE_COSIGN_KEY_FILE null
cm on-tuned remediation-controller PATCHY_CHANGESET_MAX_ENTRIES 2000
cm on-tuned investigation-controller PATCHY_AGENT_EPHEMERAL_STORAGE 20Gi
expect on-tuned 'select(.metadata.name == "patchy-repository-image-key") | .kind' ""
expect on-tuned "$src | .volumes[] | select(.name == \"repository-image-key\") | .name" ""
render on-extra -f "$fixtures/repository-images.yaml" \
  --set sourceController.config.extra.PATCHY_REPOSITORY_IMAGE_ON_REJECT=handoff
cm on-extra source-controller PATCHY_REPOSITORY_IMAGE_ON_REJECT handoff

# ---- the render-time guards: each fails, each passes once fixed -------------
f=$fixtures/repository-images.yaml
expect_fail "registries empty" "agent.repositoryImages.enabled requires agent.repositoryImages.registries" \
  -f "$f" --set-json 'agent.repositoryImages.registries=[]'
expect_fail "ephemeralStorage empty" "agent.repositoryImages.enabled requires agent.repositoryImages.ephemeralStorage" \
  -f "$f" --set agent.repositoryImages.ephemeralStorage=
expect_fail "no cosign key" "set agent.repositoryImages.allowUnsigned: true to admit unsigned images instead" \
  -f "$f" --set agent.repositoryImages.cosignPublicKey=
render guard-unsigned-fixed -f "$f" --set agent.repositoryImages.cosignPublicKey= --set agent.repositoryImages.allowUnsigned=true
expect_fail "pullSecretData without pullSecret" "agent.repositoryImages.pullSecretData requires agent.repositoryImages.pullSecret" \
  -f "$f" --set agent.repositoryImages.pullSecret=
expect_fail "no agent NetworkPolicy" "agent.networkPolicy.create is false" \
  -f "$f" --set agent.networkPolicy.create=false
expect_fail "broad egress under none" \
  'agent.networkPolicy.broadEgress ("auto") resolves to broad under agent.networkPolicy.mode "none"' \
  -f "$f" --set agent.networkPolicy.broadEgress=auto
expect_fail "broad egress names the fix" "Set agent.networkPolicy.broadEgress: never (brokered claude runners only)" \
  -f "$f" --set agent.networkPolicy.broadEgress=auto
expect_fail "broad egress under istio" \
  'agent.networkPolicy.broadEgress ("auto") resolves to broad under agent.networkPolicy.mode "istio"' \
  -f "$f" --set agent.networkPolicy.mode=istio --set agent.networkPolicy.broadEgress=auto
expect_fail "broad egress always" \
  'agent.networkPolicy.broadEgress ("always") resolves to broad under agent.networkPolicy.mode "cilium"' \
  -f "$f" --set agent.networkPolicy.mode=cilium --set agent.networkPolicy.broadEgress=always
render guard-egress-fixed-gke -f "$f" --set agent.networkPolicy.mode=gke --set agent.networkPolicy.broadEgress=auto
expect guard-egress-fixed-gke 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-agents-egress") | .spec.egress[].ports[].port | select(. == 443)' ""
render guard-kill-switch -f "$f" --set agent.repositoryImages.enabled=false --set agent.networkPolicy.broadEgress=auto \
  --set-json 'agent.repositoryImages.registries=[]' --set agent.repositoryImages.pullSecret= \
  --set agent.repositoryImages.cosignPublicKey=notapem
cm guard-kill-switch source-controller PATCHY_REPOSITORY_IMAGES null

# ---- malformed values fail the render, not the controller -------------------
# Each value below renders a ConfigMap source-controller or a job controller
# refuses at startup (runnerimage.NormalizeEntry, resource.ParseQuantity,
# resolve.ParsePublicKey); under strategy Recreate that is a crash-loop in
# place of the running pod. The schema patterns carry the first two, the
# guard the key's PEM armour.
for entry in ghcr.io ghcr.io/ docker.io/org/* 'ghcr.io/org?/' 'ghcr.io/[ab]/' ghcr.io//x/ ghcr.io/org/app:1/ \
  ghcr.io/org/app@sha256:abc 'ghcr.io/org /' ' ' ghcr.io/org/,ghcr.io; do
  expect_fail "registries entry '$entry'" "agent/repositoryImages/registries/0" \
    -f "$f" --set-json "agent.repositoryImages.registries=[\"$entry\"]"
done
for q in 8GB 8gi '8 Gi' lots; do
  expect_fail "ephemeralStorage '$q'" "agent/repositoryImages/ephemeralStorage" \
    -f "$f" --set-json "agent.repositoryImages.ephemeralStorage=\"$q\""
done
# Stricter than ParseQuantity on purpose: a sign or an exponent parses, but a
# negative size fails every agent Job's creation and nobody sizes a disk as 8e9.
for q in -8Gi +8Gi 8e9; do
  expect_fail "ephemeralStorage '$q'" "agent/repositoryImages/ephemeralStorage" \
    -f "$f" --set-json "agent.repositoryImages.ephemeralStorage=\"$q\""
done
expect_fail "cosign key without PEM armour" "agent.repositoryImages.cosignPublicKey is not a PEM public key" \
  -f "$f" --set agent.repositoryImages.cosignPublicKey=notapem
expect_fail "cosign key alongside allowUnsigned" "agent.repositoryImages.cosignPublicKey is not a PEM public key" \
  -f "$f" --set agent.repositoryImages.cosignPublicKey=notapem --set agent.repositoryImages.allowUnsigned=true
render guard-formats-fixed -f "$f" \
  --set-json 'agent.repositoryImages.registries=["localhost:5000/team","Index.Docker.IO/library/","us-docker.pkg.dev/my-project/agent_images.v2/"]' \
  --set agent.repositoryImages.ephemeralStorage=1.5Gi
cm guard-formats-fixed source-controller PATCHY_REPOSITORY_IMAGE_REGISTRIES \
  localhost:5000/team,Index.Docker.IO/library/,us-docker.pkg.dev/my-project/agent_images.v2/
cm guard-formats-fixed investigation-controller PATCHY_AGENT_EPHEMERAL_STORAGE 1.5Gi

# ---- evaluation controller: off by default, and none of its :9791 plumbing --
# evaluationController.enabled gates its own file AND source-controller's
# internal blob endpoint across four other templates; a half-rendered flag is
# a listener nobody can reach or a client dialling a port nobody opened.
evsrc='select(.kind == "Deployment" and .metadata.name == "patchy-source-controller") | .spec.template.spec'
evsvc='select(.kind == "Service" and .metadata.name == "patchy-source-controller") | .spec.ports[]'
evsrcnp='select(.kind == "NetworkPolicy" and .metadata.name == "patchy-source-controller") | .spec.ingress[]'
ev='select(.kind == "Deployment" and .metadata.name == "patchy-evaluation-controller") | .spec.template'
expect default 'select(.metadata.labels["app.kubernetes.io/name"] == "evaluation-controller") | .kind' ""
cm default source-controller PATCHY_ARTIFACT_INTERNAL_ADDR null
cm default source-controller PATCHY_WORKSPACE_RETENTION null
expect default "$evsrc | .containers[0].ports[] | select(.containerPort == 9791) | .name" ""
expect default "$evsvc | select(.port == 9791) | .name" ""
expect default "$evsrcnp | select(.ports[].port == 9791) | .ports[].port" ""

# ---- evaluation controller on: the Deployment, its identity and its grants --
render eval -f "$fixtures/evaluation-controller.yaml"
expect eval "$ev | .spec.containers[0].image | split(\":\") | .[0]" ghcr.io/devthenet-labs/patchy/evaluation-controller
expect eval "$ev | .spec.serviceAccountName" patchy-evaluation-controller
expect eval 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-evaluation-controller") | .metadata.namespace' patchy
# Every binding names that ServiceAccount, and every Role it references is rendered.
expect eval 'select((.kind == "RoleBinding" or .kind == "ClusterRoleBinding") and .metadata.labels["app.kubernetes.io/name"] == "evaluation-controller") | .roleRef.kind + "/" + .roleRef.name + " <- " + (.subjects[] | .kind + " " + .namespace + "/" + .name)' \
  "ClusterRole/patchy-evaluation-controller-authz <- ServiceAccount patchy/patchy-evaluation-controller
Role/patchy-evaluation-controller <- ServiceAccount patchy/patchy-evaluation-controller
Role/patchy-evaluation-controller-jobs <- ServiceAccount patchy/patchy-evaluation-controller"
expect eval 'select((.kind == "Role" or .kind == "ClusterRole") and .metadata.labels["app.kubernetes.io/name"] == "evaluation-controller") | .kind + "/" + .metadata.name + " " + (.metadata.namespace // "-")' \
  "ClusterRole/patchy-evaluation-controller-authz -
Role/patchy-evaluation-controller patchy
Role/patchy-evaluation-controller-jobs patchy-agents"
evrole='select(.kind == "Role" and .metadata.name == "patchy-evaluation-controller") | .rules[]'
expect eval "$evrole | select(.resources[0] == \"evaluations\") | .verbs | join(\",\")" "create,get,list,watch,delete"
expect eval "$evrole | select(.resources[0] == \"evaluationunits\") | .verbs | join(\",\")" "create,get,list,watch,update,patch"
expect eval "$evrole | select(.resources[0] == \"configmaps\") | .verbs | join(\",\")" "create,get,update"
expect eval 'select(.kind == "Role" and .metadata.name == "patchy-evaluation-controller-jobs") | .rules[] | select(.resources[0] == "jobs") | .verbs | join(",")' \
  "create,get,list,watch,delete"
expect eval 'select(.kind == "ClusterRole" and .metadata.name == "patchy-evaluation-controller-authz") | .rules[] | .resources[0] + " " + (.verbs | join(","))' \
  "subjectaccessreviews create"
expect eval 'select(.kind == "ClusterRole" and .metadata.name == "patchy-evaluations-submitter") | .kind' ""

# ---- evaluation controller on: config and the auth mount agree ---------------
expect eval "$ev | .spec.containers[0].envFrom[0].configMapRef.name" patchy-evaluation-controller-config
cm eval evaluation-controller PATCHY_HARNESSES claude
cm eval evaluation-controller PATCHY_EVOLVE_CLAUDE_IMAGE ghcr.io/bitwise-media-group/evolve-runner-claude:latest
cm eval evaluation-controller PATCHY_BROKER_URL http://patchy-egress-broker.patchy.svc.cluster.local:8080
cm eval evaluation-controller PATCHY_ARTIFACT_UPLOAD_URL http://patchy-source-controller.patchy.svc.cluster.local:9791
cm eval evaluation-controller PATCHY_ARTIFACT_BASE_URL http://patchy-source-controller.patchy.svc.cluster.local:9790
cm eval evaluation-controller PATCHY_MAX_WORKSPACE_BYTES 67108864
cm eval evaluation-controller PATCHY_AUTH_CONFIG /etc/patchy/auth/config.yaml
expect eval "$ev | .spec.containers[0].volumeMounts[] | select(.name == \"auth\") | .mountPath + \" \" + (.readOnly | tostring)" \
  "/etc/patchy/auth true"
expect eval "$ev | .spec.volumes[] | select(.name == \"auth\") | .secret.secretName" patchy-evaluation-auth
expect eval 'select(.kind == "Secret" and .metadata.name == "patchy-evaluation-auth") | .stringData["config.yaml"] | from_yaml | .mode + " " + .oidc.clientID' \
  "oidc evolve"
expect eval "$ev | .metadata.annotations[\"checksum/auth\"] | length" 64
expect eval 'select(.kind == "Service" and .metadata.name == "patchy-evaluation-controller") | .spec.ports[] | .name + " " + (.port | tostring) + " -> " + .targetPort' \
  "http 8080 -> http"
expect eval "$ev | .spec.containers[0].ports[] | select(.name == \"http\") | .containerPort" 8080

# ---- evaluation controller on: source-controller's :9791, end to end ----------
cm eval source-controller PATCHY_ARTIFACT_INTERNAL_ADDR :9791
cm eval source-controller PATCHY_WORKSPACE_RETENTION 168h
expect eval "$evsrc | .containers[0].ports[] | select(.containerPort == 9791) | .name" internal
expect eval "$evsvc | select(.port == 9791) | .name + \" -> \" + .targetPort" "internal -> internal"
expect eval "$evsrcnp | select(.ports[].port == 9791) | .from[].podSelector.matchLabels[\"app.kubernetes.io/name\"]" \
  evaluation-controller
expect eval 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-evaluation-controller") | .spec.egress[] | select(.ports[].port == 9791) | .to[].podSelector.matchLabels["app.kubernetes.io/name"]' \
  source-controller
# the flag changes source-controller's config, so an upgrade that turns the
# evaluation controller on rolls source-controller and opens the listener
evsum='select(.kind == "Deployment" and .metadata.name == "patchy-source-controller") | .spec.template.metadata.annotations["checksum/config"]'
if [ -z "$(get eval "$evsum")" ] || [ "$(get default "$evsum")" = "$(get eval "$evsum")" ]; then
  fail "eval: source-controller's checksum/config did not change, so an upgrade would not open :9791"
fi

# ---- evaluation controller variants ------------------------------------------
# An operator-owned auth Secret: mounted by name, neither rendered nor hashed.
render eval-existing -f "$fixtures/evaluation-controller.yaml" \
  --set evaluationController.auth.config=null --set evaluationController.auth.existingSecret=evals-auth
expect eval-existing 'select(.kind == "Secret") | .metadata.name' ""
expect eval-existing "$ev | .spec.volumes[] | select(.name == \"auth\") | .secret.secretName" evals-auth
expect eval-existing "$ev | .metadata.annotations[\"checksum/auth\"]" null
# A bring-your-own ServiceAccount: not rendered, but still what runs and binds.
render eval-own-sa -f "$fixtures/evaluation-controller.yaml" \
  --set evaluationController.serviceAccount.create=false --set evaluationController.serviceAccount.name=evals
expect eval-own-sa 'select(.kind == "ServiceAccount" and .metadata.labels["app.kubernetes.io/name"] == "evaluation-controller") | .metadata.name' ""
expect eval-own-sa "$ev | .spec.serviceAccountName" evals
expect eval-own-sa 'select(.kind == "RoleBinding" and .metadata.labels["app.kubernetes.io/name"] == "evaluation-controller") | .subjects[].name' \
  "evals
evals"
# claude enabled only on the evaluation fleet still deploys the broker.
render eval-broker -f "$fixtures/evaluation-controller.yaml" \
  --set agent.runners.claude.enabled=false --set agent.runners.codex.enabled=true
expect eval-broker 'select(.kind == "Deployment" and .metadata.name == "patchy-egress-broker") | .kind' Deployment
cm eval-broker evaluation-controller PATCHY_BROKER_URL http://patchy-egress-broker.patchy.svc.cluster.local:8080
# A non-brokered evaluation runner reuses agent.runners.<harness>'s Secret.
render eval-codex -f "$fixtures/evaluation-controller.yaml" \
  --set evaluationController.runners.claude.enabled=false --set evaluationController.runners.codex.enabled=true
cm eval-codex evaluation-controller PATCHY_HARNESSES codex
cm eval-codex evaluation-controller PATCHY_CODEX_SECRET patchy-openai
cm eval-codex evaluation-controller PATCHY_CODEX_SECRET_ENV OPENAI_API_KEY
cm eval-codex evaluation-controller PATCHY_BROKER_URL null
# Both exposure flavours and the example submitter tier.
render eval-exposed -f "$fixtures/evaluation-controller.yaml" \
  --set evaluationController.host=patchy-evals.example.com --set evaluationController.ingress.enabled=true \
  --set evaluationController.httpRoute.enabled=true --set evaluationController.rbac.userRoles=true
expect eval-exposed 'select(.kind == "Ingress" and .metadata.name == "patchy-evaluation-controller") | .spec.rules[0].host + " " + .spec.rules[0].http.paths[0].backend.service.name' \
  "patchy-evals.example.com patchy-evaluation-controller"
expect eval-exposed 'select(.kind == "HTTPRoute" and .metadata.name == "patchy-evaluation-controller") | .spec.hostnames[0] + " " + .spec.rules[0].backendRefs[0].name' \
  "patchy-evals.example.com patchy-evaluation-controller"
expect eval-exposed 'select(.kind == "ClusterRole" and .metadata.name == "patchy-evaluations-submitter") | .rules[0].verbs | join(",")' \
  "create,get,delete"
# Without its NetworkPolicy the component still renders; the :9791 ingress
# on source-controller is keyed on the component, not on this flag.
render eval-no-np -f "$fixtures/evaluation-controller.yaml" --set evaluationController.networkPolicy.create=false
expect eval-no-np 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-evaluation-controller") | .kind' ""
expect eval-no-np "$ev | .spec.serviceAccountName" patchy-evaluation-controller
expect eval-no-np "$evsrcnp | select(.ports[].port == 9791) | .from[].podSelector.matchLabels[\"app.kubernetes.io/name\"]" \
  evaluation-controller

# ---- evaluation controller guards --------------------------------------------
ef=$fixtures/evaluation-controller.yaml
expect_fail "eval without auth" "evaluationController requires auth configuration" \
  --set evaluationController.enabled=true
expect_fail "eval with both auth sources" \
  "evaluationController.auth.existingSecret and evaluationController.auth.config are mutually exclusive" \
  -f "$ef" --set evaluationController.auth.existingSecret=evals-auth
expect_fail "eval without a runner" "evaluationController.enabled requires at least one evaluationController.runners" \
  -f "$ef" --set evaluationController.runners.claude.enabled=false
expect_fail "eval ingress without host" "evaluationController.host is required when evaluationController.ingress is enabled" \
  -f "$ef" --set evaluationController.ingress.enabled=true
expect_fail "eval httpRoute without host" "evaluationController.host is required when evaluationController.httpRoute is enabled" \
  -f "$ef" --set evaluationController.httpRoute.enabled=true

# ---- a harness enabled only for evaluations keeps its egress policy ----------
# Evaluation Jobs carry the same harness label as finding Jobs, so a harness
# enabled on either fleet needs its per-harness policy: without one a Cilium
# or GKE pod is default-denied its model API, and an Istio pod has no
# REGISTRY_ONLY Sidecar beside the base policy's TCP 443 to anywhere.
hnp='select(.kind == "CiliumNetworkPolicy" or .kind == "FQDNNetworkPolicy" or .kind == "Sidecar" or .kind == "ServiceEntry") | .kind + "/" + .metadata.name'
render eval-codex-cilium -f "$ef" --set agent.networkPolicy.mode=cilium --set evaluationController.runners.codex.enabled=true
expect eval-codex-cilium "$hnp" "CiliumNetworkPolicy/patchy-agent-egress-claude
CiliumNetworkPolicy/patchy-agent-egress-codex"
expect eval-codex-cilium 'select(.kind == "CiliumNetworkPolicy" and .metadata.name == "patchy-agent-egress-codex") | .spec.endpointSelector.matchLabels["patchy.bitwisemedia.uk/harness"]' codex
render eval-codex-gke -f "$ef" --set agent.networkPolicy.mode=gke --set evaluationController.runners.codex.enabled=true
expect eval-codex-gke "$hnp" "FQDNNetworkPolicy/patchy-agent-egress-codex"
render eval-claude-istio -f "$ef" --set agent.networkPolicy.mode=istio \
  --set agent.runners.claude.enabled=false --set agent.runners.codex.enabled=true
expect eval-claude-istio "$hnp" "ServiceEntry/patchy-agent-codex
Sidecar/patchy-agent-egress-claude
Sidecar/patchy-agent-egress-codex"
# ...and a harness enabled nowhere gets none: the evaluation fleet's runner
# flags mean nothing while the controller itself is off.
render eval-off-cilium --set agent.networkPolicy.mode=cilium --set evaluationController.runners.codex.enabled=true
expect eval-off-cilium "$hnp" "CiliumNetworkPolicy/patchy-agent-egress-claude"

# ---- ...and the install NOTES name its model Secret -------------------------
# A non-brokered harness is enabled only when its credential Secret exists in
# the agent namespace, whichever fleet runs it, so the NOTES' list of Secrets
# to create follows the same rule as the egress policies above.
codexsecret="patchy-openai (key api-key, as OPENAI_API_KEY)"
notes notes-eval-codex -f "$ef" \
  --set evaluationController.runners.claude.enabled=false --set evaluationController.runners.codex.enabled=true
notes_has notes-eval-codex "$codexsecret" yes
notes_has notes-eval-codex "patchy-copilot" no
notes notes-codex --set agent.runners.codex.enabled=true
notes_has notes-codex "$codexsecret" yes
notes notes-eval-off --set evaluationController.runners.codex.enabled=true
notes_has notes-eval-off "patchy-openai" no

# ---- intent controller: off by default --------------------------------------
it='select(.kind == "Deployment" and .metadata.name == "patchy-intent-controller") | .spec.template'
icm='select(.kind == "ConfigMap" and .metadata.name == "patchy-intent-controller-config") | .data'
expect default 'select(.metadata.labels["app.kubernetes.io/name"] == "intent-controller") | .kind' ""
expect default "select(.kind == \"ConfigMap\") | (.data // {}) | keys | .[] | select(test(\"^PATCHY_INTENT_\"))" ""

# ---- intent controller on: the Deployment, its identity and its grants ------
ifx=$fixtures/intent-controller.yaml
render intent -f "$ifx"
expect intent "$it | .spec.containers[0].image | split(\":\") | .[0]" ghcr.io/devthenet-labs/patchy/intent-controller
expect intent "$it | .spec.serviceAccountName" patchy-intent-controller
expect intent "$it | .spec.containers[0].envFrom[] | .configMapRef.name" patchy-intent-controller-config
expect intent "$it | .spec.containers[0].ports[] | .name + \" \" + (.containerPort | tostring)" "health 8081"
expect intent "$it | .metadata.labels[\"app.kubernetes.io/component\"]" controller
expect intent 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-intent-controller") | .metadata.namespace' patchy
# Every binding names that ServiceAccount, every Role it references is
# rendered, and there is no ClusterRole: the design's tightest posture.
expect intent 'select((.kind == "RoleBinding" or .kind == "ClusterRoleBinding") and .metadata.labels["app.kubernetes.io/name"] == "intent-controller") | .roleRef.kind + "/" + .roleRef.name + " <- " + (.subjects[] | .kind + " " + .namespace + "/" + .name)' \
  "Role/patchy-intent-controller <- ServiceAccount patchy/patchy-intent-controller
Role/patchy-intent-controller-jobs <- ServiceAccount patchy/patchy-intent-controller"
expect intent 'select((.kind == "Role" or .kind == "ClusterRole") and .metadata.labels["app.kubernetes.io/name"] == "intent-controller") | .kind + "/" + .metadata.name + " " + (.metadata.namespace // "-")' \
  "Role/patchy-intent-controller patchy
Role/patchy-intent-controller-jobs patchy-agents"
# The release-namespace Role, rule by rule: exactly the verbs the engine uses.
irole='select(.kind == "Role" and .metadata.name == "patchy-intent-controller") | .rules[]'
expect intent "$irole | (.resources | join(\",\")) + \" \" + (.verbs | join(\",\"))" \
  "projects get,list,watch
projects/status update
intents create,get,list,watch,delete
intents/status,intents/finalizers update
intentruns create,get,list,watch,update
intentruns/status,intentruns/finalizers update
repositories create,get,list,watch,delete
forges get,list,watch
configmaps create,get,list,watch,update
secrets get
leases get,create,update
events create,patch"
# secrets get only on the named Forge Secrets; no other rule names secrets.
expect intent "$irole | select(.resources[0] == \"secrets\") | .resourceNames | join(\",\")" \
  "patchy-github,patchy-github-enterprise"
expect intent "$irole | select(.resourceNames == null) | .resources[] | select(. == \"secrets\")" ""
# The agents-namespace Role is a copy of the shared agent-jobs Role.
if [ -z "$(get intent 'select(.kind == "Role" and .metadata.name == "patchy-intent-controller-jobs") | .rules')" ] ||
  [ "$(get intent 'select(.kind == "Role" and .metadata.name == "patchy-intent-controller-jobs") | .rules')" != \
    "$(get intent 'select(.kind == "Role" and .metadata.name == "patchy-agent-jobs") | .rules')" ]; then
  fail "intent: patchy-intent-controller-jobs is not a copy of the agent-jobs Role"
fi
# NetworkPolicy: probes in; DNS and TCP 443/6443 out, like the job controllers.
inp='select(.kind == "NetworkPolicy" and .metadata.name == "patchy-intent-controller") | .spec'
expect intent "$inp | .ingress[].ports[] | .protocol + \"/\" + (.port | tostring)" "TCP/8081"
expect intent "$inp | .egress[].ports[] | .protocol + \"/\" + (.port | tostring)" "UDP/53
TCP/53
TCP/443
TCP/6443"

# ---- intent controller on: its ConfigMap holds only what it binds -----------
# Brokered claude only, whatever agent.runners enables for findings.
cm intent intent-controller PATCHY_HARNESSES claude
expect intent "$icm | .PATCHY_CLAUDE_AGENT_IMAGE | split(\":\") | .[0]" ghcr.io/devthenet-labs/patchy/claude-agent-runner
cm intent intent-controller PATCHY_BROKER_URL http://patchy-egress-broker.patchy.svc.cluster.local:8080
cm intent intent-controller PATCHY_CLAUDE_PROVIDER anthropic
cm intent intent-controller PATCHY_AGENT_NAMESPACE patchy-agents
cm intent intent-controller PATCHY_AGENT_SERVICE_ACCOUNT patchy-agent
cm intent intent-controller PATCHY_JOB_TTL 1h
# The finding job controllers' keys stay theirs: the intent Job deadline is
# its own, it runs no other harness, and it has no model allowlist.
for key in PATCHY_JOB_DEADLINE PATCHY_MODEL_ALLOWLIST PATCHY_CODEX_AGENT_IMAGE PATCHY_COPILOT_AGENT_IMAGE \
  PATCHY_INVESTIGATE_MODEL PATCHY_REMEDIATE_MODEL PATCHY_MAX_ATTEMPTS PATCHY_LISTEN_ADDR \
  PATCHY_REPOSITORY_IMAGES PATCHY_AGENT_EPHEMERAL_STORAGE PATCHY_CHANGESET_MAX_ENTRIES; do
  cm intent intent-controller "$key" null
done
# Its own keys equal the kustomize component's, which
# cmd/intent-controller/serve_test.go holds to the binary's flags and
# defaults — so every chart key is one the binary binds.
component=deploy/kustomize/components/intent-controller/configmap.yaml
ckeys=$(yq '.data | keys | .[] | select(test("^PATCHY_INTENT_"))' "$component" | sort)
expect intent "$icm | keys | .[] | select(test(\"^PATCHY_INTENT_\"))" "$ckeys"
for key in $ckeys; do
  cm intent intent-controller "$key" "$(yq ".data.$key" "$component")"
done
# The finding controllers are not touched by the flag...
for c in integration-controller source-controller context-controller investigation-controller remediation-controller; do
  csum="select(.kind == \"Deployment\" and .metadata.name == \"patchy-$c\") | .spec.template.metadata.annotations[\"checksum/config\"]"
  if [ "$(get default "$csum")" != "$(get intent "$csum")" ]; then
    fail "intent: enabling the intent controller changed $c's config"
  fi
done
# ...and a config change rolls the intent controller.
render intent-tuned -f "$ifx" --set intentController.config.plan.maxTurns=10 \
  --set intentController.config.intentTTL=0s --set intentController.config.rateLimitFloor=0 \
  --set intentController.config.logLevel=debug --set intentController.config.extra.PATCHY_INTENT_POLL_INTERVAL=2m
cm intent-tuned intent-controller PATCHY_INTENT_PLAN_MAX_TURNS 10
cm intent-tuned intent-controller PATCHY_INTENT_TTL 0s
cm intent-tuned intent-controller PATCHY_INTENT_RATE_LIMIT_FLOOR 0
cm intent-tuned intent-controller PATCHY_LOG_LEVEL debug
cm intent-tuned intent-controller PATCHY_INTENT_POLL_INTERVAL 2m
# Multi-repository intents are off unless asked for, and the flag rolls the
# controller like any other setting.
cm intent intent-controller PATCHY_INTENT_MULTI_REPO false
render intent-multi -f "$ifx" --set intentController.config.multiRepo=true
cm intent-multi intent-controller PATCHY_INTENT_MULTI_REPO true
if [ "$(get intent "$it | .metadata.annotations[\"checksum/config\"]")" = \
  "$(get intent-multi "$it | .metadata.annotations[\"checksum/config\"]")" ]; then
  fail "intent-multi: checksum/config did not change, so turning multiRepo on would not roll the controller"
fi
if [ "$(get intent "$it | .metadata.annotations[\"checksum/config\"]")" = \
  "$(get intent-tuned "$it | .metadata.annotations[\"checksum/config\"]")" ]; then
  fail "intent-tuned: checksum/config did not change, so an upgrade would not roll the controller"
fi

# ---- intent controller on: repository images reach it -----------------------
render intent-ri -f "$ifx" -f "$fixtures/repository-images.yaml"
cm intent-ri intent-controller PATCHY_REPOSITORY_IMAGES true
cm intent-ri intent-controller PATCHY_AGENT_EPHEMERAL_STORAGE 8Gi
cm intent-ri intent-controller PATCHY_CHANGESET_MAX_ENTRIES 500
cm intent-ri intent-controller PATCHY_REPOSITORY_IMAGE_REGISTRIES null
cm intent-ri intent-controller DOCKER_CONFIG null
# The global egress proxy reaches it too: it talks to GitHub.
render intent-proxy -f "$ifx" --set proxy.httpsProxy=http://proxy.example.com:3128
cm intent-proxy intent-controller HTTPS_PROXY http://proxy.example.com:3128
cm intent-proxy intent-controller NO_PROXY localhost,127.0.0.1,.svc,.cluster.local

# ---- agent resources: rendered only where bound, and only when set -----------
# Unset (the default), no ConfigMap carries a CPU, memory or class key, so
# every agent Job, and every checksum/config, is what it was before the
# setting existed.
resource_keys="PATCHY_AGENT_CPU_REQUEST PATCHY_AGENT_MEMORY_REQUEST PATCHY_AGENT_CPU_LIMIT PATCHY_AGENT_MEMORY_LIMIT
PATCHY_INTENT_RESOURCE_CLASSES"
render res-off -f "$ifx" -f "$ef"
for key in $resource_keys; do
  expect default "select(.kind == \"ConfigMap\") | .data.$key | select(. != null)" ""
  expect res-off "select(.kind == \"ConfigMap\") | .data.$key | select(. != null)" ""
done
# The default reaches every controller that launches agent Jobs, its
# unquoted quantities as integers (never helm's 1.073741824e+09), and the
# classes reach intent-controller alone, as JSON whose unquoted cpu stays a
# number (resourceclass.Parse reads it as the quantity 4).
render res -f "$ifx" -f "$ef" -f "$fixtures/agent-resources.yaml"
for c in investigation-controller remediation-controller intent-controller evaluation-controller; do
  cm res "$c" PATCHY_AGENT_CPU_REQUEST 0.5
  cm res "$c" PATCHY_AGENT_MEMORY_REQUEST 1073741824
  cm res "$c" PATCHY_AGENT_CPU_LIMIT 2
  cm res "$c" PATCHY_AGENT_MEMORY_LIMIT 4Gi
done
cm res intent-controller PATCHY_INTENT_RESOURCE_CLASSES \
  '{"large":{"limits":{"memory":"10Gi"},"requests":{"cpu":4,"memory":"8Gi"}},"medium":{"limits":{"cpu":"3","memory":"4Gi"},"requests":{"cpu":"1500m","memory":"3Gi"}}}'
for c in investigation-controller remediation-controller evaluation-controller; do
  cm res "$c" PATCHY_INTENT_RESOURCE_CLASSES null
done
for c in integration-controller source-controller context-controller egress-broker status-server; do
  for key in $resource_keys; do
    cm res "$c" "$key" null
  done
done
# Setting the default rolls exactly the four controllers that bind it;
# setting only the classes rolls intent-controller alone.
render res-classes -f "$ifx" -f "$ef" \
  --set-json 'agent.resources.classes={"large":{"requests":{"cpu":4,"memory":"8Gi"},"limits":{"memory":"10Gi"}}}'
for c in integration-controller source-controller context-controller investigation-controller remediation-controller \
  intent-controller evaluation-controller; do
  csum="select(.kind == \"Deployment\" and .metadata.name == \"patchy-$c\") | .spec.template.metadata.annotations[\"checksum/config\"]"
  off=$(get res-off "$csum")
  case $c in
  investigation-controller | remediation-controller | intent-controller | evaluation-controller)
    if [ "$off" = "$(get res "$csum")" ]; then
      fail "res: $c's checksum/config did not change, so setting agent.resources.default would not roll it"
    fi
    ;;
  *)
    if [ "$off" != "$(get res "$csum")" ]; then
      fail "res: agent.resources changed $c's config, which binds none of it"
    fi
    ;;
  esac
  if [ "$c" = intent-controller ]; then
    if [ "$off" = "$(get res-classes "$csum")" ]; then
      fail "res-classes: intent-controller's checksum/config did not change, so new classes would not roll it"
    fi
  elif [ "$off" != "$(get res-classes "$csum")" ]; then
    fail "res-classes: agent.resources.classes changed $c's config"
  fi
done
# The schema refuses what the controllers would refuse at startup, where it
# can say so: the wrong key (ephemeral storage lives under repositoryImages),
# a zero, negative or empty quantity, a key other than cpu and memory, a class
# without the quantities every class needs, a name no Project could pick, and
# more classes than intent-controller accepts.
expect_fail "agent.resources.ephemeralStorage" "additional properties 'ephemeralStorage' not allowed" \
  --set agent.resources.ephemeralStorage=8Gi
expect_fail "a zero cpu" "/agent/resources/default/requests/cpu" --set agent.resources.default.requests.cpu=0
expect_fail "a negative cpu" "/agent/resources/default/requests/cpu" \
  --set-string agent.resources.default.requests.cpu=-1
expect_fail "an empty memory limit" "/agent/resources/default/limits/memory" \
  --set-string agent.resources.default.limits.memory=
expect_fail "a gpu request" "additional properties 'gpu' not allowed" --set agent.resources.default.requests.gpu=1
expect_fail "a class with no memory limit" "/agent/resources/classes/large/limits" \
  --set-json 'agent.resources.classes={"large":{"requests":{"cpu":4,"memory":"8Gi"},"limits":{}}}'
expect_fail "a class with no memory request" "/agent/resources/classes/large/requests" \
  --set-json 'agent.resources.classes={"large":{"requests":{"cpu":4},"limits":{"memory":"8Gi"}}}'
expect_fail "a class name that is not a DNS label" "invalid propertyName 'Large'" \
  --set-json 'agent.resources.classes={"Large":{"requests":{"cpu":4,"memory":"8Gi"},"limits":{"memory":"8Gi"}}}'
seventeen=$(for i in $(seq 1 17); do printf '"c%s":{"requests":{"cpu":1,"memory":"1Gi"},"limits":{"memory":"1Gi"}},' "$i"; done)
expect_fail "seventeen classes" "/agent/resources/classes" --set-json "agent.resources.classes={${seventeen%,}}"

# ---- agent DNS: none closes the channel, cluster leaves everything alone ----
# Unset or cluster (the default), no ConfigMap carries PATCHY_AGENT_DNS and the
# agent egress keeps its DNS rule, byte for byte the render before the value
# existed. none reaches exactly the four controllers that launch agent Jobs and
# drops every DNS rule the chart renders for agent pods, in each dialect, while
# the controllers (which resolve the pods' names for them) keep theirs.
agentdns='select(.metadata.namespace == "patchy-agents" and (.kind == "NetworkPolicy" or .kind == "CiliumNetworkPolicy")) | .spec.egress[] | select((.ports // []) + (.toPorts[0].ports // []) | map(.port | tostring) | contains(["53"])) | .to // .toEndpoints | to_json(0)'
expect default "select(.kind == \"ConfigMap\") | .data.PATCHY_AGENT_DNS | select(. != null)" ""
expect default "$agentdns" '[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"kube-system"}}}]'
render dns-cluster --set agent.networkPolicy.dns=cluster
if ! cmp -s "$out/default.yaml" "$out/dns-cluster.yaml"; then
  fail "dns-cluster: agent.networkPolicy.dns=cluster changed the default render"
fi
render dns-none-off -f "$ifx" -f "$ef"
render dns-none -f "$ifx" -f "$ef" --set agent.networkPolicy.dns=none --set clusterDNSCIDR=10.100.0.10/32
for c in investigation-controller remediation-controller intent-controller evaluation-controller; do
  cm dns-none "$c" PATCHY_AGENT_DNS none
  csum="select(.kind == \"Deployment\" and .metadata.name == \"patchy-$c\") | .spec.template.metadata.annotations[\"checksum/config\"]"
  if [ "$(get dns-none-off "$csum")" = "$(get dns-none "$csum")" ]; then
    fail "dns-none: $c's checksum/config did not change, so dns: none would not roll it"
  fi
done
for c in integration-controller source-controller context-controller egress-broker status-server; do
  cm dns-none "$c" PATCHY_AGENT_DNS null
done
expect dns-none "$agentdns" ""
expect dns-none 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-agents-egress") | .spec.egress[].ports[].port' "9790
8080
443"
expect dns-none "$nodedns | select(test(\"agents\"))" ""
expect dns-none "$nodedns | select(test(\"investigation|remediation|intent|evaluation\"))" "patchy/patchy-evaluation-controller
patchy/patchy-intent-controller
patchy/patchy-investigation-controller
patchy/patchy-remediation-controller"
if [ "$(get dns-none-off "$npsel")" != "$(get dns-none "$npsel")" ]; then
  fail "dns-none: agent.networkPolicy.dns changed which pods a NetworkPolicy selects"
fi
render dns-none-cilium -f "$ifx" -f "$ef" --set agent.networkPolicy.dns=none --set agent.networkPolicy.mode=cilium
expect dns-none-cilium "$agentdns" ""
expect dns-none-cilium "$hnp" "CiliumNetworkPolicy/patchy-agent-egress-claude"
expect dns-none-cilium 'select(.kind == "CiliumNetworkPolicy" and .metadata.name == "patchy-agent-egress-claude") | .spec.egress[].toPorts[].ports[].port' '9790
8080'
render dns-none-gke --set agent.networkPolicy.dns=none --set agent.networkPolicy.mode=gke
expect dns-none-gke "$agentdns" ""
expect dns-none-gke "$hnp" ""
expect dns-none-gke 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-agents-egress") | .spec.egress[].ports[].port' "9790
8080"
# Without the chart's agent policies the pods still lose their resolver.
render dns-none-no-np --set agent.networkPolicy.dns=none --set agent.networkPolicy.create=false
cm dns-none-no-np investigation-controller PATCHY_AGENT_DNS none
notes notes-dns-none --set agent.networkPolicy.dns=none
notes_has notes-dns-none "Agent DNS: none" yes
notes_has notes-dns-none "broadEgress: never" yes
notes notes-dns-none-narrow --set agent.networkPolicy.dns=none --set agent.networkPolicy.broadEgress=never
notes_has notes-dns-none-narrow "Agent DNS: none" yes
notes_has notes-dns-none-narrow "broadEgress: never" no
notes notes-dns-cluster
notes_has notes-dns-cluster "Agent DNS: none" no
# Only brokered claude can run without a resolver, and an Istio sidecar must
# resolve istiod; a mode the schema does not know is refused.
expect_fail "dns none with codex" "the codex runner dials its model API by name" \
  --set agent.networkPolicy.dns=none --set agent.runners.codex.enabled=true
expect_fail "dns none with copilot on the evaluation fleet" "the copilot runner dials its model API by name" \
  -f "$ef" --set agent.networkPolicy.dns=none --set evaluationController.runners.copilot.enabled=true
expect_fail "dns none under istio" "incompatible with mode istio" \
  --set agent.networkPolicy.dns=none --set agent.networkPolicy.mode=istio
expect_fail "dns none under the legacy istio switch" "incompatible with mode istio" \
  --set agent.networkPolicy.dns=none --set agent.networkPolicy.istio.enabled=true
expect_fail "an unknown dns mode" "/agent/networkPolicy/dns" --set agent.networkPolicy.dns=off
render dns-none-fixed --set agent.networkPolicy.dns=none --set agent.runners.codex.enabled=false

# ---- intent controller on: claude everywhere it runs ------------------------
# Intents run on claude even when the finding fleet does not: the broker
# deploys, the agent egress admits it, and each egress dialect keeps a claude
# policy for the intent pods.
render intent-codex -f "$ifx" --set agent.runners.claude.enabled=false --set agent.runners.codex.enabled=true
expect intent-codex 'select(.kind == "Deployment" and .metadata.name == "patchy-egress-broker") | .kind' Deployment
expect intent-codex 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-agents-egress") | .spec.egress[].to[].podSelector.matchLabels["app.kubernetes.io/name"] | select(. == "egress-broker")' \
  egress-broker
cm intent-codex intent-controller PATCHY_HARNESSES claude
cm intent-codex investigation-controller PATCHY_HARNESSES codex
render intent-codex-cilium -f "$ifx" --set agent.networkPolicy.mode=cilium \
  --set agent.runners.claude.enabled=false --set agent.runners.codex.enabled=true
expect intent-codex-cilium "$hnp" "CiliumNetworkPolicy/patchy-agent-egress-claude
CiliumNetworkPolicy/patchy-agent-egress-codex"
render intent-codex-istio -f "$ifx" --set agent.networkPolicy.mode=istio \
  --set agent.runners.claude.enabled=false --set agent.runners.codex.enabled=true
expect intent-codex-istio "$hnp" "ServiceEntry/patchy-agent-codex
Sidecar/patchy-agent-egress-claude
Sidecar/patchy-agent-egress-codex"
# ...and with the controller off, claude disabled for findings is claude off.
render intent-off-codex --set agent.runners.claude.enabled=false --set agent.runners.codex.enabled=true
expect intent-off-codex 'select(.kind == "Deployment" and .metadata.name == "patchy-egress-broker") | .kind' ""

# ---- intent controller variants ----------------------------------------------
# A bring-your-own ServiceAccount: not rendered, but still what runs and binds.
render intent-own-sa -f "$ifx" --set intentController.serviceAccount.create=false \
  --set intentController.serviceAccount.name=intents
expect intent-own-sa 'select(.kind == "ServiceAccount" and .metadata.labels["app.kubernetes.io/name"] == "intent-controller") | .metadata.name' ""
expect intent-own-sa "$it | .spec.serviceAccountName" intents
expect intent-own-sa 'select(.kind == "RoleBinding" and .metadata.labels["app.kubernetes.io/name"] == "intent-controller") | .subjects[].name' \
  "intents
intents"
# Without its NetworkPolicy the component still renders.
render intent-no-np -f "$ifx" --set intentController.networkPolicy.create=false
expect intent-no-np "$inp | .podSelector" ""
expect intent-no-np "$it | .spec.serviceAccountName" patchy-intent-controller
render intent-extra-egress -f "$ifx" \
  --set-json 'intentController.networkPolicy.extraEgress=[{"ports":[{"protocol":"TCP","port":3128}]}]'
expect intent-extra-egress "$inp | .egress[].ports[] | select(.port == 3128) | .protocol" TCP

# ---- intent controller guards -------------------------------------------------
# An empty resourceNames list grants every Secret, so it must never render:
# the schema refuses it, and the template refuses it again when the schema
# is skipped.
expect_fail "intent without forge secrets" "missing property 'forgeSecrets'" \
  -f "$ifx" --set-json 'intentController.forgeSecrets=null'
expect_fail "intent with an empty forge secret list" "intentController/forgeSecrets" \
  -f "$ifx" --set-json 'intentController.forgeSecrets=[]'
expect_fail "intent without forge secrets, schema skipped" \
  "intentController.enabled requires intentController.forgeSecrets" \
  -f "$ifx" --set-json 'intentController.forgeSecrets=[]' --skip-schema-validation
expect_fail "intent with a malformed forge secret" "intentController/forgeSecrets/0" \
  -f "$ifx" --set-json 'intentController.forgeSecrets=["Not A Name"]'
expect_fail "intent with a unitless TTL" "intentController/config/intentTTL" \
  -f "$ifx" --set intentController.config.intentTTL=0
expect_fail "intent with a zero-turn stage" "intentController/config/build/maxTurns" \
  -f "$ifx" --set intentController.config.build.maxTurns=0

# ---- ...and the install NOTES say what it still needs ------------------------
notes notes-intent -f "$ifx"
notes_has notes-intent "intent-controller is enabled" yes
notes_has notes-intent "patchy-github, patchy-github-enterprise" yes
notes_has notes-intent "every intent build blocks on" yes
notes notes-intent-ri -f "$ifx" -f "$fixtures/repository-images.yaml"
notes_has notes-intent-ri "every intent build blocks on" no
notes notes-intent-off
notes_has notes-intent-off "intent-controller" no

# ---- egress broker limits ---------------------------------------------------
render limits -f "$fixtures/broker-limits.yaml"
cm limits egress-broker PATCHY_REQUESTS_PER_POD 2000
cm limits egress-broker PATCHY_CONCURRENT_PER_POD 4
cm limits egress-broker PATCHY_CONCURRENCY_WAIT -1s
cm limits egress-broker PATCHY_TOKENS_PER_POD 30000000
cm limits egress-broker PATCHY_TOKENS_PER_HOUR 100000000
cm limits egress-broker PATCHY_MAX_TOKENS_CEILING 64000
cm limits egress-broker PATCHY_MODEL_ALLOWLIST anthropic/claude-sonnet-5,anthropic/claude-opus-5
cm limits egress-broker PATCHY_BETA_DENYLIST none
cm limits egress-broker PATCHY_MAX_ANTHROPIC_REQUEST_BYTES 4194304
cm limits egress-broker PATCHY_MAX_REQUEST_BYTES 20971520
cm limits egress-broker PATCHY_PREAUTH_REQUESTS_PER_SECOND 2.5
cm limits egress-broker PATCHY_PREAUTH_BURST 100
cm limits egress-broker PATCHY_TOKEN_REVIEWS_PER_SECOND 20
# The token unit renders no unit and no Role, so the fixture above (unit
# unset) and an explicit tokens agree.
expect limits 'select(.metadata.name == "patchy-egress-broker-budgets") | .kind' ""
render limits-tokens -f "$fixtures/broker-limits.yaml" --set egressBroker.limits.unit=tokens
if ! cmp -s "$out/limits.yaml" "$out/limits-tokens.yaml"; then
  fail "limits-tokens: an explicit egressBroker.limits.unit tokens changed the render"
fi
# The cost unit: the micro-USD limits (integers past float notation), and a
# Role granting get on pods in the agent namespace, nothing more.
render limits-cost -f "$fixtures/broker-limits.yaml" --set egressBroker.limits.unit=cost \
  --set egressBroker.limits.costPerPod=60000000 --set egressBroker.limits.costPerHour=200000000
cm limits-cost egress-broker PATCHY_LIMIT_UNIT cost
cm limits-cost egress-broker PATCHY_COST_PER_POD 60000000
cm limits-cost egress-broker PATCHY_COST_PER_HOUR 200000000
cm limits-cost egress-broker PATCHY_TOKENS_PER_POD 30000000
expect limits-cost 'select(.kind == "Role" and .metadata.name == "patchy-egress-broker-budgets") | .metadata.namespace' patchy-agents
expect limits-cost 'select(.kind == "Role" and .metadata.name == "patchy-egress-broker-budgets") | .rules | length' 1
expect limits-cost 'select(.kind == "Role" and .metadata.name == "patchy-egress-broker-budgets") | .rules[0] | (.apiGroups + .resources + .verbs) | join(",")' ',pods,get'
expect limits-cost 'select(.kind == "RoleBinding" and .metadata.name == "patchy-egress-broker-budgets") | .subjects[0].name' patchy-egress-broker
render limits-cost-pod -f "$fixtures/broker-limits.yaml" --set egressBroker.limits.unit=cost \
  --set egressBroker.limits.costPerPod=60000000
cm limits-cost-pod egress-broker PATCHY_COST_PER_HOUR null
expect_fail cost-without-pod "needs egressBroker.limits.costPerPod" --set egressBroker.limits.unit=cost
expect_fail cost-under-tokens "need egressBroker.limits.unit cost" --set egressBroker.limits.costPerPod=5
expect_fail cost-hour-under-tokens "need egressBroker.limits.unit cost" --set egressBroker.limits.costPerHour=5
expect_fail unknown-unit "/egressBroker/limits/unit" --set egressBroker.limits.unit=usd
# Run budgets (LF2): with the intent controller on, the token unit renders no
# budget key and no budget policy; the cost unit tells intent-controller to
# grant budgets (and the hourly limit, for its Project check) and renders the
# three budget admission policies, each with its binding.
render limits-intent -f "$fixtures/broker-limits.yaml" -f "$fixtures/intent-controller.yaml"
cm limits-intent intent-controller PATCHY_BROKER_LIMIT_UNIT null
cm limits-intent intent-controller PATCHY_BROKER_COST_PER_HOUR null
cm limits-intent intent-controller PATCHY_INTENT_JOB_DEADLINE_MAX 4h30m
expect limits-intent 'select(.metadata.name // "" | test("^patchy-budget-")) | .kind' ""
render limits-intent-cost -f "$fixtures/broker-limits.yaml" -f "$fixtures/intent-controller.yaml" \
  --set egressBroker.limits.unit=cost --set egressBroker.limits.costPerPod=60000000 \
  --set egressBroker.limits.costPerHour=200000000
cm limits-intent-cost intent-controller PATCHY_BROKER_LIMIT_UNIT cost
cm limits-intent-cost intent-controller PATCHY_BROKER_COST_PER_HOUR 200000000
for p in budget-jobs budget-pods budget-runs; do
  expect limits-intent-cost "select(.kind == \"ValidatingAdmissionPolicy\" and .metadata.name == \"patchy-$p\") | .spec.failurePolicy" Fail
  expect limits-intent-cost "select(.kind == \"ValidatingAdmissionPolicyBinding\" and .metadata.name == \"patchy-$p\") | .spec.validationActions | join(\",\")" Deny
done
expect limits-intent-cost 'select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == "patchy-budget-jobs") | .spec.matchResources.namespaceSelector.matchLabels["kubernetes.io/metadata.name"]' patchy-agents
expect limits-intent-cost 'select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == "patchy-budget-runs") | .spec.matchResources.namespaceSelector.matchLabels["kubernetes.io/metadata.name"]' patchy
render limits-intent-cost-pod -f "$fixtures/broker-limits.yaml" -f "$fixtures/intent-controller.yaml" \
  --set egressBroker.limits.unit=cost --set egressBroker.limits.costPerPod=60000000
cm limits-intent-cost-pod intent-controller PATCHY_BROKER_LIMIT_UNIT cost
cm limits-intent-cost-pod intent-controller PATCHY_BROKER_COST_PER_HOUR null
# Without the intent controller nothing grants budgets, so no policy renders.
expect limits-cost 'select(.metadata.name // "" | test("^patchy-budget-")) | .kind' ""
render limits-extra -f "$fixtures/broker-limits.yaml" --set egressBroker.config.extra.PATCHY_TOKENS_PER_POD=7
cm limits-extra egress-broker PATCHY_TOKENS_PER_POD 7
if [ "$(get default 'select(.kind == "Deployment" and .metadata.name == "patchy-egress-broker") | .spec.template.metadata.annotations["checksum/config"]')" = \
  "$(get limits 'select(.kind == "Deployment" and .metadata.name == "patchy-egress-broker") | .spec.template.metadata.annotations["checksum/config"]')" ]; then
  fail "limits: the broker's checksum/config did not change, so an upgrade would not roll it"
fi

# ---- status server: the intents views ----------------------------------------
# Off (the default), the status server's template renders byte for byte what
# main rendered before the views existed, blank lines and the chart version
# aside: status-server.default.yaml and status-server.user-roles.yaml are those
# renders, taken from main, and are never regenerated to make this pass. On,
# status-server.intents.yaml is the whole template (regenerate it with the
# same helm template command, the version replaced by VERSION, and review the
# diff). The version is neutralised because release-please bumps it.
chart_version=$(yq eval '.version' "$chart/Chart.yaml" | sed 's/[.]/\\./g')
app_version=$(yq eval '.appVersion' "$chart/Chart.yaml" | sed 's/[.]/\\./g')
# golden_neutral NAME FILE [helm args...]: golden_render with the chart and
# app versions replaced by VERSION in the render, and every checksum/config
# annotation by CHECKSUM: those hash ConfigMaps that carry the version, so they
# change on every release, while the ConfigMaps themselves stay compared.
golden_neutral() {
  name=$1
  file=$2
  shift 2
  if ! helm template patchy "$chart" --namespace patchy "$@" >"$out/$name.raw" 2>"$out/$name.err"; then
    fail "$name: render failed: $(cat "$out/$name.err")"
    return
  fi
  sed -e "s/$app_version/VERSION/g" -e "s/$chart_version/VERSION/g" \
    -e 's/checksum\/config: [0-9a-f]\{64\}/checksum\/config: CHECKSUM/' "$out/$name.raw" >"$out/$name.golden"
  if ! same_render "$file" "$out/$name.golden"; then
    fail "$name: render differs from $file: $(head -20 "$out/same.diff")"
  fi
}
ss=templates/status-server.yaml
sif=$fixtures/status-intents.yaml
golden_neutral status-server-off "$golden/status-server.default.yaml" --show-only "$ss"
golden_neutral status-server-off-roles "$golden/status-server.user-roles.yaml" --show-only "$ss" \
  --set statusServer.rbac.userRoles=true
golden_neutral status-server-intents "$golden/status-server.intents.yaml" --show-only "$ss" -f "$sif"
render status-intents -f "$sif"
cm default status-server PATCHY_INTENTS_ENABLED null
cm status-intents status-server PATCHY_INTENTS_ENABLED true
# Exactly the grants the views need, and none of them while off.
intentrole='select(.kind == "Role" and .metadata.name == "patchy-status-server-intents") | .rules[] | (.resources | join(",")) + " " + (.verbs | join(","))'
expect status-intents "$intentrole" "projects,intents,intentruns,previews get,list,watch"
expect status-intents 'select(.kind == "Role" and .metadata.name == "patchy-status-server-intents") | .metadata.namespace' patchy
jobsrole='select(.kind == "Role" and .metadata.name == "patchy-status-server-agent-jobs")'
expect status-intents "$jobsrole | .metadata.namespace" patchy-agents
expect status-intents "$jobsrole | .rules[] | (.apiGroups | join(\",\")) + \" \" + (.resources | join(\",\")) + \" \" + (.verbs | join(\",\"))" \
  "batch jobs get"
for n in patchy-status-server-intents patchy-status-server-agent-jobs patchy-intents-viewer patchy-intents-content; do
  expect default "select(.metadata.name == \"$n\") | .kind" ""
done
# The kustomize component (components/status-intents) grants exactly what the
# chart does.
kz=deploy/kustomize/components/status-intents/rbac.yaml
for n in patchy-status-server-intents patchy-status-server-agent-jobs; do
  q="select(.kind == \"Role\" and .metadata.name == \"$n\") | .metadata.namespace + \" \" + (.rules | to_json(0))"
  want=$(yq eval "$q" "$kz" | grep -v '^---$' || true)
  got=$(get status-intents "$q")
  if [ -z "$want" ] || [ "$want" != "$got" ]; then
    fail "status-intents: the kustomize component's $n ($want) differs from the chart's ($got)"
  fi
done
# The server never gains a write, a status subresource, Secrets or pods/exec
# from the views.
expect status-intents 'select((.kind == "Role" or .kind == "ClusterRole") and (.metadata.name | test("status-server"))) | .rules[] | select(.resources[] | test("status|secrets|exec")) | .resources | join(",")' ""
expect status-intents "select(.kind == \"ClusterRole\" and .metadata.name == \"patchy-intents-viewer\") | .rules[0].resources | join(\",\")" \
  "projects/intents"
expect status-intents "select(.kind == \"ClusterRole\" and .metadata.name == \"patchy-intents-content\") | .rules[0].resources | join(\",\")" \
  "projects/intents,projects/transcripts"
# Enabling the views rolls the server (its ConfigMap changed).
sscsum='select(.kind == "Deployment" and .metadata.name == "patchy-status-server") | .spec.template.metadata.annotations["checksum/config"]'
if [ "$(get default "$sscsum")" = "$(get status-intents "$sscsum")" ]; then
  fail "status-intents: the status server's checksum/config did not change, so enabling the views would not roll it"
fi
# Guards: the views refuse every auth posture but oidc with both prefixes.
expect_fail 'intents without auth' 'statusServer.intents.enabled requires an auth config in mode oidc' \
  --set statusServer.intents.enabled=true
expect_fail 'intents in mode none' 'requires statusServer.auth.config.mode oidc' \
  --set statusServer.intents.enabled=true --set statusServer.auth.config.mode=none
expect_fail 'intents in mode anonymous' 'requires statusServer.auth.config.mode oidc' \
  --set statusServer.intents.enabled=true --set statusServer.auth.config.mode=anonymous \
  --set statusServer.auth.config.anonymous.username=viewer
expect_fail 'intents without claim prefixes' 'claims.usernamePrefix and groupsPrefix' \
  --set statusServer.intents.enabled=true --set statusServer.auth.config.mode=oidc \
  --set statusServer.auth.config.oidc.issuerURL=https://sso.example.com --set statusServer.auth.config.oidc.clientID=c
expect_fail 'intents with one claim prefix' 'claims.usernamePrefix and groupsPrefix' \
  --set statusServer.intents.enabled=true --set statusServer.auth.config.mode=oidc \
  --set statusServer.auth.config.oidc.claims.usernamePrefix=github:
render status-intents-inline --set statusServer.intents.enabled=true --set statusServer.auth.config.mode=oidc \
  --set statusServer.auth.config.oidc.claims.usernamePrefix=github: \
  --set statusServer.auth.config.oidc.claims.groupsPrefix=github:
cm status-intents-inline status-server PATCHY_INTENTS_ENABLED true
expect_fail 'intents block of the wrong shape' 'additional properties' \
  --set statusServer.intents.bogus=true

# ---- status server: the usage views -----------------------------------------
# Off (the default) the template is the goldens above, untouched. On: exactly
# read on usagerollups for the server, the usage-admin role with the example
# user roles, the flag in the ConfigMap, and the auth guards.
suf=$fixtures/status-usage.yaml
render status-usage -f "$suf"
cm default status-server PATCHY_USAGE_ENABLED null
cm status-intents status-server PATCHY_USAGE_ENABLED null
cm status-usage status-server PATCHY_USAGE_ENABLED true
usagerole='select(.kind == "Role" and .metadata.name == "patchy-status-server-usage") | .metadata.namespace + " " + (.rules[] | (.resources | join(",")) + " " + (.verbs | join(",")))'
expect status-usage "$usagerole" "patchy usagerollups get,list,watch"
expect status-usage 'select(.kind == "RoleBinding" and .metadata.name == "patchy-status-server-usage") | .subjects[0].name' \
  patchy-status-server
expect status-usage 'select(.kind == "ClusterRole" and .metadata.name == "patchy-usage-admin") | .rules[] | (.resources | join(",")) + " " + (.verbs | join(","))' \
  "usage/all get
usagerollups get,list"
for n in patchy-status-server-usage patchy-usage-admin; do
  expect default "select(.metadata.name == \"$n\") | .kind" ""
  expect status-intents "select(.metadata.name == \"$n\") | .kind" ""
done
# The server never gains a write, a status subresource or Secrets from them.
expect status-usage 'select((.kind == "Role" or .kind == "ClusterRole") and (.metadata.name | test("status-server-usage"))) | .rules[] | select(.verbs[] | test("create|update|patch|delete")) | .verbs | join(",")' ""
if [ "$(get default "$sscsum")" = "$(get status-usage "$sscsum")" ]; then
  fail "status-usage: the status server's checksum/config did not change, so enabling the views would not roll it"
fi
uoidc="--set statusServer.usage.enabled=true --set statusServer.auth.config.mode=oidc --set statusServer.auth.config.oidc.issuerURL=https://dex.example.com --set statusServer.auth.config.oidc.clientID=c --set statusServer.auth.config.oidc.claims.usernamePrefix=github: --set statusServer.auth.config.oidc.claims.groupsPrefix=github:"
expect_fail 'usage without auth' 'statusServer.usage.enabled requires an auth config in mode oidc' \
  --set statusServer.usage.enabled=true
expect_fail 'usage in mode none' 'statusServer.usage.enabled requires statusServer.auth.config.mode oidc' \
  --set statusServer.usage.enabled=true --set statusServer.auth.config.mode=none
# shellcheck disable=SC2086 # $uoidc is a word list on purpose
expect_fail 'usage without the federated:id scope' 'requires federated:id' $uoidc
# shellcheck disable=SC2086
expect_fail 'usage with other scopes only' 'requires federated:id' $uoidc \
  --set 'statusServer.auth.config.oidc.scopes={openid,profile,groups}'
expect_fail 'usage without claim prefixes' 'claims.usernamePrefix and groupsPrefix' \
  --set statusServer.usage.enabled=true --set statusServer.auth.config.mode=oidc \
  --set 'statusServer.auth.config.oidc.scopes={openid,federated:id}'
# shellcheck disable=SC2086
render status-usage-inline $uoidc --set 'statusServer.auth.config.oidc.scopes={openid,profile,groups,federated:id}'
cm status-usage-inline status-server PATCHY_USAGE_ENABLED true
expect_fail 'usage block of the wrong shape' 'additional properties' \
  --set statusServer.usage.bogus=true

# ---- egress proxy: off renders exactly what main rendered ------------------
# The templates the egress proxy and network profiles touch, rendered with
# egressProxy off, are byte for byte what main rendered before the feature
# existed (blank lines and the chart version aside): the egress-off.*.yaml
# goldens were taken from main before any template change and are never
# regenerated to make this pass. The CRDs are left out on purpose: the
# networkProfile status field changed them earlier.
egress_off_templates="networkpolicy networkpolicy-gke networkpolicy-cilium configmap deployments intent-controller egress-broker rbac services admission-policy"
egress_show=""
for t in $egress_off_templates; do
  egress_show="$egress_show --show-only templates/$t.yaml"
done
ri=$fixtures/repository-images.yaml
ic=$fixtures/intent-controller.yaml
# shellcheck disable=SC2086
egress_off() {
  name=$1
  shift
  golden_neutral "egress-off-$name" "$golden/egress-off.$name.yaml" $egress_show "$@"
}
egress_off default
egress_off repository-images -f "$ri" -f "$ic"
egress_off repository-images-dns-none -f "$ri" -f "$ic" --set agent.networkPolicy.dns=none
egress_off repository-images-gke -f "$ri" -f "$ic" --set agent.networkPolicy.mode=gke
egress_off repository-images-cilium -f "$ri" -f "$ic" --set agent.networkPolicy.mode=cilium

# Profiles set with the proxy off add nothing: the same objects as the
# default render, and no profile key anywhere.
ep=$fixtures/egress-proxy.yaml
objects='.kind + " " + (.metadata.namespace // "") + " " + .metadata.name'
render egress-default -f "$ri" -f "$ic"
render egress-profiles-off -f "$ri" -f "$ic" -f "$ep" --set egressProxy.enabled=false
if [ "$(get egress-default "$objects" | sort)" != "$(get egress-profiles-off "$objects" | sort)" ]; then
  fail "egress-profiles-off: profiles with egressProxy off changed the object list"
fi
for key in PATCHY_NETWORK_PROFILES_ENABLED PATCHY_NETWORK_PROFILES PATCHY_EGRESS_PROXY_URL; do
  expect egress-profiles-off "select(.kind == \"ConfigMap\") | .data.$key | select(. != null)" ""
done
notes egress-profiles-off -f "$ri" -f "$ep" --set egressProxy.enabled=false
notes_has egress-profiles-off 'agent.networkProfiles is set but egressProxy.enabled is false' yes
notes egress-default -f "$ri"
notes_has egress-default 'agent.networkProfiles is set' no
notes_has egress-default 'Network profiles are on' no

# ---- egress proxy on: two profiles under mode none and dns none -------------
render egress-on -f "$ri" -f "$ic" -f "$ep" --set agent.networkPolicy.dns=none
epdep='select(.kind == "Deployment" and .metadata.name == "patchy-egress-proxy")'
expect egress-on "$epdep | .spec.replicas" 2
expect egress-on "$epdep | .spec.strategy.rollingUpdate.maxUnavailable" 0
expect egress-on "$epdep | .spec.template.spec.containers[0].image | split(\":\") | .[0]" ghcr.io/devthenet-labs/patchy/egress-proxy
expect egress-on "$epdep | .spec.template.spec.containers[0].args | join(\" \")" serve
expect egress-on "$epdep | .spec.template.spec.containers[0].ports[] | .name + \"=\" + (.containerPort | tostring)" \
  'p8090=8090
p8091=8091
health=8081'
expect egress-on "$epdep | .spec.template.spec.containers[0].readinessProbe.initialDelaySeconds" 15
expect egress-on "$epdep | .spec.template.spec.automountServiceAccountToken" false
expect egress-on "$epdep | .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem" true
expect egress-on "$epdep | .spec.template.spec.securityContext.runAsUser" 65532
expect egress-on 'select(.kind == "PodDisruptionBudget" and .metadata.name == "patchy-egress-proxy") | .spec.minAvailable' 1
expect egress-on 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-egress-proxy") | .automountServiceAccountToken' false
expect egress-on 'select(.kind == "Service" and .metadata.name == "patchy-egress-proxy") | .spec.ports[] | .name + " " + (.port | tostring) + " " + (.targetPort | tostring)' \
  'p8090 8090 8090
p8091 8091 8091'
expect egress-on 'select(.kind == "Service" and .metadata.name == "patchy-egress-proxy") | .spec.type' ClusterIP
render egress-one-replica -f "$ri" -f "$ep" --set egressProxy.replicas=1
expect egress-one-replica 'select(.kind == "PodDisruptionBudget") | .metadata.name | select(. == "patchy-egress-proxy")' ""
# The proxy's own ConfigMap, from its own dict.
profiles='[{"allowPseudoVersions":false,"bakedModCache":"/opt/go/pkg/mod","host":"github.com","modules":["github.com/google/uuid","golang.org/x"],"name":"go","port":8090,"repositories":["devthenet-labs/patchy-preview-demo","devthenet-labs/patchy-target"],"route":"go"},{"allowPseudoVersions":true,"bakedModCache":"","host":"github.example.com","modules":["example.com/lib"],"name":"npm","port":8091,"repositories":["acme/.*"],"route":"go"}]'
cm egress-on egress-proxy PATCHY_NETWORK_PROFILES "$profiles"
cm egress-on egress-proxy PATCHY_HEALTH_ADDR :8081
cm egress-on egress-proxy PATCHY_SUSPENDED false
cm egress-on egress-proxy PATCHY_ATTRIBUTE_PODS false
cm egress-on egress-proxy PATCHY_AGENT_NAMESPACE patchy-agents
cm egress-on egress-proxy PATCHY_DENY_CIDRS 10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16
for key in PATCHY_MAX_CONCURRENT PATCHY_LIMIT_WAIT PATCHY_MAX_ZIP_BYTES PATCHY_REQUESTS_PER_SECOND_PER_SOURCE; do
  cm egress-on egress-proxy "$key" null
done
# Both job controllers carry the three keys, the same profile JSON; the other
# job launchers carry none.
for c in remediation-controller intent-controller; do
  cm egress-on "$c" PATCHY_NETWORK_PROFILES_ENABLED true
  cm egress-on "$c" PATCHY_NETWORK_PROFILES "$profiles"
  cm egress-on "$c" PATCHY_EGRESS_PROXY_URL http://patchy-egress-proxy.patchy.svc.cluster.local
done
render egress-on-eval -f "$ri" -f "$ep" -f "$fixtures/evaluation-controller.yaml"
for c in investigation-controller evaluation-controller; do
  for key in PATCHY_NETWORK_PROFILES_ENABLED PATCHY_NETWORK_PROFILES PATCHY_EGRESS_PROXY_URL; do
    cm egress-on-eval "$c" "$key" null
  done
done
# checksum/config: turning the feature on rolls exactly the proxy (new),
# remediation- and intent-controller.
csum='select(.kind == "Deployment") | .metadata.name + " " + .spec.template.metadata.annotations["checksum/config"]'
render egress-before -f "$ri" -f "$ic" --set agent.networkPolicy.dns=none
get egress-before "$csum" | sort >"$out/csum.before"
get egress-on "$csum" | sort >"$out/csum.after"
changed=$(comm -13 "$out/csum.before" "$out/csum.after" | cut -d' ' -f1 | sort | tr '\n' ' ')
if [ "$changed" != "patchy-egress-proxy patchy-intent-controller patchy-remediation-controller " ]; then
  fail "egress-on: checksum/config changed on '$changed', want the proxy, intent- and remediation-controller"
fi
# The proxy's NetworkPolicy: one ingress rule per profile with its exact
# label and port, the probe port, DNS, and 443 out with every private,
# link-local and IPv4 cluster range excepted and nothing IPv6.
epnp='select(.kind == "NetworkPolicy" and .metadata.name == "patchy-egress-proxy")'
expect egress-on "$epnp | .spec.policyTypes | join(\",\")" Ingress,Egress
expect egress-on "$epnp | .spec.ingress[] | select(.from) | (.from[0].namespaceSelector.matchLabels[\"kubernetes.io/metadata.name\"]) + \" \" + (.from[0].podSelector.matchLabels | to_entries | map(.key + \"=\" + .value) | join(\",\")) + \" \" + (.ports[0].port | tostring)" \
  'patchy-agents app.kubernetes.io/name=patchy-agent,app.kubernetes.io/managed-by=patchy,patchy.bitwisemedia.uk/network-go=true 8090
patchy-agents app.kubernetes.io/name=patchy-agent,app.kubernetes.io/managed-by=patchy,patchy.bitwisemedia.uk/network-npm=true 8091'
expect egress-on "$epnp | .spec.ingress[] | select(.from == null) | .ports[0].port" 8081
expect egress-on "$epnp | .spec.egress[] | select(.to[0].ipBlock.cidr == \"0.0.0.0/0\") | .to[0].ipBlock.except | join(\",\")" \
  10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,100.64.0.0/10
expect egress-on "$epnp | .spec.egress[] | select(.to[0].ipBlock.cidr == \"0.0.0.0/0\") | .ports | map(.port) | join(\",\")" 443
expect egress-on "$epnp | .spec.egress[].to[].ipBlock | select(. != null) | .cidr" 0.0.0.0/0
expect egress-on "$epnp | .. | select(tag == \"!!str\") | select(test(\":\"))" ""
# An IPv6 cluster range and a node-local DNS address reach the deny list;
# only the IPv4 ones the except list.
render egress-v6 -f "$ri" -f "$ep" --set clusterDNSCIDR=172.20.0.10/32 \
  --set-json 'agent.networkPolicy.clusterCIDRs=["10.0.0.0/8","fd00::/8","100.80.0.0/16"]'
expect egress-v6 "$epnp | .spec.egress[] | select(.to[0].ipBlock.cidr == \"0.0.0.0/0\") | .to[0].ipBlock.except | join(\",\")" \
  10.0.0.0/8,100.80.0.0/16,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,100.64.0.0/10,172.20.0.10/32
cm egress-v6 egress-proxy PATCHY_DENY_CIDRS 10.0.0.0/8,fd00::/8,100.80.0.0/16,172.20.0.10/32
expect egress-v6 "$epnp | .spec.egress[] | select(.to[0].ipBlock.cidr == \"172.20.0.10/32\") | .ports[0].port" 53
# The agent side: one policy per profile, its label and port alone, and no
# DNS rule anywhere in the agent namespace under dns none.
for pair in go:8090 npm:8091; do
  p=${pair%%:*}
  port=${pair#*:}
  anp="select(.kind == \"NetworkPolicy\" and .metadata.name == \"patchy-agents-network-$p\")"
  expect egress-on "$anp | .metadata.namespace" patchy-agents
  expect egress-on "$anp | .spec.podSelector.matchLabels | to_entries | map(.key + \"=\" + .value) | join(\",\")" \
    "app.kubernetes.io/name=patchy-agent,app.kubernetes.io/managed-by=patchy,patchy.bitwisemedia.uk/network-$p=true"
  expect egress-on "$anp | .spec.policyTypes | join(\",\")" Egress
  expect egress-on "$anp | .spec.egress | length" 1
  expect egress-on "$anp | .spec.egress[0].to[0].podSelector.matchLabels[\"app.kubernetes.io/name\"]" egress-proxy
  expect egress-on "$anp | .spec.egress[0].to[0].namespaceSelector.matchLabels[\"kubernetes.io/metadata.name\"]" patchy
  expect egress-on "$anp | .spec.egress[0].ports | map(.port) | join(\",\")" "$port"
done
expect egress-on 'select(.kind == "NetworkPolicy" and .metadata.namespace == "patchy-agents") | .spec.egress[]?.ports[]? | select(.port == 53) | .port' ""
# The profile-label admission policy names exactly the two launchers.
vap='select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-network-profiles")'
expect egress-on "$vap | .spec.matchConstraints.resourceRules[0].resources | join(\",\")" jobs
expect egress-on "$vap | .spec.validations[0].expression | [match(\"system:serviceaccount:[a-z0-9:-]+\"; \"g\").string] | join(\",\") | select(. != \"\")" \
  system:serviceaccount:patchy:patchy-remediation-controller,system:serviceaccount:patchy:patchy-intent-controller
expect egress-on 'select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == "patchy-network-profiles") | .spec.matchResources.namespaceSelector.matchLabels["kubernetes.io/metadata.name"]' patchy-agents
render egress-on-no-intent -f "$ri" -f "$ep"
expect egress-on-no-intent "$vap | .spec.validations[0].expression | [match(\"system:serviceaccount:[a-z0-9:-]+\"; \"g\").string] | join(\",\") | select(. != \"\")" \
  system:serviceaccount:patchy:patchy-remediation-controller
# Its pod side: Pods in the agent namespace, admitted with a profile label
# only from the Job controller's identity (overridable) and only for a
# controlling batch/v1 Job.
pvap='select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-network-profile-pods")'
expect egress-on "$pvap | .spec.failurePolicy" Fail
expect egress-on "$pvap | .spec.matchConstraints.resourceRules[0].resources | join(\",\")" pods,pods/status
expect egress-on "$pvap | .spec.matchConstraints.resourceRules[0].operations | join(\",\")" CREATE,UPDATE
expect egress-on "$pvap | .spec.validations[0].expression | [match(\"system:serviceaccount:[a-z0-9:-]+\"; \"g\").string] | join(\",\") | select(. != \"\")" \
  system:serviceaccount:kube-system:job-controller
expect egress-on "$pvap | .spec.variables[] | select(.name == \"jobControlled\") | .expression | test(\"r.apiVersion == 'batch/v1' && r.kind == 'Job'\")" true
expect egress-on "$pvap | .spec.variables[] | select(.name == \"profileLabels\") | .expression | test(\"patchy.bitwisemedia.uk/network-\")" true
expect egress-on 'select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == "patchy-network-profile-pods") | .spec.matchResources.namespaceSelector.matchLabels["kubernetes.io/metadata.name"]' patchy-agents
expect egress-on 'select(.kind == "ValidatingAdmissionPolicyBinding" and .metadata.name == "patchy-network-profile-pods") | .spec.validationActions | join(",")' Deny
render egress-on-kcm -f "$ri" -f "$ep" --set-json 'admissionPolicy.jobControllerUsernames=["system:kube-controller-manager"]'
expect egress-on-kcm "$pvap | .spec.validations[0].expression | contains(\"[\\\"system:kube-controller-manager\\\"]\")" true
expect_fail 'an empty jobControllerUsernames' 'minItems' -f "$ri" -f "$ep" --set-json 'admissionPolicy.jobControllerUsernames=[]'
expect egress-profiles-off 'select(.metadata.name == "patchy-network-profile-pods") | .kind' ""
render egress-on-no-vap -f "$ri" -f "$ep" --set admissionPolicy.enabled=false
expect egress-on-no-vap 'select(.metadata.name == "patchy-network-profiles") | .kind' ""
expect egress-on-no-vap 'select(.metadata.name == "patchy-network-profile-pods") | .kind' ""
notes egress-on-no-vap -f "$ri" -f "$ep" --set admissionPolicy.enabled=false
notes_has egress-on-no-vap 'is enforced by
controller code only' yes
notes egress-on -f "$ri" -f "$ep"
notes_has egress-on 'Network profiles are on' yes
notes_has egress-on 'controller code only' no
# The global proxy block never reaches the egress proxy's ConfigMap, and
# its extra keys do (config.extra, then its own).
render egress-https-proxy -f "$ri" -f "$ep" --set proxy.httpsProxy=http://corp-proxy:3128 \
  --set proxy.httpProxy=http://corp-proxy:3128 --set config.extra.PATCHY_X=global \
  --set egressProxy.config.extra.PATCHY_Y=own
for key in HTTPS_PROXY HTTP_PROXY NO_PROXY; do
  cm egress-https-proxy egress-proxy "$key" null
done
cm egress-https-proxy remediation-controller HTTPS_PROXY http://corp-proxy:3128
cm egress-https-proxy egress-proxy PATCHY_X global
cm egress-https-proxy egress-proxy PATCHY_Y own
# Limits render only when set.
render egress-limits -f "$ri" -f "$ep" --set egressProxy.limits.maxConcurrent=64 \
  --set egressProxy.limits.limitWait=10s --set egressProxy.limits.maxZipBytes=1048576000 \
  --set-json egressProxy.limits.requestsPerSecondPerSource=2.5
cm egress-limits egress-proxy PATCHY_MAX_CONCURRENT 64
cm egress-limits egress-proxy PATCHY_LIMIT_WAIT 10s
cm egress-limits egress-proxy PATCHY_MAX_ZIP_BYTES 1048576000
cm egress-limits egress-proxy PATCHY_REQUESTS_PER_SECOND_PER_SOURCE 2.5
cm egress-limits egress-proxy PATCHY_BURST_PER_SOURCE null
# The proxy's log level: the audit and credential lines are info, so the
# shared default warn becomes info for the proxy alone; a louder shared
# level and the proxy's own level are kept.
cm egress-on egress-proxy PATCHY_LOG_LEVEL info
cm egress-on remediation-controller PATCHY_LOG_LEVEL warn
render egress-log-debug -f "$ri" -f "$ep" --set config.logLevel=debug
cm egress-log-debug egress-proxy PATCHY_LOG_LEVEL debug
render egress-log-own -f "$ri" -f "$ep" --set egressProxy.config.logLevel=warn
cm egress-log-own egress-proxy PATCHY_LOG_LEVEL warn
expect_fail 'egress proxy log level of the wrong value' '/egressProxy/config/logLevel' \
  -f "$ri" -f "$ep" --set egressProxy.config.logLevel=loud

# ---- egress proxy: the kill switch -------------------------------------------
# suspendProfiles: the controllers stop applying profiles and the proxy
# rolls into suspended, with the proxy and every policy still rendered.
render egress-suspended -f "$ri" -f "$ic" -f "$ep" --set agent.networkPolicy.dns=none \
  --set egressProxy.suspendProfiles=true
for c in remediation-controller intent-controller; do
  cm egress-suspended "$c" PATCHY_NETWORK_PROFILES_ENABLED false
done
cm egress-suspended egress-proxy PATCHY_SUSPENDED true
if [ "$(get egress-on "$objects" | sort)" != "$(get egress-suspended "$objects" | sort)" ]; then
  fail "egress-suspended: suspendProfiles changed the object list"
fi
epsum="$epdep | .spec.template.metadata.annotations[\"checksum/config\"]"
if [ "$(get egress-on "$epsum")" = "$(get egress-suspended "$epsum")" ]; then
  fail "egress-suspended: the proxy's checksum/config did not change, so suspending would not roll it"
fi
notes egress-suspended -f "$ri" -f "$ep" --set egressProxy.suspendProfiles=true
notes_has egress-suspended 'egressProxy.suspendProfiles is on' yes

# ---- egress proxy: attribution -----------------------------------------------
render egress-attr -f "$ri" -f "$ep" --set egressProxy.attribution.enabled=true \
  --set-json 'egressProxy.attribution.apiServerCIDRs=["10.0.1.10/32","10.0.2.20/32"]'
expect egress-attr "$epdep | .spec.template.spec.automountServiceAccountToken" true
cm egress-attr egress-proxy PATCHY_ATTRIBUTE_PODS true
attrrole='select(.kind == "Role" and .metadata.name == "patchy-egress-proxy-attribution")'
expect egress-attr "$attrrole | .metadata.namespace" patchy-agents
expect egress-attr "$attrrole | .rules[] | (.apiGroups | join(\",\")) + \"|\" + (.resources | join(\",\")) + \"|\" + (.verbs | join(\",\"))" \
  '|pods|get,list,watch'
expect egress-attr 'select(.kind == "RoleBinding" and .metadata.name == "patchy-egress-proxy-attribution") | .subjects[0].name' patchy-egress-proxy
expect egress-attr "$epnp | .spec.egress[] | select(.to[0].ipBlock.cidr == \"10.0.1.10/32\") | (.to | map(.ipBlock.cidr) | join(\",\")) + \" \" + (.ports | map(.port | tostring) | join(\",\"))" \
  "10.0.1.10/32,10.0.2.20/32 443,6443"
expect egress-on 'select(.metadata.name == "patchy-egress-proxy-attribution") | .kind' ""

# ---- egress proxy: dialects ----------------------------------------------------
# gke and cilium render the same plain policies, and no CiliumNetworkPolicy
# for a profile; istio is refused.
for mode in gke cilium; do
  render "egress-$mode" -f "$ri" -f "$ep" --set agent.networkPolicy.mode=$mode
  for n in patchy-egress-proxy patchy-agents-network-go patchy-agents-network-npm; do
    expect "egress-$mode" "select(.metadata.name == \"$n\" and .kind == \"NetworkPolicy\") | .spec" \
      "$(get egress-on "select(.metadata.name == \"$n\" and .kind == \"NetworkPolicy\") | .spec")"
  done
  expect "egress-$mode" 'select(.kind == "CiliumNetworkPolicy") | .metadata.name | select(test("network|egress-proxy"))' ""
done
expect_fail 'egress proxy under istio' 'not supported under agent.networkPolicy.mode istio' \
  -f "$ri" -f "$ep" --set agent.networkPolicy.mode=istio --set agent.networkPolicy.broadEgress=never

# ---- egress proxy guards: each fails, each passes once fixed ------------------
expect_fail 'egress proxy without repository images' 'egressProxy.enabled requires agent.repositoryImages.enabled' \
  -f "$ep"
expect_fail 'egress proxy without profiles' 'requires at least one agent.networkProfiles entry' \
  -f "$ri" --set egressProxy.enabled=true
expect_fail 'egress proxy without its policy' 'egressProxy.networkPolicy.create must stay true' \
  -f "$ri" -f "$ep" --set egressProxy.networkPolicy.create=false
expect_fail 'two profiles on one port' 'go and npm both use port 8090' \
  -f "$ri" -f "$ep" --set agent.networkProfiles.npm.port=8090
expect_fail 'profile on the health port' 'agent/networkProfiles/go/port' \
  -f "$ri" -f "$ep" --set agent.networkProfiles.go.port=8081
expect_fail 'profile name not a DNS label' "invalid propertyName 'Go'" \
  -f "$ri" -f "$ep" --set-json 'agent.networkProfiles.Go={"route":"go","port":8092,"repositories":["a/b"],"modules":["example.com/x"]}'
for m in '*' com GitHub.com/; do
  expect_fail "module prefix $m" 'agent/networkProfiles/go/modules/0' \
    -f "$ri" -f "$ep" --set-json "agent.networkProfiles.go.modules=[\"$m\"]"
done
for m in golang.org golang.org/ golang.org/toolchain golang.org/toolchainx/y; do
  expect_fail "module prefix $m" 'would admit the Go toolchain modules' \
    -f "$ri" -f "$ep" --set-json "agent.networkProfiles.go.modules=[\"$m\"]"
done
expect_fail 'route npm' 'agent/networkProfiles/npm/route' \
  -f "$ri" -f "$ep" --set agent.networkProfiles.npm.route=npm
expect_fail 'empty repositories' 'agent/networkProfiles/go/repositories' \
  -f "$ri" -f "$ep" --set-json 'agent.networkProfiles.go.repositories=[]'
for b in /opt/../x /opt/./x /opt/x/ /opt//x opt/x; do
  expect_fail "bakedModCache $b" 'agent/networkProfiles/go/bakedModCache' \
    -f "$ri" -f "$ep" --set "agent.networkProfiles.go.bakedModCache=$b"
  # A render that skips the schema still meets the guard.
  expect_fail "bakedModCache $b without the schema" 'is not an absolute clean path' \
    -f "$ri" -f "$ep" --set "agent.networkProfiles.go.bakedModCache=$b" --skip-schema-validation
done
expect_fail 'attribution without CIDRs' 'egressProxy.attribution.enabled requires egressProxy.attribution.apiServerCIDRs' \
  -f "$ri" -f "$ep" --set egressProxy.attribution.enabled=true
expect_fail 'attribution with a /24' 'egressProxy/attribution/apiServerCIDRs/0' \
  -f "$ri" -f "$ep" --set egressProxy.attribution.enabled=true \
  --set-json 'egressProxy.attribution.apiServerCIDRs=["10.0.1.0/24"]'
expect_fail 'attribution with a /24 without the schema' 'is not an IPv4 /32' \
  -f "$ri" -f "$ep" --set egressProxy.attribution.enabled=true \
  --set-json 'egressProxy.attribution.apiServerCIDRs=["10.0.1.0/24"]' --skip-schema-validation
expect_fail 'unknown egressProxy key' 'additional properties' \
  -f "$ri" -f "$ep" --set egressProxy.bogus=true
# ...and each passes once fixed.
render egress-fixed -f "$ri" -f "$ep" --set agent.networkProfiles.npm.port=8092 \
  --set agent.networkProfiles.go.bakedModCache=/opt/go/.cache/mod \
  --set-json 'agent.networkProfiles.go.modules=["golang.org/x/mod","github.com/google/uuid/"]' \
  --set egressProxy.attribution.enabled=true --set-json 'egressProxy.attribution.apiServerCIDRs=["10.0.1.10/32"]'
expect egress-fixed 'select(.kind == "Service" and .metadata.name == "patchy-egress-proxy") | .spec.ports | map(.port) | join(",")' 8090,8092
expect egress-fixed 'select(.kind == "ConfigMap" and .metadata.name == "patchy-egress-proxy-config") | .data.PATCHY_NETWORK_PROFILES | from_json | .[0] | .bakedModCache + " " + (.modules | join(","))' \
  "/opt/go/.cache/mod golang.org/x/mod,github.com/google/uuid"
render egress-no-schema -f "$ri" -f "$ep" --skip-schema-validation
expect egress-no-schema 'select(.kind == "Deployment" and .metadata.name == "patchy-egress-proxy") | .kind' Deployment

# ---- egress proxy registries (step 2): off adds nothing ----------------------
# An explicit empty registries map renders exactly the step-1 render, and the
# step-1 render carries none of the registry keys, volumes or rules.
er=$fixtures/egress-registries.yaml
render egress-on-empty-registries -f "$ri" -f "$ic" -f "$ep" --set agent.networkPolicy.dns=none \
  --set-json 'egressProxy.registries={}' --set egressProxy.cloudCredentials=none
if ! same_render "$out/egress-on.yaml" "$out/egress-on-empty-registries.yaml"; then
  fail "egress-on-empty-registries: registries: {} changed the render: $(head -20 "$out/same.diff")"
fi
for key in PATCHY_EGRESS_REGISTRIES PATCHY_ALWAYS_DENY_CIDRS PATCHY_DENY_HOSTS; do
  expect egress-on "select(.kind == \"ConfigMap\") | .data.$key | select(. != null)" ""
done
expect egress-on "$epdep | .spec.template.spec.volumes[] | select(.name | test(\"^registry-\")) | .name" ""
expect egress-on 'select(.kind == "CiliumNetworkPolicy" and .metadata.name == "patchy-egress-proxy-cloud-credentials") | .kind' ""
expect egress-on "$epnp | .spec.egress[] | select(.to[0].ipBlock.cidr == \"169.254.170.23/32\") | .ports[0].port" ""

# ---- egress proxy registries on: four registries, two new profiles -----------
render egress-reg -f "$ri" -f "$ic" -f "$ep" -f "$er" --set agent.networkPolicy.dns=none
regjson='select(.kind == "ConfigMap" and .metadata.name == "patchy-egress-proxy-config") | .data.PATCHY_EGRESS_REGISTRIES'
# The registries JSON: sorted by name, the credential by type alone, and no
# Secret name or key anywhere in any ConfigMap.
expect egress-reg "$regjson | from_json | map(.name + \"=\" + .ecosystem + \"/\" + .auth.type) | join(\",\")" \
  art-go=go/basic,art-npm=npm/bearer,ca-npm=npm/codeartifact,nexus-pypi=pypi/basic
expect egress-reg "$regjson | from_json | .[] | select(.name == \"art-go\") | (.privateModules | join(\",\")) + \" \" + (.publicChecksumLookups | tostring)" \
  "acme.example false"
expect egress-reg "$regjson | from_json | .[] | select(.name == \"ca-npm\") | .auth.codeartifact | .domain + \" \" + .domainOwner + \" \" + .region + \" \" + (.durationSeconds | tostring)" \
  "patchy-gate 377946145366 us-east-1 3600"
expect egress-reg "$regjson | from_json | .[] | select(.name == \"nexus-pypi\") | (.ca | tostring) + \" \" + (.internal.cidrs | join(\",\")) + \" \" + (.internal.targetPort | tostring) + \" \" + .internal.podSelector.namespace" \
  "true 10.100.12.34/32 8081 tools"
expect egress-reg "$regjson | from_json | .[] | select(.name == \"art-npm\") | .redirectHosts[0] + \" \" + .healthPackage" \
  "*.s3.us-east-1.amazonaws.com left-pad"
expect egress-reg "$regjson | from_json | .[] | select(.name != \"art-go\") | has(\"privateModules\")" "false
false
false"
expect egress-reg 'select(.kind == "ConfigMap") | .data | to_entries[] | select(.value | test("patchy-registry-|secretName")) | .key' ""
for c in remediation-controller intent-controller; do
  cm egress-reg "$c" PATCHY_EGRESS_REGISTRIES null
done
cm egress-reg egress-proxy PATCHY_ALWAYS_DENY_CIDRS 172.20.0.10/32
cm egress-reg egress-proxy PATCHY_DENY_HOSTS null
# The profile JSON: one string in the three ConfigMaps, grants on the two new
# profiles only, each with its registry's ecosystem, hosts and (go)
# privateModules, and the step-1 profiles exactly as before.
pj='.data.PATCHY_NETWORK_PROFILES'
for c in remediation-controller intent-controller; do
  expect egress-reg "select(.kind == \"ConfigMap\" and .metadata.name == \"patchy-$c-config\") | $pj" \
    "$(get egress-reg "select(.kind == \"ConfigMap\" and .metadata.name == \"patchy-egress-proxy-config\") | $pj")"
done
regpj="select(.kind == \"ConfigMap\" and .metadata.name == \"patchy-egress-proxy-config\") | $pj | from_json"
expect egress-reg "$regpj | .[] | select(has(\"registries\")) | .name + \" \" + (.registries | map(.name + \"/\" + .ecosystem) | join(\",\"))" \
  'ca ca-npm/npm
web art-go/go,art-npm/npm,nexus-pypi/pypi'
expect egress-reg "$regpj | .[] | select(.name == \"web\") | .route + \"|\" + (.modules | length | tostring)" "|0"
expect egress-reg "$regpj | .[] | select(.name == \"web\") | .registries[] | .name + \" \" + (.hosts | join(\",\"))" \
  'art-go acme.jfrog.io
art-npm acme.jfrog.io,*.s3.us-east-1.amazonaws.com
nexus-pypi nexus.tools.svc.cluster.local,files.pythonhosted.org'
expect egress-reg "$regpj | .[] | select(.name == \"web\") | .registries[] | select(.name == \"art-go\") | (.privateModules | join(\",\")) + \" \" + (.packages | join(\",\")) + \" \" + (.allowPseudoVersions | tostring)" \
  "acme.example acme.example true"
expect egress-reg "$regpj | .[] | select(.name == \"web\") | .registries[] | select(.name == \"art-npm\") | (.packages | join(\",\")) + \" \" + (has(\"privateModules\") | tostring) + \" \" + (has(\"scripts\") | tostring)" \
  "left-pad,@acme/* false false"
expect egress-reg "$regpj | .[] | select(.name == \"ca\") | .registries[0] | .packagePolicy + \" \" + (.scripts | tostring) + \" \" + (has(\"packages\") | tostring)" \
  "registry true false"
for p in go npm; do
  expect egress-reg "$regpj | .[] | select(.name == \"$p\") | to_json(0)" \
    "$(get egress-on "select(.kind == \"ConfigMap\" and .metadata.name == \"patchy-egress-proxy-config\") | $pj | from_json | .[] | select(.name == \"$p\") | to_json(0)")"
done
# The credential volume: one Secret per bearer/basic registry, exactly the
# needed keys, optional false, 0440, mounted read-only with no subPath (kubelet
# never refreshes a subPath mount); the CA volume likewise.
regvol="$epdep | .spec.template.spec.volumes[] | select(.name == \"registry-credentials\") | .projected"
expect egress-reg "$regvol | .defaultMode" 288
expect egress-reg "$regvol | .sources[] | .secret.name + \" \" + (.secret.optional | tostring) + \" \" + (.secret.items | map(.key + \"=\" + .path) | join(\",\"))" \
  'patchy-registry-art-go false username=art-go/username,password=art-go/password
patchy-registry-art false token=art-npm/token
patchy-registry-nexus false username=nexus-pypi/username,password=nexus-pypi/password'
expect egress-reg "$epdep | .spec.template.spec.volumes[] | select(.name == \"registry-ca\") | .projected.sources[] | .configMap.name + \" \" + (.configMap.items | map(.key + \"=\" + .path) | join(\",\"))" \
  "corp-root-ca ca.crt=nexus-pypi.pem"
expect egress-reg "$epdep | .spec.template.spec.containers[0].volumeMounts[] | select(.name | test(\"^registry-\")) | .name + \" \" + .mountPath + \" \" + (.readOnly | tostring)" \
  'registry-credentials /etc/patchy/registry-credentials true
registry-ca /etc/patchy/registry-ca true'
expect egress-reg "$epdep | .. | select(tag == \"!!map\") | select(has(\"subPath\")) | .subPath" ""
# The proxy's policy: the internal registry's CIDR on its URL port, its pods
# on their container port, and the Pod Identity agent; nothing else new.
expect egress-reg "$epnp | .spec.egress[] | select(.to[0].ipBlock.cidr == \"10.100.12.34/32\") | .ports | map(.protocol + \"/\" + (.port | tostring)) | join(\",\")" TCP/8443
expect egress-reg "$epnp | .spec.egress[] | select(.to[0].podSelector) | .to[0].namespaceSelector.matchLabels[\"kubernetes.io/metadata.name\"] + \" \" + (.to[0].podSelector.matchLabels | to_entries | map(.key + \"=\" + .value) | join(\",\")) + \" \" + (.ports[0].port | tostring)" \
  "tools app=nexus 8081"
expect egress-reg "$epnp | .spec.egress[] | select(.to[0].ipBlock.cidr == \"169.254.170.23/32\") | .ports | map(.protocol + \"/\" + (.port | tostring)) | join(\",\")" TCP/80
render egress-reg-base -f "$ri" -f "$ic" -f "$ep" --set agent.networkPolicy.dns=none --set clusterDNSCIDR=172.20.0.10/32
expect egress-reg "$epnp | .spec.egress | length" "$(($(get egress-reg-base "$epnp | .spec.egress | length") + 3))"
expect egress-reg "$epnp | .spec.ingress[] | select(.from) | .ports[0].port" '8093
8090
8091
8092'
# A podSelector rule without targetPort uses the URL's port.
render egress-reg-no-target -f "$ri" -f "$ep" -f "$er" --set egressProxy.registries.nexus-pypi.internal.targetPort=0
expect egress-reg-no-target "$epnp | .spec.egress[] | select(.to[0].podSelector) | .ports[0].port" 8443
# checksum/config: registries roll the proxy and both job controllers, and
# nothing else.
get egress-reg "$csum" | sort >"$out/csum.reg"
changed=$(comm -13 "$out/csum.after" "$out/csum.reg" | cut -d' ' -f1 | sort | tr '\n' ' ')
if [ "$changed" != "patchy-egress-proxy patchy-intent-controller patchy-remediation-controller " ]; then
  fail "egress-reg: checksum/config changed on '$changed', want the proxy, intent- and remediation-controller"
fi
# Under mode cilium the Pod Identity agent is granted by a host-entity CNP.
render egress-reg-cilium -f "$ri" -f "$ep" -f "$er" --set agent.networkPolicy.mode=cilium
regcnp='select(.kind == "CiliumNetworkPolicy" and .metadata.name == "patchy-egress-proxy-cloud-credentials")'
expect egress-reg-cilium "$regcnp | .spec.egress[0].toEntities[0] + \" \" + .spec.egress[0].toPorts[0].ports[0].port" "host 80"
expect egress-reg-cilium "$regcnp | .spec.endpointSelector.matchLabels[\"app.kubernetes.io/name\"]" egress-proxy
expect egress-reg 'select(.kind == "CiliumNetworkPolicy") | .metadata.name' ""
# irsa: the role annotation, and no Pod Identity rule.
render egress-reg-irsa -f "$ri" -f "$ep" -f "$er" --set egressProxy.cloudCredentials=irsa \
  --set-json 'egressProxy.serviceAccount.annotations={"eks.amazonaws.com/role-arn":"arn:aws:iam::377946145366:role/patchy-egress-proxy"}'
expect egress-reg-irsa "$epnp | .spec.egress[] | select(.to[0].ipBlock.cidr == \"169.254.170.23/32\") | .kind" ""
expect egress-reg-irsa 'select(.kind == "ServiceAccount" and .metadata.name == "patchy-egress-proxy") | .metadata.annotations["eks.amazonaws.com/role-arn"]' \
  arn:aws:iam::377946145366:role/patchy-egress-proxy
expect egress-reg-irsa "$epdep | .spec.template.spec.automountServiceAccountToken" false
# Attribution adds the API server's endpoints to the always-denied ranges; a
# render without an internal registry stamps none.
render egress-reg-attr -f "$ri" -f "$ep" -f "$er" --set egressProxy.attribution.enabled=true \
  --set-json 'egressProxy.attribution.apiServerCIDRs=["10.0.1.10/32"]'
cm egress-reg-attr egress-proxy PATCHY_ALWAYS_DENY_CIDRS 172.20.0.10/32,10.0.1.10/32
render egress-reg-public -f "$ri" -f "$ep" --set clusterDNSCIDR=172.20.0.10/32 \
  --set-json 'egressProxy.registries={"art-npm":{"ecosystem":"npm","url":"https://acme.jfrog.io/npm/","auth":{"type":"none"}}}'
cm egress-reg-public egress-proxy PATCHY_ALWAYS_DENY_CIDRS null
expect egress-reg-public "$epdep | .spec.template.spec.volumes[] | select(.name == \"registry-ca\") | .name" ""
expect egress-reg-public "$epnp | .spec.egress[] | select(.to[0].podSelector) | .kind" ""
# The broker's base-URL overrides become request-time redirect denials.
render egress-reg-broker -f "$ri" -f "$ep" -f "$er" \
  --set egressBroker.config.extra.PATCHY_ANTHROPIC_BASE_URL=https://LLM-Gateway.corp.example:8443/anthropic \
  --set config.extra.PATCHY_BEDROCK_BASE_URL=https://bedrock.corp.example
cm egress-reg-broker egress-proxy PATCHY_DENY_HOSTS bedrock.corp.example,llm-gateway.corp.example
# extraEgress appends verbatim.
render egress-reg-extra -f "$ri" -f "$ep" -f "$er" \
  --set-json 'egressProxy.networkPolicy.extraEgress=[{"to":[{"ipBlock":{"cidr":"10.9.0.5/32"}}],"ports":[{"protocol":"TCP","port":4317}]}]'
expect egress-reg-extra "$epnp | .spec.egress[-1] | .to[0].ipBlock.cidr + \" \" + (.ports[0].port | tostring)" "10.9.0.5/32 4317"
# The NOTES name the Secrets the operator creates and the probe.
notes egress-reg -f "$ri" -f "$ep" -f "$er"
notes_has egress-reg '  - patchy-registry-art (art-npm, key token)' yes
notes_has egress-reg '  - patchy-registry-nexus (nexus-pypi, keys username, password)' yes
notes_has egress-reg 'ca-npm mints a CodeArtifact token' yes
notes_has egress-reg '/usr/local/bin/controller probe --registry <name>' yes
notes_has egress-on 'Registries:' no

# ---- egress proxy registry guards: each fails, each passes once fixed ---------
expect_fail 'guard 9: a grant of an undefined registry' 'agent.networkProfiles.ca.registries.nope grants a registry that egressProxy.registries does not define' \
  -f "$ri" -f "$ep" -f "$er" --set-json 'agent.networkProfiles.ca.registries.nope={"packagePolicy":"registry"}'
expect_fail 'guard 10: two npm grants' 'agent.networkProfiles.ca grants two npm registries (art-npm and ca-npm)' \
  -f "$ri" -f "$ep" -f "$er" --set-json 'agent.networkProfiles.ca.registries.art-npm={"packagePolicy":"registry"}'
expect_fail 'four grants (schema)' 'agent/networkProfiles/web/registries' \
  -f "$ri" -f "$ep" -f "$er" --set-json 'agent.networkProfiles.web.registries.ca-npm={"packagePolicy":"registry"}'
expect_fail 'guard 11: no package policy (schema)' 'agent/networkProfiles/ca/registries/ca-npm' \
  -f "$ri" -f "$ep" -f "$er" --set-json 'agent.networkProfiles.ca.registries.ca-npm={"scripts":true}'
expect_fail 'guard 11: no package policy' 'must say what it admits' --skip-schema-validation \
  -f "$ri" -f "$ep" -f "$er" --set-json 'agent.networkProfiles.ca.registries.ca-npm={"scripts":true}'
expect_fail 'guard 11: both policies' 'sets both packages and packagePolicy' --skip-schema-validation \
  -f "$ri" -f "$ep" -f "$er" --set-json 'agent.networkProfiles.ca.registries.ca-npm={"packagePolicy":"registry","packages":["x"]}'
expect_fail 'guard 12: codeartifact without a cloud identity' 'egressProxy.registries.ca-npm is a codeartifact registry but egressProxy.cloudCredentials is none' \
  -f "$ri" -f "$ep" -f "$er" --set egressProxy.cloudCredentials=none
expect_fail 'guard 12: codeartifact host of another domain' 'is not the CodeArtifact endpoint of its auth.codeartifact domain' \
  -f "$ri" -f "$ep" -f "$er" --set-string egressProxy.registries.ca-npm.auth.codeartifact.domainOwner=111111111111
expect_fail 'guard 12: codeartifact go' 'codeartifact has no Go module support' \
  -f "$ri" -f "$ep" -f "$er" --set egressProxy.registries.ca-npm.ecosystem=go \
  --set-json 'egressProxy.registries.ca-npm.privateModules=[]' --set egressProxy.registries.ca-npm.publicChecksumLookups=false
for h in npm.pkg.github.com api.anthropic.com bedrock-runtime.us-east-1.amazonaws.com us-east5-aiplatform.googleapis.com \
  x.cognitiveservices.azure.com generativelanguage.googleapis.com kubernetes.default.svc github.example.com; do
  expect_fail "guard 13: registry on $h" "egressProxy.registries.art-npm.url host $h is" \
    -f "$ri" -f "$ep" -f "$er" --set "egressProxy.registries.art-npm.url=https://$h/npm/"
done
expect_fail 'guard 13: an in-cluster name without internal' 'is an in-cluster name and the registry is not internal' \
  -f "$ri" -f "$ep" -f "$er" --set egressProxy.registries.art-npm.url=https://nexus.tools.svc/npm/
expect_fail 'guard 13: an IP literal outside internal.cidrs' 'is an IP literal outside' \
  -f "$ri" -f "$ep" -f "$er" --set egressProxy.registries.nexus-pypi.url=https://10.100.12.35:8443/simple/
expect_fail 'guard 13: the broker override host' 'egressProxy.registries.art-npm.url host gw.corp.example is gw.corp.example or below it' \
  -f "$ri" -f "$ep" -f "$er" --set egressProxy.registries.art-npm.url=https://gw.corp.example/npm/ \
  --set egressBroker.config.extra.PATCHY_ANTHROPIC_BASE_URL=https://gw.corp.example
expect_fail 'guard 14: a public registry off 443' 'egressProxy.registries.art-npm.url names port 8443' \
  -f "$ri" -f "$ep" -f "$er" --set egressProxy.registries.art-npm.url=https://npm.acme.example:8443/npm/
expect_fail 'guard 15: a /24 inside clusterCIDRs' 'egressProxy.registries.nexus-pypi.internal.cidrs entry 10.100.12.0/24 is inside agent.networkPolicy.clusterCIDRs 10.0.0.0/8' \
  -f "$ri" -f "$ep" -f "$er" --set-json 'egressProxy.registries.nexus-pypi.internal.cidrs=["10.100.12.0/24"]'
expect_fail 'guard 15: clusterDNSCIDR inside cidrs' 'overlaps clusterDNSCIDR 172.20.0.10/32' \
  -f "$ri" -f "$ep" -f "$er" --set-json 'egressProxy.registries.nexus-pypi.internal.cidrs=["172.20.0.0/24"]' \
  --set-json 'agent.networkPolicy.clusterCIDRs=["10.0.0.0/8"]'
expect_fail 'guard 15: link-local' 'overlaps the always-denied range 169.254.0.0/16' \
  -f "$ri" -f "$ep" -f "$er" --set-json 'egressProxy.registries.nexus-pypi.internal.cidrs=["169.254.169.0/24"]' \
  --set-json 'agent.networkPolicy.clusterCIDRs=["10.0.0.0/8"]'
expect_fail 'guard 15: the API server endpoints' 'overlaps attribution.apiServerCIDRs 10.0.1.10/32' \
  -f "$ri" -f "$ep" -f "$er" --set-json 'egressProxy.registries.nexus-pypi.internal.cidrs=["10.0.1.10/32"]' \
  --set egressProxy.attribution.enabled=true --set-json 'egressProxy.attribution.apiServerCIDRs=["10.0.1.10/32"]'
expect_fail 'guard 15: an unmasked CIDR' 'is not a masked IPv4 CIDR' \
  -f "$ri" -f "$ep" -f "$er" --set-json 'egressProxy.registries.nexus-pypi.internal.cidrs=["192.0.2.7/24"]' \
  --set-json 'agent.networkPolicy.clusterCIDRs=["10.0.0.0/8"]'
expect_fail 'guard 16: bearer without a Secret (schema)' 'egressProxy/registries/art-npm/auth' \
  -f "$ri" -f "$ep" -f "$er" --set egressProxy.registries.art-npm.auth.secretName=null
expect_fail 'guard 16: bearer without a Secret' 'egressProxy.registries.art-npm.auth.secretName is required with auth.type bearer' \
  --skip-schema-validation -f "$ri" -f "$ep" -f "$er" --set egressProxy.registries.art-npm.auth.secretName=null
expect_fail 'guard 16: codeartifact with a Secret' 'egressProxy.registries.ca-npm.auth.secretName is set with auth.type codeartifact' \
  -f "$ri" -f "$ep" -f "$er" --set egressProxy.registries.ca-npm.auth.secretName=x
expect_fail 'guard 17: privateModules on npm' 'egressProxy.registries.art-npm: privateModules and publicChecksumLookups are for go registries only' \
  -f "$ri" -f "$ep" -f "$er" --set-json 'egressProxy.registries.art-npm.privateModules=[]'
expect_fail 'guard 17: scripts on a pypi grant' 'agent.networkProfiles.web.registries.nexus-pypi: scripts and allowPrerelease are for npm grants only' \
  -f "$ri" -f "$ep" -f "$er" --set agent.networkProfiles.web.registries.nexus-pypi.scripts=true
expect_fail 'guard 17: sdists on an npm grant' 'sdists is for pypi grants only' \
  -f "$ri" -f "$ep" -f "$er" --set agent.networkProfiles.web.registries.art-npm.sdists=true
expect_fail 'guard 17: allowPseudoVersions on an npm grant' 'allowPseudoVersions is for go grants only' \
  -f "$ri" -f "$ep" -f "$er" --set agent.networkProfiles.web.registries.art-npm.allowPseudoVersions=true
expect_fail 'guard 17: targetPort without podSelector' 'internal.targetPort is the podSelector rule' \
  --skip-schema-validation -f "$ri" -f "$ep" -f "$er" --set-json 'egressProxy.registries.nexus-pypi.internal.podSelector=null'
expect_fail 'guard 18: go without publicChecksumLookups (schema)' 'egressProxy/registries/art-go' \
  -f "$ri" -f "$ep" -f "$er" --set egressProxy.registries.art-go.publicChecksumLookups=null
expect_fail 'guard 18: go without publicChecksumLookups' 'must state both privateModules' --skip-schema-validation \
  -f "$ri" -f "$ep" -f "$er" --set egressProxy.registries.art-go.publicChecksumLookups=null
expect_fail 'guard 19: a denied file host' 'egressProxy.registries.nexus-pypi.fileHosts entry objects.githubusercontent.com is githubusercontent.com or below it' \
  -f "$ri" -f "$ep" -f "$er" --set-json 'egressProxy.registries.nexus-pypi.fileHosts=["objects.githubusercontent.com"]'
expect_fail 'guard 19: a wildcard file host' 'is not an exact host name' --skip-schema-validation \
  -f "$ri" -f "$ep" -f "$er" --set-json 'egressProxy.registries.nexus-pypi.fileHosts=["*.files.acme.example"]'
for pat in '*.amazonaws.com:has fewer than 3 labels' '*.googleapis.com:has fewer than 3 labels' \
  '*.acme.io:has fewer than 3 labels' '*.s3.amazonaws.com:covers the shared suffix' \
  '*.storage.googleapis.com:covers the shared suffix' '*.blob.core.windows.net:covers the shared suffix' \
  '*.us-east-1.amazonaws.com:can match a Bedrock endpoint' \
  '*.api.anthropic.com:overlaps the denied anthropic.com' '*.default.svc.cluster.local:can match the Kubernetes API name'; do
  needle=${pat#*:}
  pat=${pat%%:*}
  expect_fail "guard 19: redirect pattern $pat" "egressProxy.registries.art-npm.redirectHosts entry $pat $needle" \
    -f "$ri" -f "$ep" -f "$er" --set-json "egressProxy.registries.art-npm.redirectHosts=[\"$pat\"]"
done
expect_fail 'a pypi URL that is not an index' 'egressProxy.registries.nexus-pypi.url must be the index URL, ending /simple/' \
  -f "$ri" -f "$ep" -f "$er" --set egressProxy.registries.nexus-pypi.url=https://nexus.tools.svc.cluster.local:8443/repository/pypi-all/
expect_fail 'a profile with neither route nor grant (schema)' 'agent/networkProfiles/web' \
  -f "$ri" -f "$ep" -f "$er" --set-json 'agent.networkProfiles.web.registries={}'
expect_fail 'modules without a route (schema)' 'agent/networkProfiles/web' \
  -f "$ri" -f "$ep" -f "$er" --set-json 'agent.networkProfiles.web.modules=["example.com/x"]'
expect_fail 'irsa without the role' 'egressProxy.cloudCredentials irsa needs the role' \
  -f "$ri" -f "$ep" -f "$er" --set egressProxy.cloudCredentials=irsa
expect_fail 'an unknown registry key' 'additional properties' \
  -f "$ri" -f "$ep" -f "$er" --set egressProxy.registries.art-npm.credential=file
# ...and each passes once fixed: wildcards the table admits, an internal
# registry by in-cluster name with a /32 ClusterIP, and an overlap-free set.
render egress-reg-fixed -f "$ri" -f "$ep" -f "$er" \
  --set-json 'egressProxy.registries.art-npm.redirectHosts=["*.s3.us-east-1.amazonaws.com","*.anthropic.acme.com","cdn.acme.example"]' \
  --set-json 'egressProxy.registries.nexus-pypi.fileHosts=["files.pythonhosted.org","download.pytorch.org"]'
expect egress-reg-fixed "$regjson | from_json | .[] | select(.name == \"art-npm\") | .redirectHosts | join(\",\")" \
  "*.s3.us-east-1.amazonaws.com,*.anthropic.acme.com,cdn.acme.example"
render egress-reg-no-schema -f "$ri" -f "$ep" -f "$er" --skip-schema-validation
expect egress-reg-no-schema "$regjson | from_json | length" 4

# ---- egress proxy tunnels (step 3): no hosts adds nothing ---------------------
# Without hosts on any profile no tunnel key renders anywhere: the go-only and
# registries renders above carry the step-1/2 profile JSON they asserted, and
# no PATCHY_TUNNEL_* key in any ConfigMap.
tunnelkeys='select(.kind == "ConfigMap") | .data | keys | .[] | select(test("^PATCHY_TUNNEL_"))'
for r in egress-on egress-reg egress-attr egress-suspended; do
  expect "$r" "$tunnelkeys" ""
done
notes_has egress-on 'Tunnels:' no

# ---- egress proxy tunnels on: a go profile with hosts, a tunnel-only one ------
et=$fixtures/egress-tunnel.yaml
render egress-tunnel -f "$ri" -f "$ic" -f "$et" --set agent.networkPolicy.dns=none
tpj="select(.kind == \"ConfigMap\" and .metadata.name == \"patchy-egress-proxy-config\") | $pj | from_json"
expect egress-tunnel "$tpj | .[] | select(.name == \"go\") | (.hosts | join(\",\")) + \" \" + (.clients | join(\",\")) + \" \" + (.upload.perTunnel | tostring) + \" \" + (.upload.perSource | tostring)" \
  "pypi.org,files.pythonhosted.org pip,uv 524288 16777216"
# A tunnel-only profile renders none of the go route's values, and no upload
# key when it sets none.
expect egress-tunnel "$tpj | .[] | select(.name == \"npm\") | .route + \" \" + (.modules | length | tostring) + \" [\" + .bakedModCache + \"] \" + (.allowPseudoVersions | tostring) + \" \" + (.hosts | join(\",\")) + \" \" + (.clients | join(\",\")) + \" \" + (has(\"upload\") | tostring)" \
  "tunnel 0 [] false registry.npmjs.org npm false"
# The go profile is its step-1 self plus the three tunnel keys.
expect egress-tunnel "$tpj | .[] | select(.name == \"go\") | del(.hosts, .clients, .upload) | to_json(0)" \
  "$(get egress-on "select(.kind == \"ConfigMap\" and .metadata.name == \"patchy-egress-proxy-config\") | $pj | from_json | .[] | select(.name == \"go\") | to_json(0)")"
for c in remediation-controller intent-controller; do
  expect egress-tunnel "select(.kind == \"ConfigMap\" and .metadata.name == \"patchy-$c-config\") | $pj" \
    "$(get egress-tunnel "select(.kind == \"ConfigMap\" and .metadata.name == \"patchy-egress-proxy-config\") | $pj")"
  cm egress-tunnel "$c" PATCHY_TUNNEL_MAX_CONCURRENT null
done
# The proxy's tunnel keys: the values.yaml defaults (the binary's, pinned by
# cmd/egress-proxy TestChartTunnelDefaultsMatchFlags), the budget window and
# its floor (the intent jobDeadline, 90m, is the longest), and the API
# server's addresses beside tunnel.denyCIDRs for the tunnel alone.
for kv in MAX_CONCURRENT=512 MAX_CONCURRENT_PER_SOURCE=64 RATE_PER_SOURCE=50 BURST_PER_SOURCE=200 \
  LIMIT_WAIT=10s DIAL_TIMEOUT=10s HELLO_TIMEOUT=10s IDLE_TIMEOUT=2m MAX_DURATION=30m MAX_UPLOAD_BYTES=1048576 \
  MAX_DOWNLOAD_BYTES=2147483648 SOURCE_UPLOAD_BUDGET=8388608 SOURCE_DOWNLOAD_BUDGET=8589934592 MAX_SOURCES=4096 \
  PROGRESS_INTERVAL=5m SOURCE_BUDGET_IDLE=2h MIN_BUDGET_IDLE=90m DENY_CIDRS=10.0.1.10/32,203.0.113.10/32; do
  cm egress-tunnel egress-proxy "PATCHY_TUNNEL_${kv%%=*}" "${kv#*=}"
done
expect egress-tunnel "$tunnelkeys | select(. != \"PATCHY_TUNNEL_DENY_CIDRS\")" "$(printf 'PATCHY_TUNNEL_%s\n' BURST_PER_SOURCE DIAL_TIMEOUT \
  HELLO_TIMEOUT IDLE_TIMEOUT LIMIT_WAIT MAX_CONCURRENT MAX_CONCURRENT_PER_SOURCE MAX_DOWNLOAD_BYTES MAX_DURATION MAX_SOURCES \
  MAX_UPLOAD_BYTES MIN_BUDGET_IDLE PROGRESS_INTERVAL RATE_PER_SOURCE SOURCE_BUDGET_IDLE SOURCE_DOWNLOAD_BUDGET \
  SOURCE_UPLOAD_BUDGET)"
# Against the go-only fixture with the same ports and attribution: the Go and
# registry routes' deny list is the same, and so is every NetworkPolicy,
# Service, RBAC and admission object; only the three ConfigMaps (and so the
# three checksums) differ.
render egress-tunnel-base -f "$ri" -f "$ic" -f "$ep" --set agent.networkPolicy.dns=none \
  --set egressProxy.attribution.enabled=true --set-json 'egressProxy.attribution.apiServerCIDRs=["10.0.1.10/32"]'
cm egress-tunnel egress-proxy PATCHY_DENY_CIDRS "$(get egress-tunnel-base 'select(.kind == "ConfigMap" and .metadata.name == "patchy-egress-proxy-config") | .data.PATCHY_DENY_CIDRS')"
for kind in NetworkPolicy Service Role RoleBinding ClusterRole ClusterRoleBinding ServiceAccount PodDisruptionBudget \
  ValidatingAdmissionPolicy ValidatingAdmissionPolicyBinding; do
  if [ "$(get egress-tunnel "select(.kind == \"$kind\") | to_json(0)" | sort)" != \
    "$(get egress-tunnel-base "select(.kind == \"$kind\") | to_json(0)" | sort)" ]; then
    fail "egress-tunnel: a $kind differs from the go-only render with the same ports"
  fi
done
get egress-tunnel-base "$csum" | sort >"$out/csum.tbase"
get egress-tunnel "$csum" | sort >"$out/csum.tunnel"
changed=$(comm -13 "$out/csum.tbase" "$out/csum.tunnel" | cut -d' ' -f1 | sort | tr '\n' ' ')
if [ "$changed" != "patchy-egress-proxy patchy-intent-controller patchy-remediation-controller " ]; then
  fail "egress-tunnel: checksum/config changed on '$changed', want the proxy, intent- and remediation-controller"
fi
# The budget window: without the intent controller its floor is
# agent.jobDeadline; a longer deadline raises both; a set window renders as
# set; and an unset limit keeps the binary's default (no key). The API
# server's addresses reach the tunnel's deny list without attribution too.
render egress-tunnel-no-intent -f "$ri" -f "$et"
cm egress-tunnel-no-intent egress-proxy PATCHY_TUNNEL_MIN_BUDGET_IDLE 1h
cm egress-tunnel-no-intent egress-proxy PATCHY_TUNNEL_SOURCE_BUDGET_IDLE 2h
render egress-tunnel-long -f "$ri" -f "$ic" -f "$et" --set agent.jobDeadline=3h
cm egress-tunnel-long egress-proxy PATCHY_TUNNEL_MIN_BUDGET_IDLE 3h
cm egress-tunnel-long egress-proxy PATCHY_TUNNEL_SOURCE_BUDGET_IDLE 3h
# An intent stage whose timeout plus 15m outlasts intentController's
# jobDeadline stretches its Job's deadline (LF2), up to jobDeadlineMax, and
# the window follows the longest such deadline. The floor stays 90m: it is
# not tied to the stage timeouts, so this is a shape intent-controller starts
# with (TestSettingsDecouplesTheDeadlineFloor).
render egress-tunnel-stage -f "$ri" -f "$ic" -f "$et" --set intentController.config.build.timeout=3h
cm egress-tunnel-stage egress-proxy PATCHY_TUNNEL_MIN_BUDGET_IDLE 11700s
cm egress-tunnel-stage egress-proxy PATCHY_TUNNEL_SOURCE_BUDGET_IDLE 11700s
cm egress-tunnel-stage intent-controller PATCHY_INTENT_JOB_DEADLINE 90m
cm egress-tunnel-stage intent-controller PATCHY_INTENT_BUILD_TIMEOUT 3h
# A stage that exactly fits jobDeadlineMax with its 15m margin reaches it.
render egress-tunnel-capped -f "$ri" -f "$ic" -f "$et" --set intentController.config.build.timeout=1h45m \
  --set intentController.config.jobDeadlineMax=2h
cm egress-tunnel-capped egress-proxy PATCHY_TUNNEL_MIN_BUDGET_IDLE 7200s
# One that does not fit is refused, as intent-controller refuses to start.
for stage in plan build revise; do
  expect_fail "intent $stage timeout past jobDeadlineMax" \
    "intentController.config.$stage.timeout 3h plus 15m is longer than intentController.config.jobDeadlineMax 2h" \
    -f "$ri" -f "$ic" -f "$et" --set "intentController.config.$stage.timeout=3h" \
    --set intentController.config.jobDeadlineMax=2h
done
expect_fail 'guard 26: an unreadable intent deadline ceiling' 'intentController.config.jobDeadlineMax must be written in whole h, m and s' \
  -f "$ri" -f "$ic" -f "$et" --set intentController.config.jobDeadlineMax=4.5h
render egress-tunnel-set -f "$ri" -f "$ic" -f "$et" --set egressProxy.tunnel.sourceBudgetIdle=4h \
  --set egressProxy.tunnel.maxConcurrent=0 --set-string egressProxy.tunnel.progressInterval=0 \
  --set egressProxy.attribution.enabled=false --set-json 'egressProxy.tunnel.denyCIDRs=[]'
cm egress-tunnel-set egress-proxy PATCHY_TUNNEL_SOURCE_BUDGET_IDLE 4h
cm egress-tunnel-set egress-proxy PATCHY_TUNNEL_MAX_CONCURRENT null
cm egress-tunnel-set egress-proxy PATCHY_TUNNEL_PROGRESS_INTERVAL 0
cm egress-tunnel-set egress-proxy PATCHY_TUNNEL_DENY_CIDRS 10.0.1.10/32
render egress-tunnel-no-deny -f "$ri" -f "$et" --set-json 'egressProxy.attribution.apiServerCIDRs=[]' \
  --set egressProxy.attribution.enabled=false --set-json 'egressProxy.tunnel.denyCIDRs=[]'
cm egress-tunnel-no-deny egress-proxy PATCHY_TUNNEL_DENY_CIDRS null
# The NOTES name the tunnel profiles and the npm publish residue.
notes egress-tunnel -f "$ri" -f "$et"
notes_has egress-tunnel 'Tunnels: go, npm open HTTPS CONNECT tunnels' yes
notes_has egress-tunnel 'Publish residue: registry.npmjs.org (npm) is an npm registry' yes
notes egress-tunnel-pip -f "$ri" -f "$et" --set-json 'agent.networkProfiles.npm.hosts=["pypi.org"]' \
  --set-json 'agent.networkProfiles.npm.clients=["pip"]'
notes_has egress-tunnel-pip 'Publish residue' no

# ---- egress proxy tunnel guards: each fails, each passes once fixed -----------
for pair in 'github.com:github.com or below it' 'objects.githubusercontent.com:githubusercontent.com or below it' \
  'api.anthropic.com:anthropic.com or below it' 'x.eks.amazonaws.com:eks.amazonaws.com or below it' \
  'gitlab.com:gitlab.com or below it' 'x.hcp.westeurope.azmk8s.io:azmk8s.io or below it' \
  'upload.pypi.org:upload.pypi.org or below it' 'www.nuget.org:www.nuget.org or below it' \
  'metadata.google.internal:internal or below it' 'x.kubernetes.default:below kubernetes.default' \
  'bedrock-runtime.us-east-1.amazonaws.com:a Bedrock endpoint' 'pkg.github.example.com:github.example.com or below it'; do
  h=${pair%%:*}
  expect_fail "guard 22: tunnel host $h" "agent.networkProfiles.npm.hosts entry $h is ${pair#*:}" \
    -f "$ri" -f "$et" --set-json "agent.networkProfiles.npm.hosts=[\"$h\"]"
done
expect_fail 'guard 22: the broker override host' 'agent.networkProfiles.npm.hosts entry gw.corp.net is gw.corp.net or below it' \
  -f "$ri" -f "$et" --set-json 'agent.networkProfiles.npm.hosts=["gw.corp.net"]' \
  --set egressBroker.config.extra.PATCHY_ANTHROPIC_BASE_URL=https://gw.corp.net
for h in '*.npmjs.org' 10.0.0.1 Registry.npmjs.org registry.npmjs.org. registry_npm.org localhost example.123; do
  expect_fail "tunnel host $h (schema)" 'agent/networkProfiles/npm/hosts/0' \
    -f "$ri" -f "$et" --set-json "agent.networkProfiles.npm.hosts=[\"$h\"]"
  expect_fail "guard 21: tunnel host $h without the schema" "agent.networkProfiles.npm.hosts entry \"$h\" is not an exact lowercase host name" \
    --skip-schema-validation -f "$ri" -f "$et" --set-json "agent.networkProfiles.npm.hosts=[\"$h\"]"
done
expect_fail 'guard 21: a host twice (schema)' 'agent/networkProfiles/npm/hosts' \
  -f "$ri" -f "$et" --set-json 'agent.networkProfiles.npm.hosts=["registry.npmjs.org","registry.npmjs.org"]'
expect_fail 'guard 21: a host twice' 'agent.networkProfiles.npm.hosts lists registry.npmjs.org twice' --skip-schema-validation \
  -f "$ri" -f "$et" --set-json 'agent.networkProfiles.npm.hosts=["registry.npmjs.org","registry.npmjs.org"]'
expect_fail 'guard 20: tunnel without hosts (schema)' 'agent/networkProfiles/npm' \
  -f "$ri" -f "$et" --set-json 'agent.networkProfiles.npm.hosts=[]'
expect_fail 'guard 20: tunnel without hosts' 'agent.networkProfiles.npm is route tunnel without hosts' --skip-schema-validation \
  -f "$ri" -f "$et" --set-json 'agent.networkProfiles.npm.hosts=[]' --set-json 'agent.networkProfiles.npm.clients=[]'
expect_fail 'guard 20: tunnel with modules (schema)' 'agent/networkProfiles/npm/modules' \
  -f "$ri" -f "$et" --set-json 'agent.networkProfiles.npm.modules=["example.com/x"]'
expect_fail 'guard 20: tunnel with modules' 'agent.networkProfiles.npm is route tunnel: modules, bakedModCache and allowPseudoVersions' \
  --skip-schema-validation -f "$ri" -f "$et" --set-json 'agent.networkProfiles.npm.modules=["example.com/x"]'
expect_fail 'guard 20: tunnel with allowPseudoVersions (schema)' 'agent/networkProfiles/npm/allowPseudoVersions' \
  -f "$ri" -f "$et" --set agent.networkProfiles.npm.allowPseudoVersions=true
expect_fail 'guard 20: tunnel with a baked cache' 'agent.networkProfiles.npm is route tunnel' --skip-schema-validation \
  -f "$ri" -f "$et" --set agent.networkProfiles.npm.bakedModCache=/opt/go/pkg/mod
expect_fail 'a go profile without modules (schema)' 'agent/networkProfiles/py' \
  -f "$ri" -f "$et" --set-json 'agent.networkProfiles.py={"route":"go","port":8093,"repositories":["a/b"],"hosts":["pypi.org"]}'
expect_fail 'hosts without a route (schema)' 'agent/networkProfiles/py' \
  -f "$ri" -f "$et" -f "$er" --set-json 'agent.networkProfiles.py={"port":8094,"repositories":["a/b"],"registries":{"ca-npm":{"packagePolicy":"registry"}},"hosts":["pypi.org"]}'
expect_fail 'guard 23: a Go route upstream' 'agent.networkProfiles.go.hosts entry proxy.golang.org is an upstream of the profile' \
  -f "$ri" -f "$et" --set-json 'agent.networkProfiles.go.hosts=["proxy.golang.org"]'
expect_fail 'guard 23: a grant host' 'agent.networkProfiles.web.hosts entry files.pythonhosted.org is files.pythonhosted.org, a host of its nexus-pypi grant' \
  -f "$ri" -f "$ep" -f "$er" --set agent.networkProfiles.web.route=tunnel --set-json 'agent.networkProfiles.web.hosts=["files.pythonhosted.org"]'
expect_fail 'guard 23: a grant pattern' 'is *.s3.us-east-1.amazonaws.com, a host of its art-npm grant' \
  -f "$ri" -f "$ep" -f "$er" --set agent.networkProfiles.web.route=tunnel --set-json 'agent.networkProfiles.web.hosts=["b.s3.us-east-1.amazonaws.com"]'
expect_fail 'guard 24: clients without hosts (schema)' 'agent/networkProfiles/go' \
  -f "$ri" -f "$ep" --set-json 'agent.networkProfiles.go.clients=["pip"]'
expect_fail 'guard 24: clients without hosts' 'agent.networkProfiles.go.clients configure the tunnel' --skip-schema-validation \
  -f "$ri" -f "$ep" --set-json 'agent.networkProfiles.go.clients=["pip"]'
expect_fail 'guard 24: an unknown client (schema)' 'agent/networkProfiles/npm/clients/0' \
  -f "$ri" -f "$et" --set-json 'agent.networkProfiles.npm.clients=["pnpm"]'
expect_fail 'guard 24: an unknown client' 'agent.networkProfiles.npm.clients entry "pnpm" is not one of npm, yarn, pip, uv, nuget, cargo, jvm' \
  --skip-schema-validation -f "$ri" -f "$et" --set-json 'agent.networkProfiles.npm.clients=["pnpm"]'
expect_fail 'guard 24: a preset beside a grant of its package manager' 'agent.networkProfiles.web: client pip and the pypi grant nexus-pypi both configure the same package manager' \
  -f "$ri" -f "$ep" -f "$er" --set agent.networkProfiles.web.route=tunnel --set-json 'agent.networkProfiles.web.hosts=["pypi.org"]' \
  --set-json 'agent.networkProfiles.web.clients=["pip"]'
expect_fail 'guard 25: upload without hosts (schema)' 'agent/networkProfiles/go' \
  -f "$ri" -f "$ep" --set agent.networkProfiles.go.upload.perTunnel=65536
expect_fail 'guard 25: upload without hosts' 'agent.networkProfiles.go.upload caps the tunnel' --skip-schema-validation \
  -f "$ri" -f "$ep" --set agent.networkProfiles.go.upload.perTunnel=65536
expect_fail 'guard 25: upload below 64 KiB (schema)' 'agent/networkProfiles/go/upload/perTunnel' \
  -f "$ri" -f "$et" --set agent.networkProfiles.go.upload.perTunnel=1024
expect_fail 'guard 25: upload below 64 KiB' 'agent.networkProfiles.go.upload.perTunnel 1024 is outside 65536-1073741824 bytes' \
  --skip-schema-validation -f "$ri" -f "$et" --set agent.networkProfiles.go.upload.perTunnel=1024
expect_fail 'guard 25: perTunnel above perSource' 'agent.networkProfiles.go.upload.perTunnel 16777217 is above upload.perSource 16777216' \
  -f "$ri" -f "$et" --set agent.networkProfiles.go.upload.perTunnel=16777217
expect_fail 'guard 26: a budget window below the intent deadline' 'egressProxy.tunnel.sourceBudgetIdle 30m is below the longest Job deadline 90m' \
  -f "$ri" -f "$ic" -f "$et" --set egressProxy.tunnel.sourceBudgetIdle=30m
expect_fail 'guard 26: a budget window below the agent deadline' 'egressProxy.tunnel.sourceBudgetIdle 2h is below the longest Job deadline 3h' \
  -f "$ri" -f "$et" --set egressProxy.tunnel.sourceBudgetIdle=2h --set agent.jobDeadline=3h
expect_fail 'guard 26: a deadline in milliseconds' 'agent.jobDeadline must be written in whole h, m and s' \
  -f "$ri" -f "$et" --set agent.jobDeadline=3600000ms
# An intent deadline the chart cannot read is refused on its own, not passed
# over for a shorter agent deadline (the budget window would come out short).
expect_fail 'guard 26: a fractional intent deadline' 'intentController.config.jobDeadline must be written in whole h, m and s' \
  -f "$ri" -f "$ic" -f "$et" --set agent.jobDeadline=1h --set intentController.config.jobDeadline=2.5h
expect_fail 'guard 26: an intent deadline in milliseconds' 'intentController.config.jobDeadline must be written in whole h, m and s' \
  -f "$ri" -f "$ic" -f "$et" --set agent.jobDeadline=1h --set intentController.config.jobDeadline=5400000ms
expect_fail 'guard 26: a short window beside a fractional intent deadline' 'intentController.config.jobDeadline must be written in whole h, m and s' \
  -f "$ri" -f "$ic" -f "$et" --set agent.jobDeadline=1h --set intentController.config.jobDeadline=2.5h \
  --set egressProxy.tunnel.sourceBudgetIdle=1h
expect_fail 'a budget window in milliseconds (schema)' 'egressProxy/tunnel/sourceBudgetIdle' \
  -f "$ri" -f "$et" --set egressProxy.tunnel.sourceBudgetIdle=7200000ms
expect_fail 'an unknown tunnel key' 'additional properties' \
  -f "$ri" -f "$et" --set egressProxy.tunnel.bogus=1
expect_fail 'a tunnel deny entry that is no CIDR' 'egressProxy/tunnel/denyCIDRs/0' \
  -f "$ri" -f "$et" --set-json 'egressProxy.tunnel.denyCIDRs=["api.example.com"]'
# A deadline in milliseconds is fine while nothing tunnels.
render egress-tunnel-ms-off -f "$ri" -f "$ep" --set agent.jobDeadline=3600000ms
expect egress-tunnel-ms-off "$epdep | .kind" Deployment
# ...and each passes once fixed: a budget window at the deadline, a host that
# only looks like a denied one, and the cap at its bounds.
render egress-tunnel-fixed -f "$ri" -f "$ic" -f "$et" --set egressProxy.tunnel.sourceBudgetIdle=1h30m \
  --set-json 'agent.networkProfiles.npm.hosts=["registry.npmjs.org","github.com.example.org","xn--p1ai.a1"]' \
  --set agent.networkProfiles.go.upload.perTunnel=16777216
cm egress-tunnel-fixed egress-proxy PATCHY_TUNNEL_SOURCE_BUDGET_IDLE 1h30m
expect egress-tunnel-fixed "$tpj | .[] | select(.name == \"npm\") | .hosts | join(\",\")" "registry.npmjs.org,github.com.example.org,xn--p1ai.a1"
render egress-tunnel-no-schema -f "$ri" -f "$et" --skip-schema-validation
expect egress-tunnel-no-schema "$tpj | length" 2

# ---- operator env, envFrom, volumes and mounts on every Deployment ----------
# <component>.extraEnv/extraEnvFrom/extraVolumes/extraVolumeMounts, the same
# four keys on all twelve Deployments: a Secret-backed env var (AWS keys for
# source-controller resolving ECR off AWS), an envFrom source listed BEFORE
# the chart's ConfigMap (a later source wins a clash, so the chart's keys
# win), and a mounted volume. Every optional component is on here.
components="integrationController:integration-controller sourceController:source-controller
contextController:context-controller investigationController:investigation-controller
remediationController:remediation-controller evaluationController:evaluation-controller
intentController:intent-controller statusServer:status-server egressBroker:egress-broker
egressProxy:egress-proxy previewController:preview-controller previewAuth:preview-auth"
extras=$out/extras.yaml
: >"$extras"
for pair in $components; do
  key=${pair%%:*}
  cat >>"$extras" <<EXTRAS
$key:
  extraEnv:
    - name: EXTRA_FROM_SECRET
      valueFrom:
        secretKeyRef: {name: $key-env, key: value}
  extraEnvFrom:
    - secretRef: {name: $key-envfrom}
  extraVolumes:
    - name: extra-ca
      configMap: {name: $key-ca}
  extraVolumeMounts:
    - name: extra-ca
      mountPath: /etc/extra-ca
      readOnly: true
EXTRAS
done
allcomponents="-f $pf -f $ic -f $pc -f $pav -f $ri -f $fixtures/egress-proxy.yaml -f $fixtures/evaluation-controller.yaml"
# shellcheck disable=SC2086 # $allcomponents is a list of helm arguments.
render extras-all $allcomponents -f "$extras"
# shellcheck disable=SC2086
render extras-none $allcomponents
for pair in $components; do
  key=${pair%%:*}
  dep="select(.kind == \"Deployment\" and .metadata.name == \"patchy-${pair#*:}\") | .spec.template.spec"
  expect extras-all "$dep | .containers[0].env[] | select(.name == \"EXTRA_FROM_SECRET\") | .valueFrom.secretKeyRef.name" "$key-env"
  expect extras-all "$dep | .containers[0].envFrom[0].secretRef.name" "$key-envfrom"
  expect extras-all "$dep | .containers[0].envFrom[-1].configMapRef.name" "patchy-${pair#*:}-config"
  expect extras-all "$dep | .volumes[] | select(.name == \"extra-ca\") | .configMap.name" "$key-ca"
  expect extras-all "$dep | .containers[0].volumeMounts[] | select(.name == \"extra-ca\") | .mountPath" "/etc/extra-ca"
  # Unset, nothing of it renders.
  expect extras-none "$dep | .containers[0].env[]? | select(.name == \"EXTRA_FROM_SECRET\") | .name" ""
  expect extras-none "$dep | .containers[0].envFrom | length" "1"
  expect extras-none "$dep | .volumes[] | select(.name == \"extra-ca\") | .name" ""
done
# The names patchy sets are refused, wherever they are asked for.
expect_fail "extraEnv names a PATCHY_ key" 'sourceController.extraEnv: PATCHY_ARTIFACT_DIR is set by the chart' \
  --set-json 'sourceController.extraEnv=[{"name":"PATCHY_ARTIFACT_DIR","value":"/x"}]'
expect_fail "extraEnv names POD_NAMESPACE" 'egressBroker.extraEnv: POD_NAMESPACE is set by the chart' \
  --set-json 'egressBroker.extraEnv=[{"name":"POD_NAMESPACE","value":"x"}]'
expect_fail "extraEnv names DOCKER_CONFIG" 'sourceController.extraEnv: DOCKER_CONFIG is set by the chart' \
  --set-json 'sourceController.extraEnv=[{"name":"DOCKER_CONFIG","value":"/x"}]'
expect_fail "an extraEnv entry of the wrong shape" 'additional properties' \
  --set-json 'contextController.extraEnv=[{"name":"A","secretRef":{"name":"x"}}]'
expect_fail "an extraVolumeMount without a path" 'mountPath' \
  --set-json 'contextController.extraVolumeMounts=[{"name":"x"}]'

# ---- agent.paused: the cluster-wide pause ------------------------------------
# PATCHY_AGENTS_PAUSED reaches exactly the four controllers that launch agent
# Jobs (and status-server, which shows it), only while paused; NOTES say so.
# shellcheck disable=SC2086
render paused $allcomponents --set agent.paused=true
for c in investigation-controller remediation-controller intent-controller evaluation-controller status-server; do
  cm paused "$c" PATCHY_AGENTS_PAUSED true
  cm extras-none "$c" PATCHY_AGENTS_PAUSED null
done
expect paused 'select(.kind == "ConfigMap") | select(.data.PATCHY_AGENTS_PAUSED != null) | .metadata.name' \
  "$(printf '%s\n' patchy-investigation-controller-config patchy-remediation-controller-config patchy-evaluation-controller-config patchy-intent-controller-config patchy-status-server-config)"
expect_fail "agent.paused of the wrong type" '/agent/paused' --set-string agent.paused=yes
notes paused-notes --set agent.paused=true
notes_has paused-notes 'AGENTS PAUSED: agent.paused is true' yes
notes default-notes
notes_has default-notes 'AGENTS PAUSED' no

# ---- attribution.mode: run attribution (design L1) -------------------------
# Off (the default) renders no PATCHY_ATTRIBUTION_MODE and no attribution
# policy at all. attribute reaches exactly the four controllers that launch
# agent Jobs, and renders the Job, pod, Finding-stamp and run-stamp policies,
# plus the Intent-requester policy only with the intent controller and the
# Evaluation-submitter policy only with the evaluation controller; the Job
# policy names each launcher present, investigation-controller org-only, and
# the run-stamp policy pins each run kind to its own controller.
attribution_policies='select(.kind == "ValidatingAdmissionPolicy") | select(.metadata.name | test("^patchy-attribution-")) | .metadata.name'
# shellcheck disable=SC2086
expect extras-none 'select(.kind == "ConfigMap") | select(.data.PATCHY_ATTRIBUTION_MODE != null) | .metadata.name' ""
expect extras-none "$attribution_policies" ""
# shellcheck disable=SC2086
render attribution $allcomponents --set attribution.mode=attribute
expect attribution 'select(.kind == "ConfigMap") | select(.data.PATCHY_ATTRIBUTION_MODE != null) | .metadata.name' \
  "$(printf '%s\n' patchy-investigation-controller-config patchy-remediation-controller-config patchy-evaluation-controller-config patchy-intent-controller-config)"
for c in investigation-controller remediation-controller intent-controller evaluation-controller; do
  cm attribution "$c" PATCHY_ATTRIBUTION_MODE attribute
done
expect attribution "$attribution_policies" \
  "$(printf '%s\n' patchy-attribution-jobs patchy-attribution-pods patchy-attribution-finding-stamps patchy-attribution-run-stamps patchy-attribution-intent-requester patchy-attribution-evaluation-submitter)"
expect attribution 'select(.kind == "ValidatingAdmissionPolicyBinding") | select(.metadata.name == "patchy-attribution-jobs") | .spec.matchResources.namespaceSelector.matchLabels."kubernetes.io/metadata.name"' "patchy-agents"
launchers='select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-attribution-jobs") | .spec.variables[] | select(.name == "launcher") | .expression'
for sa in investigation-controller remediation-controller intent-controller evaluation-controller; do
  if ! get attribution "$launchers" | grep -qF "system:serviceaccount:patchy:patchy-$sa"; then
    fail "attribution: the Job policy does not name $sa as a launcher"
  fi
done
if ! get attribution 'select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-attribution-jobs") | .spec.validations[0].expression' |
  grep -qF "patchy-investigation-controller' && variables.runKind == 'investigation' && variables.job.value() == 'org'"; then
  fail "attribution: the Job policy does not hold investigation-controller to org"
fi
run_stamps='select(.kind == "ValidatingAdmissionPolicy" and .metadata.name == "patchy-attribution-run-stamps") | .spec.validations[0].expression'
for pair in investigations:investigation-controller remediations:remediation-controller intentruns:intent-controller evaluationunits:evaluation-controller; do
  if ! get attribution "$run_stamps" |
    grep -qF "request.resource.resource == '${pair%%:*}' && request.userInfo.username == 'system:serviceaccount:patchy:patchy-${pair#*:}'"; then
    fail "attribution: the run-stamp policy does not pin ${pair%%:*} to ${pair#*:}"
  fi
done
render attribution-core --set attribution.mode=attribute
expect attribution-core "$attribution_policies" \
  "$(printf '%s\n' patchy-attribution-jobs patchy-attribution-pods patchy-attribution-finding-stamps patchy-attribution-run-stamps)"
for sa in intent-controller evaluation-controller; do
  if get attribution-core "$launchers" | grep -qF "patchy-$sa"; then
    fail "attribution-core: the Job policy names $sa, which is not installed"
  fi
done
expect_fail "attribution.mode not in the enum" '/attribution/mode' --set attribution.mode=enforce
expect_fail "attribution of the wrong shape" 'additional properties' --set attribution.bogus=true

# ---- trusted usage (design L2): the broker's usage endpoint ------------------
# Off (attribution.mode off, the default) renders no usage key, port, token or
# policy rule anywhere. Under attribute, with the broker deployed, the broker
# serves the endpoint on 8082 to exactly the run controllers present (its
# callers, its NetworkPolicy), and each of them gets the meter's settings, the
# projected usage-audience token and an egress rule to that port only.
usage_cms='select(.kind == "ConfigMap") | select(.data.PATCHY_USAGE_MEASUREMENT != null) | .metadata.name'
expect extras-none "$usage_cms" ""
expect extras-none 'select(.kind == "ConfigMap" and .metadata.name == "patchy-egress-broker-config") | .data.PATCHY_USAGE_ADDR' null
expect extras-none 'select(.kind == "Service" and .metadata.name == "patchy-egress-broker") | .spec.ports[] | select(.name == "usage") | .port' ""
expect extras-none 'select(.kind == "Deployment") | .spec.template.spec.volumes[] | select(.name == "broker-usage-token") | .name' ""
expect attribution "$usage_cms" \
  "$(printf '%s\n' patchy-investigation-controller-config patchy-remediation-controller-config patchy-evaluation-controller-config patchy-intent-controller-config)"
for c in investigation-controller remediation-controller intent-controller evaluation-controller; do
  cm attribution "$c" PATCHY_USAGE_MEASUREMENT true
  cm attribution "$c" PATCHY_USAGE_BROKER_URL http://patchy-egress-broker.patchy.svc.cluster.local:8082
  cm attribution "$c" PATCHY_USAGE_TOKEN_FILE /var/run/secrets/patchy/broker-usage/token
  expect attribution "select(.kind == \"Deployment\" and .metadata.name == \"patchy-$c\") | .spec.template.spec.volumes[] | select(.name == \"broker-usage-token\") | .projected.sources[0].serviceAccountToken.audience" patchy-egress-broker-usage
  expect attribution "select(.kind == \"Deployment\" and .metadata.name == \"patchy-$c\") | .spec.template.spec.containers[0].volumeMounts[] | select(.name == \"broker-usage-token\") | .mountPath" /var/run/secrets/patchy/broker-usage
  expect attribution "select(.kind == \"NetworkPolicy\" and .metadata.name == \"patchy-$c\") | .spec.egress[] | select(.ports[0].port == 8082) | .to[0].podSelector.matchLabels.\"app.kubernetes.io/name\"" egress-broker
done
expect attribution 'select(.kind == "ConfigMap" and .metadata.name == "patchy-egress-broker-config") | .data.PATCHY_USAGE_ADDR' :8082
expect attribution 'select(.kind == "ConfigMap" and .metadata.name == "patchy-egress-broker-config") | .data.PATCHY_USAGE_CALLERS' \
  patchy:patchy-investigation-controller,patchy:patchy-remediation-controller,patchy:patchy-intent-controller,patchy:patchy-evaluation-controller
expect attribution 'select(.kind == "Service" and .metadata.name == "patchy-egress-broker") | .spec.ports[] | select(.name == "usage") | .port' 8082
expect attribution 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-egress-broker") | .spec.ingress[] | select(.ports[0].port == 8082) | .from[].podSelector.matchLabels."app.kubernetes.io/name"' \
  "$(printf '%s\n' investigation-controller remediation-controller intent-controller evaluation-controller)"
# No other controller reaches the usage port, and the agents never do.
expect attribution 'select(.kind == "NetworkPolicy" and .metadata.name == "patchy-egress-broker") | .spec.ingress[] | select(.ports[0].port == 8082) | .from[] | select(.namespaceSelector != null) | .namespaceSelector' ""
expect attribution-core 'select(.kind == "ConfigMap" and .metadata.name == "patchy-egress-broker-config") | .data.PATCHY_USAGE_CALLERS' \
  patchy:patchy-investigation-controller,patchy:patchy-remediation-controller
# Without a broker (a fake-only values file) nothing is measured.
render usage-no-broker --set attribution.mode=attribute --set agent.runners.claude.enabled=false \
  --set agent.runners.codex.enabled=true
expect usage-no-broker "$usage_cms" ""

# ---- usage rollups (design L2): durable per-user usage ------------------------
# Off renders no UsageRollup grant anywhere (the CRD alone is installed). Under
# attribute each run controller present may create, get and status-update
# UsageRollups (uncached, by name: no list or watch), and remediation-controller,
# which hosts the compaction loop, may also list them; no one may delete one.
# shellcheck disable=SC2016 # $n is a yq variable, not the shell's
usage_rules='select(.kind == "Role") | .metadata.name as $n | .rules[] | select(.resources[0] | test("^usagerollups")) | $n + " " + (.resources | join(",")) + ":" + (.verbs | join(","))'
expect extras-none "$usage_rules" ""
expect attribution "$usage_rules" \
  "patchy-evaluation-controller usagerollups:create,get
patchy-evaluation-controller usagerollups/status:get,update
patchy-intent-controller usagerollups:create,get
patchy-intent-controller usagerollups/status:get,update
patchy-investigation-controller usagerollups:create,get
patchy-investigation-controller usagerollups/status:get,update
patchy-remediation-controller usagerollups:create,get,list
patchy-remediation-controller usagerollups/status:get,update"
expect attribution-core "$usage_rules" \
  "patchy-investigation-controller usagerollups:create,get
patchy-investigation-controller usagerollups/status:get,update
patchy-remediation-controller usagerollups:create,get,list
patchy-remediation-controller usagerollups/status:get,update"
expect attribution 'select(.kind == "Role" or .kind == "ClusterRole") | .rules[] | select(.resources[] | test("usagerollups")) | .verbs[] | select(. == "delete" or . == "deletecollection" or . == "watch")' ""

# ---- NOTES: the agent egress posture as rendered -----------------------------
# The NOTES once said "TCP 443 to anywhere" whatever broadEgress resolved to.
notes_has default-notes 'base NetworkPolicy allows TCP 443 to anywhere' yes
notes np-never --set agent.networkPolicy.broadEgress=never
notes_has np-never 'TCP 443 to anywhere' no
notes_has np-never 'no direct route out of the cluster' yes
notes_has np-never 'plus the cluster DNS resolver' yes
notes np-never-dns-none --set agent.networkPolicy.broadEgress=never --set agent.networkPolicy.dns=none
notes_has np-never-dns-none 'TCP 443' no
notes_has np-never-dns-none 'no direct route out of the cluster' yes
notes_has np-never-dns-none 'plus the cluster DNS resolver' no
notes np-never-codex --set agent.networkPolicy.broadEgress=never --set agent.runners.codex.enabled=true
notes_has np-never-codex 'WARNING: codex is enabled, but no policy lets an agent pod reach a' yes
notes np-off --set agent.networkPolicy.create=false
notes_has np-off 'WARNING: agent.networkPolicy.create is false' yes
notes_has np-off 'TCP 443 to anywhere' no
# With no policy rendered, the DNS note names no policy of this chart's and the
# broadEgress advice (a setting with nothing to act on) is not given.
notes np-off-dns-none --set agent.networkPolicy.create=false --set agent.networkPolicy.dns=none \
  --set agent.networkPolicy.broadEgress=always
notes_has np-off-dns-none 'WARNING: agent.networkPolicy.create is false' yes
notes_has np-off-dns-none 'Agent DNS: none.' yes
notes_has np-off-dns-none 'agent egress policies allow no DNS' no
notes_has np-off-dns-none 'base policy' no
notes_has np-off-dns-none 'broadEgress: never' no
notes np-gke --set agent.networkPolicy.mode=gke
notes_has np-gke 'hostname-enforced via mode gke' yes
notes_has np-gke 'TCP 443 to anywhere' no

# ---- charts/patchy-config: Projects ------------------------------------------
# The CR chart renders .Values.projects into Project CRs verbatim; its values
# schema embeds the CRD's spec schema (hack/codegen.sh), so a malformed entry
# fails the render client-side rather than at the API server.
cfgchart=charts/patchy-config

# render_cfg NAME [helm args...]: render the CR chart into $out/NAME.yaml.
render_cfg() {
  name=$1
  shift
  if ! helm template patchy-config "$cfgchart" --namespace patchy "$@" >"$out/$name.yaml" 2>"$out/$name.err"; then
    fail "$name: render failed: $(cat "$out/$name.err")"
    : >"$out/$name.yaml"
  fi
}

# expect_fail_cfg DESC NEEDLE [helm args...]: the CR chart's render fails and
# its error contains NEEDLE verbatim.
expect_fail_cfg() {
  desc=$1
  needle=$2
  shift 2
  if helm template patchy-config "$cfgchart" --namespace patchy "$@" >/dev/null 2>"$out/guard.err"; then
    fail "config guard $desc: render succeeded, want a failure containing: $needle"
  elif ! grep -qF -- "$needle" "$out/guard.err"; then
    fail "config guard $desc: error lacks '$needle': $(cat "$out/guard.err")"
  fi
}

render_cfg cfg-default
expect cfg-default 'select(.kind == "Project") | .metadata.name' ""
cf=$fixtures/config-projects.yaml
render_cfg cfg-projects -f "$cf"
expect cfg-projects 'select(.kind == "Project") | .metadata.namespace + "/" + .metadata.name' "patchy/target
patchy/docs"
expect cfg-projects 'select(.kind == "Project") | .apiVersion' "patchy.bitwisemedia.uk/v1alpha1
patchy.bitwisemedia.uk/v1alpha1"
expect cfg-projects 'select(.kind == "Project") | .metadata.labels["app.kubernetes.io/name"]' "intent-controller
intent-controller"
# spec is rendered verbatim: the minimal entry gains nothing client-side (the
# CRD defaults it server-side), the full one keeps every value it set.
expect cfg-projects 'select(.kind == "Project" and .metadata.name == "target") | .spec | keys | join(",")' \
  "approvers,intentRepository,repositories"
# (helm's toYaml sorts map keys, so both sides are compared key-sorted.)
for path in intentRepository labels.trigger labels.approve approvers.logins limits checks requireRepositoryImage suspend \
  repositories; do
  want=$(yq eval -o=json -I=0 ".projects[1].spec.$path | sort_keys(..)" "$cf")
  expect cfg-projects \
    "select(.kind == \"Project\" and .metadata.name == \"docs\") | .spec.$path | sort_keys(..) | to_json(0)" "$want"
done
render_cfg cfg-common -f "$cf" --set-json 'commonLabels={"team":"platform"}' \
  --set-json 'commonAnnotations={"owner":"ops"}'
expect cfg-common 'select(.kind == "Project" and .metadata.name == "docs") | .metadata.labels.team + " " + .metadata.annotations.owner' \
  "platform ops"

# The guards: one valid project, then that project with one field broken.
# Each case passes the whole projects list (helm replaces a list wholesale
# rather than merging --set indices into a -f list), and the base renders, so
# every failure below is the broken field's.
base='{"name":"t","spec":{"intentRepository":"https://github.com/acme/intents","approvers":{"logins":["octocat"]},"repositories":[{"name":"t","url":"https://github.com/acme/t"}]}}'
# project JQ: the base project with a yq expression applied, as compact JSON.
project() {
  printf '%s' "$base" | yq eval -p=json -o=json -I=0 "$1" -
}
render_cfg cfg-guard-base --set-json "projects=[$base]"
expect cfg-guard-base 'select(.kind == "Project") | .metadata.name' "t"
expect_fail_cfg "project without a name" "projects/0" \
  --set-json "projects=[$(project 'del(.name)')]"
render_cfg cfg-guard-name --set-json "projects=[$(project '.name = "ppppppppppppppppppppppppp"')]"
expect cfg-guard-name 'select(.kind == "Project") | .metadata.name | length' "25"
expect_fail_cfg "project name over 25 characters" "projects/0/name" \
  --set-json "projects=[$(project '.name = "pppppppppppppppppppppppppp"')]"
expect_fail_cfg "repository key over 16 characters" "projects/0/spec/repositories/0/name" \
  --set-json "projects=[$(project '.spec.repositories[0].name = "kkkkkkkkkkkkkkkkk"')]"
expect_fail_cfg "unknown spec field" "projects/0/spec" \
  --set-json "projects=[$(project '.spec.bogus = true')]"
nine=$(for i in 0 1 2 3 4 5 6 7 8; do printf '{"name": "a%s", "url": "https://github.com/acme/a%s"},' "$i" "$i"; done)
expect_fail_cfg "nine repositories" "projects/0/spec/repositories" \
  --set-json "projects=[$(project ".spec.repositories = [${nine%,}]")]"
expect_fail_cfg "no approvers" "projects/0/spec/approvers/logins" \
  --set-json "projects=[$(project '.spec.approvers.logins = []')]"
expect_fail_cfg "credentials in a repository url" "projects/0/spec/repositories/0/url" \
  --set-json "projects=[$(project '.spec.repositories[0].url = "https://x:token@github.com/acme/t"')]"
expect_fail_cfg "cost ceiling past its cap" "projects/0/spec/limits/maxCostMicroUSD" \
  --set-json "projects=[$(project '.spec.limits.maxCostMicroUSD = 1000000001')]"
# Per-repository previews (slice 3): rendered verbatim, their fields
# checked client-side. The CEL rules (at most four, distinct paths, no
# shorthand beside them) are the API server's; the schema envtest covers them.
webpreview='{"imageRepository":"registry.example/patchy/previews/acme-web","port":8080,"readinessPath":"/healthz"}'
render_cfg cfg-repo-preview --set-json "projects=[$(project ".spec.repositories[0].preview = $webpreview | .spec.repositories[0].preview.path = \"/api\"")]"
expect cfg-repo-preview 'select(.kind == "Project") | .spec.repositories[0].preview.path + " " + .spec.repositories[0].preview.imageRepository' \
  "/api registry.example/patchy/previews/acme-web"
expect_fail_cfg "a preview path outside the grammar" "projects/0/spec/repositories/0/preview/path" \
  --set-json "projects=[$(project ".spec.repositories[0].preview = $webpreview | .spec.repositories[0].preview.path = \"/API\"")]"
expect_fail_cfg "an unknown repository preview field" "projects/0/spec/repositories/0/preview" \
  --set-json "projects=[$(project ".spec.repositories[0].preview = $webpreview | .spec.repositories[0].preview.dockerfile = \"x\"")]"

# A repository picks one of the operator's resource classes by name: rendered
# verbatim; a name no class could have is refused client-side, the same
# DNS-label rule as the patchy chart's class names.
render_cfg cfg-repo-class --set-json "projects=[$(project '.spec.repositories[0].agentResourceClass = "large"')]"
expect cfg-repo-class 'select(.kind == "Project") | .spec.repositories[0].agentResourceClass' "large"
expect_fail_cfg "a resource class that is not a DNS label" "projects/0/spec/repositories/0/agentResourceClass" \
  --set-json "projects=[$(project '.spec.repositories[0].agentResourceClass = "Large"')]"

if [ "$failures" -gt 0 ]; then
  echo "chart-render-test: $failures assertion(s) failed" >&2
  exit 1
fi
echo "chart-render-test: all assertions passed"
