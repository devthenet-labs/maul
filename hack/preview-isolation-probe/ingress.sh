#!/usr/bin/env bash
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# The preview isolation gate's ingress direction (design Revision 1, W9): on
# any cluster, with no cloud API, check that the slot NetworkPolicy lets only
# the preview gateway reach a slot Pod. A netprobe Pod listens in a slot on
# its port http; three client Pods each try to connect to it with the egress
# run's burst. One in the agents namespace and one in the release namespace
# without the gateway label must be refused; one in the release namespace
# with the label (patchy.bitwisemedia.uk/preview-gateway: "true") must be
# admitted, which also proves the listener answers, so the refusals are the
# policy's. See README.md.
set -euo pipefail

usage() {
    cat <<EOF
usage: $0 [options]

Check, from inside the cluster, that a slot Pod admits only the preview
gateway. kubectl's current context must be the cluster. See README.md.

Each option falls back to the environment variable named beside it.
  --image <ref>                 PROBE_IMAGE (required): the netprobe image,
                                pushed under preview-controller's image prefix
                                as <registry>/<path>/<leaf>:sha-<40 hex>, the
                                only shape a slot admits
  --node-selector <k=v[,k=v]>   PROBE_NODE_SELECTOR: the preview nodes' labels
                                (the chart's preview.nodeIsolation.selector);
                                required unless --no-node-isolation
  --taint-key <key>             PROBE_TAINT_KEY: the preview nodes' NoExecute
                                taint key (preview.nodeIsolation.taintKey);
                                required with --node-selector
  --no-node-isolation           the install runs preview.nodeIsolation.mode
                                none: no selector or toleration
  --release-namespace <ns>      PROBE_RELEASE_NAMESPACE: patchy's namespace,
                                where the gateway runs (default patchy)
  --agents-namespace <ns>       PROBE_AGENTS_NAMESPACE: the agents' namespace
                                (default patchy-agents)
  --slot <n>                    PROBE_SLOT: the slot to listen in (default 0)
  --timeout <seconds>           PROBE_TIMEOUT: how long each Pod may take to
                                start or finish (default 300)
  --dry-run                     check every value and print the site, then
                                exit before kubectl is called
  --print-manifests             print the four Pods it would create (the
                                listener's address as 192.0.2.1), then exit
                                before kubectl is called
  -h, --help                    print this help
EOF
}

fail() {
    echo "$1" >&2
    usage >&2
    exit 2
}

image=${PROBE_IMAGE:-}
node_selector=${PROBE_NODE_SELECTOR:-}
taint_key=${PROBE_TAINT_KEY:-}
release_ns=${PROBE_RELEASE_NAMESPACE:-patchy}
agents_ns=${PROBE_AGENTS_NAMESPACE:-patchy-agents}
slot=${PROBE_SLOT:-0}
timeout=${PROBE_TIMEOUT:-300}
isolation=1
dry_run=0
print_manifests=0
while [[ $# -gt 0 ]]; do
    case $1 in
    -h | --help)
        usage
        exit 0
        ;;
    --dry-run) dry_run=1 ;;
    --print-manifests) print_manifests=1 ;;
    --no-node-isolation) isolation=0 ;;
    --image | --node-selector | --taint-key | --release-namespace | --agents-namespace | --slot | --timeout)
        [[ $# -ge 2 ]] || fail "$1 needs a value"
        case $1 in
        --image) image=$2 ;;
        --node-selector) node_selector=$2 ;;
        --taint-key) taint_key=$2 ;;
        --release-namespace) release_ns=$2 ;;
        --agents-namespace) agents_ns=$2 ;;
        --slot) slot=$2 ;;
        --timeout) timeout=$2 ;;
        esac
        shift
        ;;
    *) fail "unknown argument $1" ;;
    esac
    shift
done

# Every value lands in a kubectl argument or in a Pod manifest, so each must
# be exactly the shape it names.
dns_label='[a-z0-9]([-a-z0-9]*[a-z0-9])?'
dns_name="$dns_label(\\.$dns_label)*"
qualified="($dns_name/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?"
label_value='([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?'
[[ -n $image ]] || fail "--image (PROBE_IMAGE) is required"
if [[ ! $image =~ ^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+:sha-[0-9a-f]{40}$ ]]; then
    fail "--image must be <registry>/<path>:sha-<40 hex>, the shape a slot admits, not $image"
fi
for ns in "$release_ns" "$agents_ns"; do
    [[ $ns =~ ^$dns_label$ ]] || fail "namespaces must be DNS labels, not $ns"
done
[[ $slot =~ ^[0-9]+$ ]] || fail "--slot must be a slot number, not $slot"
[[ $timeout =~ ^[1-9][0-9]*$ ]] || fail "--timeout must be a positive number of seconds, not $timeout"
selector_yaml=
if [[ $isolation -eq 1 ]]; then
    [[ -n $node_selector ]] || fail "--node-selector (PROBE_NODE_SELECTOR) is required unless --no-node-isolation"
    [[ -n $taint_key ]] || fail "--taint-key (PROBE_TAINT_KEY) is required with --node-selector"
    [[ $taint_key =~ ^$qualified$ ]] || fail "--taint-key must be a taint key, [<dns prefix>/]<name>, not $taint_key"
    IFS=, read -r -a pairs <<<"$node_selector"
    for pair in "${pairs[@]}"; do
        if [[ ! $pair =~ ^($qualified)=($label_value)$ ]]; then
            fail "--node-selector must be <label>=<value>[,<label>=<value>], not $node_selector"
        fi
        selector_yaml+="    ${pair%%=*}: \"${pair#*=}\""$'\n'
    done
elif [[ -n $node_selector || -n $taint_key ]]; then
    fail "--no-node-isolation takes no --node-selector or --taint-key"
fi
slot_ns="patchy-preview-$slot"
listener=preview-ingress-listener
clients=(preview-ingress-agent preview-ingress-release preview-ingress-gateway)

# pod NAMESPACE NAME ARGS LABELS: one netprobe Pod in the slot Pod's shape
# (the shape the slot admission policies require, which every namespace
# admits). LABELS are extra label lines; only the listener selects and
# tolerates the preview nodes.
pod() {
    local ns=$1 name=$2 args=$3 labels=${4:-} placement=
    if [[ $ns == "$slot_ns" && $isolation -eq 1 ]]; then
        placement="  nodeSelector:
$selector_yaml  tolerations:
    - {key: $taint_key, operator: Equal, value: \"true\", effect: NoExecute}
"
    fi
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $ns
  labels:
    app: $name
$labels
spec:
  restartPolicy: Never
  serviceAccountName: default
  automountServiceAccountToken: false
$placement  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile: {type: RuntimeDefault}
  containers:
    - name: probe
      image: $image
      imagePullPolicy: IfNotPresent
      args: [$args]
      ports:
        - {name: http, containerPort: 8080, protocol: TCP}
      resources:
        requests: {cpu: 25m, memory: 32Mi}
        limits: {cpu: 250m, memory: 256Mi}
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities: {drop: [ALL]}
EOF
}

# client NAME: the client Pod NAME, connecting to ip.
client() {
    local target="connect, \"$1:8080\""
    case $2 in
    preview-ingress-agent) pod "$agents_ns" "$2" "$target" ;;
    preview-ingress-release) pod "$release_ns" "$2" "$target" ;;
    preview-ingress-gateway) pod "$release_ns" "$2" "$target" '    patchy.bitwisemedia.uk/preview-gateway: "true"' ;;
    esac
}

if [[ $print_manifests -eq 1 ]]; then
    pod "$slot_ns" "$listener" listen
    for name in "${clients[@]}"; do
        echo ---
        client 192.0.2.1 "$name"
    done
    exit 0
fi
if [[ $dry_run -eq 1 ]]; then
    cat <<EOF
image:       $image
slot:        $slot_ns (listener on port http, 8080)
placement:   $(if [[ $isolation -eq 1 ]]; then echo "$node_selector, tolerating $taint_key"; else echo "none (nodeIsolation none)"; fi)
refused:     $agents_ns/preview-ingress-agent, $release_ns/preview-ingress-release
admitted:    $release_ns/preview-ingress-gateway (patchy.bitwisemedia.uk/preview-gateway=true)
dry run: nothing was called
EOF
    exit 0
fi

kube() { kubectl --request-timeout=60s "$@"; }

created=0
cleanup() {
    if [[ $created -eq 1 ]]; then
        kube delete pod "$listener" -n "$slot_ns" --ignore-not-found --wait=false >&2 || true
        kube delete pod preview-ingress-agent -n "$agents_ns" --ignore-not-found --wait=false >&2 || true
        kube delete pod preview-ingress-release preview-ingress-gateway -n "$release_ns" --ignore-not-found \
            --wait=false >&2 || true
    fi
}
trap cleanup EXIT

# The slot policy must exist and name the gateway's label as a peer, or the
# admitted client would fail for a reason this run is not about.
policy=$(kube get networkpolicy preview-isolation -n "$slot_ns" -o json)
if ! jq -e '[.spec.ingress[]?.from[]? | .podSelector.matchLabels["patchy.bitwisemedia.uk/preview-gateway"]?] |
    index("true") != null' <<<"$policy" >/dev/null; then
    echo "$slot_ns/preview-isolation admits no preview gateway Pod: is previewAuth.gateway.enabled set?" >&2
    exit 1
fi
# An ipBlock peer admits every address in it (the preview ALB's subnets, while
# preview.legacyIngress serves), and so whatever else runs there, past the
# gateway's sign-in. This run's clients are never in it, so say so plainly.
blocks=$(jq -r '[.spec.ingress[]?.from[]? | .ipBlock.cidr? // empty] | join(", ")' <<<"$policy")
if [[ -n $blocks ]]; then
    echo "WARNING: $slot_ns/preview-isolation also admits the address ranges $blocks on port http, bypassing the" \
        "preview gateway; this run does not test them. They go once preview.legacyIngress is false." >&2
fi
for target in "$slot_ns/$listener" "$agents_ns/preview-ingress-agent" "$release_ns/preview-ingress-release" \
    "$release_ns/preview-ingress-gateway"; do
    if kube get pod "${target#*/}" -n "${target%%/*}" >/dev/null 2>&1; then
        echo "existing Pod $target: refusing to overwrite another probe" >&2
        exit 1
    fi
done

created=1
pod "$slot_ns" "$listener" listen | kube create -f - >&2
kube wait --for=condition=Ready "pod/$listener" -n "$slot_ns" --timeout="${timeout}s" >&2
ip=$(kube get pod "$listener" -n "$slot_ns" -o jsonpath='{.status.podIP}')
if [[ ! $ip =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "the listener $slot_ns/$listener has no IPv4 address ($ip)" >&2
    exit 1
fi
echo "listener $slot_ns/$listener at $ip:8080" >&2
for name in "${clients[@]}"; do
    client "$ip" "$name" | kube create -f - >&2
done

# verdict NAMESPACE NAME: the client's summary once it has finished.
verdict() {
    local ns=$1 name=$2 phase tries=0 logs
    while ((tries < timeout / 5)); do
        phase=$(kube get pod "$name" -n "$ns" -o jsonpath='{.status.phase}')
        if [[ $phase == Succeeded || $phase == Failed ]]; then
            logs=$(kube logs "$name" -n "$ns" -c probe)
            jq -r 'select(.name == "summary") | .outcome' <<<"$logs" | tail -1
            return 0
        fi
        sleep 5
        tries=$((tries + 1))
    done
    echo "TIMEOUT"
}

failed=0
check() {
    local ns=$1 name=$2 want=$3 got
    got=$(verdict "$ns" "$name")
    if [[ $got == "$want" ]]; then
        echo "PASS $ns/$name: $got" >&2
    else
        echo "FAIL $ns/$name: $got, want $want" >&2
        failed=1
    fi
}
check "$agents_ns" preview-ingress-agent BLOCKED
check "$release_ns" preview-ingress-release BLOCKED
check "$release_ns" preview-ingress-gateway REACHED
if [[ $failed -ne 0 ]]; then
    echo "The slot policy does not admit exactly the preview gateway; keep previews unused and investigate." >&2
    exit 1
fi
echo "Ingress isolation passed: only the preview gateway's label reaches $slot_ns." >&2
