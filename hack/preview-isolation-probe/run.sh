#!/usr/bin/env bash
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Re-run the first-instant EKS Auto Mode isolation gate against an immutable
# disposable PR image. This never reads a metadata response body or a token.
#
# The site is the caller's to name, as a flag or in the environment (see
# README.md and --help); nothing about any one site is written in here. AWS
# credentials (AWS_PROFILE and the rest) come from the caller's environment
# as they are; the script sets none.
#
# `run.sh edges` is a separate run on any cluster, with no cloud API:
# acceptance verification's edge policies admit a Preview's declared edges
# and nothing else (see edges_main below and README.md).
set -euo pipefail

usage() {
    cat <<EOF
usage: $0 [options] <open disposable PR number>
       $0 edges [options]   (the edges run; $0 edges --help)

Re-run the preview cold-start isolation gate against the image the trusted
publisher pushed for a disposable test/preview-* PR's head. See README.md.

Each option falls back to the environment variable named beside it.
  --repository <owner>/<name>   PROBE_REPOSITORY (required): the app
                                repository the disposable PR is open on
  --image <host>/<path>         PROBE_IMAGE (required): the ECR repository its
                                trusted publisher pushes preview images to,
                                <account>.dkr.ecr.<region>.amazonaws.com/
                                <preview path prefix>/<app>; the registry
                                host, its account and region are read from it
  --taint-key <key>             PROBE_TAINT_KEY (required): the preview
                                NodePool's NoExecute taint key (the chart's
                                preview.nodeIsolation.taintKey)
  --node-pool <name>            PROBE_NODE_POOL: the preview NodePool (the
                                chart's preview.nodeIsolation.nodePool;
                                default maul-preview)
  --node-class <name>           PROBE_NODE_CLASS: the preview NodeClass (the
                                chart's preview.nodeIsolation.nodeClass;
                                default maul-preview)
  --dry-run                     check every value and print the site the run
                                would probe, then exit before gh, aws or
                                kubectl is called
  -h, --help                    print this help

The disposable PR must be open into the repository's default branch, which
is read from GitHub rather than taken as an option.
EOF
}

# fail MESSAGE: a usage error, with the help on stderr.
fail() {
    echo "$1" >&2
    usage >&2
    exit 2
}

# wait_zero: wait until the preview NodePool $node_pool has no node and no
# NodeClaim, so the next run starts on a cold pool.
wait_zero() {
    local tries=0
    while (( tries < 120 )); do
        local nodes claims
        nodes=$(kubectl get nodes -l "karpenter.sh/nodepool=$node_pool" -o name)
        claims=$(kubectl get nodeclaims -o json | jq -r --arg pool "$node_pool" \
            '.items[] | select(.metadata.labels["karpenter.sh/nodepool"] == $pool) | .metadata.name')
        if [[ -z $nodes && -z $claims ]]; then
            return 0
        fi
        sleep 10
        tries=$((tries + 1))
    done
    echo "preview pool did not return to zero within 20 minutes" >&2
    return 1
}

# The edges run (acceptance verification, spec §9): that the per-part
# NetworkPolicies preview-controller renders admit exactly a Preview's
# declared edges. In a free slot it creates edges.yaml's policies (the
# controller's render for a Preview whose tester calls web, api and render;
# internal/controller/preview/probe_edges_test.go holds the file to it), a
# netprobe listener for each part they select, carrying the fixture's
# Preview UID and the part, a Service for each component, and a listener in
# the other slot. Each check is a connect exec'd in a stand-in, so it is
# that Pod's traffic: every declared edge must connect through the called
# part's Service; every other pair of parts, every part to the other slot,
# and the tester to the API server, maul's Services and the instance
# metadata service must be refused. A refusal (ECONNREFUSED) is
# inconclusive unless --refused-is-blocked, and then counts only against a
# listener that answered on its own loopback before and after the checks
# and never restarted. Any cluster, no cloud API; see README.md.

edges_usage() {
    cat <<EOF
usage: $0 edges [options]

Check, in a free slot, that acceptance verification's edge policies admit
each declared edge between a Preview's parts and refuse every other pair,
the other slot, and the tester's way to the API server, maul's Services and
the instance metadata service. kubectl's current context must be the
cluster. Calls neither gh nor aws. See README.md.

Each option falls back to the environment variable named beside it.
  --image <ref>                 PROBE_IMAGE (required): the netprobe image,
                                under preview-controller's image prefix as
                                <registry>/<path>/<leaf>:sha-<40 hex>
  --node-selector <k=v[,k=v]>   PROBE_NODE_SELECTOR: the preview nodes' labels
                                (preview.nodeIsolation.selector); required
                                unless --no-node-isolation
  --taint-key <key>             PROBE_TAINT_KEY: the preview nodes' NoExecute
                                taint key; required with --node-selector
  --no-node-isolation           the install runs preview.nodeIsolation.mode
                                none: no selector or toleration
  --refused-is-blocked          count a refused connection as blocked, for a
                                policy controller that REJECTs (k3s, k3d);
                                only against a listener that answers on its
                                own loopback before and after and never
                                restarted
  --slots <a,b,a>               PROBE_SLOTS: the slots to run in, in order:
                                two distinct slots, each run's other slot the
                                other one (default 0,1,0)
  --cold-node-pool <name>       PROBE_COLD_NODE_POOL: wait for this Karpenter
                                NodePool to have no node or claim before each
                                run (EKS Auto Mode); omit where nodes stay
  --release-namespace <ns>      PROBE_RELEASE_NAMESPACE: maul's namespace
                                (release maul), whose Services the tester
                                must not reach (default maul)
  --timeout <seconds>           PROBE_TIMEOUT: how long each stand-in may take
                                to start, and each declared edge to connect
                                (default 300)
  --dry-run                     check every value and print the site, then
                                exit before kubectl is called
  --print-manifests             print the Services and Deployments a run in
                                the first slot creates, then exit before
                                kubectl is called
  -h, --help                    print this help
EOF
}

edges_fail() {
    echo "$1" >&2
    edges_usage >&2
    exit 2
}

# edges_set KEY VALUE and edges_get KEY: a small map of per-run facts
# (addresses, Pod names), keyed by part or app name, without bash 4's
# associative arrays.
edges_set() { printf -v "edges_v_${1//-/_}" '%s' "$2"; }
edges_get() {
    local var="edges_v_${1//-/_}"
    printf '%s' "${!var:-}"
}

edges_main() {
    image=${PROBE_IMAGE:-}
    local node_selector=${PROBE_NODE_SELECTOR:-} taint_key=${PROBE_TAINT_KEY:-}
    local slots=${PROBE_SLOTS:-0,1,0} release_ns=${PROBE_RELEASE_NAMESPACE:-maul}
    local isolation=1 dry_run=0 print_manifests=0
    node_pool=${PROBE_COLD_NODE_POOL:-}
    timeout=${PROBE_TIMEOUT:-300}
    refused_blocks=0
    while [[ $# -gt 0 ]]; do
        case $1 in
        -h | --help)
            edges_usage
            exit 0
            ;;
        --dry-run) dry_run=1 ;;
        --print-manifests) print_manifests=1 ;;
        --no-node-isolation) isolation=0 ;;
        --refused-is-blocked) refused_blocks=1 ;;
        --image | --node-selector | --taint-key | --slots | --cold-node-pool | --release-namespace | --timeout)
            [[ $# -ge 2 ]] || edges_fail "$1 needs a value"
            case $1 in
            --image) image=$2 ;;
            --node-selector) node_selector=$2 ;;
            --taint-key) taint_key=$2 ;;
            --slots) slots=$2 ;;
            --cold-node-pool) node_pool=$2 ;;
            --release-namespace) release_ns=$2 ;;
            --timeout) timeout=$2 ;;
            esac
            shift
            ;;
        *) edges_fail "unknown argument $1" ;;
        esac
        shift
    done

    # Every value lands in a kubectl argument or in a manifest, so each must
    # be exactly the shape it names.
    local dns_label='[a-z0-9]([-a-z0-9]*[a-z0-9])?'
    local dns_name="$dns_label(\\.$dns_label)*"
    local qualified="($dns_name/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?"
    local label_value='([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?'
    [[ -n $image ]] || edges_fail "--image (PROBE_IMAGE) is required"
    if [[ ! $image =~ ^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+:sha-[0-9a-f]{40}$ ]]; then
        edges_fail "--image must be <registry>/<path>:sha-<40 hex>, the shape a slot admits, not $image"
    fi
    [[ $release_ns =~ ^$dns_label$ ]] || edges_fail "--release-namespace must be a DNS label, not $release_ns"
    if [[ -n $node_pool && ! $node_pool =~ ^$dns_name$ ]]; then
        edges_fail "--cold-node-pool must be a Kubernetes object name, not $node_pool"
    fi
    [[ $timeout =~ ^[1-9][0-9]*$ ]] || edges_fail "--timeout must be a positive number of seconds, not $timeout"
    [[ $slots =~ ^[0-9]+(,[0-9]+)*$ ]] || edges_fail "--slots must be slot numbers separated by commas, not $slots"
    local -a order
    IFS=, read -r -a order <<<"$slots"
    local distinct
    distinct=$(printf '%s\n' "${order[@]}" | sort -u)
    [[ $(wc -l <<<"$distinct") -eq 2 ]] || edges_fail "--slots must name exactly two distinct slots, not $slots"
    read -r -a edges_pair <<<"$(tr '\n' ' ' <<<"$distinct")"
    placement=
    if [[ $isolation -eq 1 ]]; then
        [[ -n $node_selector ]] || edges_fail "--node-selector (PROBE_NODE_SELECTOR) is required unless --no-node-isolation"
        [[ -n $taint_key ]] || edges_fail "--taint-key (PROBE_TAINT_KEY) is required with --node-selector"
        [[ $taint_key =~ ^$qualified$ ]] || edges_fail "--taint-key must be a taint key, [<dns prefix>/]<name>, not $taint_key"
        local -a pairs
        IFS=, read -r -a pairs <<<"$node_selector"
        placement="      nodeSelector:"$'\n'
        local pair
        for pair in "${pairs[@]}"; do
            if [[ ! $pair =~ ^($qualified)=($label_value)$ ]]; then
                edges_fail "--node-selector must be <label>=<value>[,<label>=<value>], not $node_selector"
            fi
            placement+="        ${pair%%=*}: \"${pair#*=}\""$'\n'
        done
        placement+="      tolerations:"$'\n'
        placement+="        - {key: $taint_key, operator: Equal, value: \"true\", effect: NoExecute}"$'\n'
    elif [[ -n $node_selector || -n $taint_key ]]; then
        edges_fail "--no-node-isolation takes no --node-selector or --taint-key"
    fi
    edges_fixture

    if [[ $print_manifests -eq 1 ]]; then
        edges_manifests "maul-preview-${order[0]}" "maul-preview-$(edges_other "${order[0]}")"
        exit 0
    fi
    if [[ $dry_run -eq 1 ]]; then
        local s names=() joined
        for s in "${order[@]}"; do names+=("maul-preview-$s"); done
        joined=$(IFS=,; echo "${names[*]}")
        cat <<EOF
image:       $image
runs:        ${joined//,/, }
fixture:     $fixture (Preview UID $edges_uid; parts ${edges_parts[*]})
placement:   $(if [[ $isolation -eq 1 ]]; then echo "$node_selector, tolerating $taint_key"; else echo "none (nodeIsolation none)"; fi)
cold pool:   $(if [[ -n $node_pool ]]; then echo "$node_pool, empty before each run"; else echo "not waited for"; fi)
refusal:     $(if [[ $refused_blocks -eq 1 ]]; then echo "blocked, from a listener answering on its loopback"; else echo "inconclusive"; fi)
assumed:     release maul in namespace $release_ns
dry run: nothing was called
EOF
        exit 0
    fi

    trap edges_cleanup EXIT
    local slot
    for slot in "${order[@]}"; do
        if [[ -n $node_pool ]]; then
            wait_zero
        fi
        edges_run "maul-preview-$slot" "maul-preview-$(edges_other "$slot")" "$release_ns"
    done
    echo "Edges isolation passed in ${order[*]}: each declared edge connected and every other pair was refused." >&2
}

# edges_other SLOT: the other slot of the pair.
edges_other() {
    if [[ $1 == "${edges_pair[0]}" ]]; then echo "${edges_pair[1]}"; else echo "${edges_pair[0]}"; fi
}

# edges_fixture reads edges.yaml's parts (from its policies' names) and its
# one Preview UID, which the stand-ins are labelled with.
edges_fixture() {
    fixture="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/edges.yaml"
    read -r -a edges_parts <<<"$(sed -n 's/^  name: preview-edges-np-\([a-z0-9-]*\)$/\1/p' "$fixture" | tr '\n' ' ')"
    edges_uid=$(sed -n 's/^ *maul\.run\/preview-uid: \(.*\)$/\1/p' "$fixture" | sort -u)
    if [[ ${#edges_parts[@]} -lt 2 || ! $edges_uid =~ ^[0-9a-f-]{36}$ ]] || ! printf '%s\n' "${edges_parts[@]}" |
        grep -qx tester; then
        echo "$fixture must name a tester and another part, and one Preview UID" >&2
        exit 1
    fi
}

# edges_service NAMESPACE PART: the component's Service, port 80 to the
# stand-in's 8080, as a component Service is.
edges_service() {
    cat <<EOF
apiVersion: v1
kind: Service
metadata:
  name: preview-edges-$2
  namespace: $1
spec:
  type: ClusterIP
  selector: {app: preview-edges-$2}
  ports:
    - {name: http, port: 80, targetPort: 8080, protocol: TCP}
EOF
}

# edges_deployment NAMESPACE NAME [PART]: a Deployment-managed netprobe
# listener in the slot Pod's shape; a part's carries the fixture's Preview
# UID and the part, the two labels its policies select.
edges_deployment() {
    local labels="        app: $2"
    if [[ -n ${3:-} ]]; then
        labels+=$'\n'"        maul.run/preview-uid: $edges_uid"$'\n'"        maul.run/preview-part: $3"
    fi
    cat <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $2
  namespace: $1
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector:
    matchLabels: {app: $2}
  template:
    metadata:
      labels:
$labels
    spec:
      serviceAccountName: default
      automountServiceAccountToken: false
$placement      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: probe
          image: $image
          imagePullPolicy: IfNotPresent
          args: [listen]
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

# edges_manifests SLOT_NS OTHER_NS: every object a run creates but the
# fixture's policies, Services first.
edges_manifests() {
    local part sep=
    for part in "${edges_parts[@]}"; do
        if [[ $part != tester ]]; then
            printf '%s' "$sep"
            edges_service "$1" "$part"
            sep=$'---\n'
        fi
    done
    for part in "${edges_parts[@]}"; do
        printf '%s' "$sep"
        edges_deployment "$1" "preview-edges-$part" "$part"
    done
    printf '%s' "$sep"
    edges_deployment "$2" preview-edges-other
}

ekube() { kubectl --request-timeout=60s "$@"; }

edges_created=
edges_cleanup() {
    [[ -n $edges_created ]] || return 0
    local ns=${edges_created% *} ons=${edges_created#* } part names=() services=()
    for part in "${edges_parts[@]}"; do
        names+=("preview-edges-$part")
        [[ $part == tester ]] || services+=("preview-edges-$part")
    done
    ekube delete deployment "${names[@]}" -n "$ns" --ignore-not-found --cascade=foreground --wait=true >&2 || true
    ekube delete deployment preview-edges-other -n "$ons" --ignore-not-found --cascade=foreground --wait=true >&2 ||
        true
    ekube delete service "${services[@]}" -n "$ns" --ignore-not-found --wait=true >&2 || true
    ekube delete networkpolicy -n "$ns" -l app.kubernetes.io/managed-by=maul-isolation-probe --ignore-not-found \
        --wait=true >&2 || true
    edges_created=
}

# edges_verdict NAMESPACE POD TARGET [FLAG]: the summary of a connect to
# TARGET exec'd in POD (with FLAG before it), NO_SUMMARY when it printed
# none.
edges_verdict() {
    local ns=$1 pod=$2 out got args=()
    [[ -z ${4:-} ]] || args+=("$4")
    args+=("$3")
    out=$(ekube exec -n "$ns" "$pod" -c probe -- /probe connect "${args[@]}" </dev/null 2>/dev/null) || out=
    got=$(jq -Rr 'fromjson? | select(type == "object" and .name == "summary") | .outcome // empty' <<<"$out" |
        tail -1) || got=
    echo "${got:-NO_SUMMARY}"
}

# edges_pod NAMESPACE APP: "<name> <IPv4> <restarts>" of APP's one running
# Pod, or nothing.
edges_pod() {
    local line
    line=$(ekube get pods -n "$1" -l "app=$2" --field-selector=status.phase=Running -o \
        jsonpath='{range .items[*]}{.metadata.name} {.status.podIP} {.status.containerStatuses[0].restartCount}{"\n"}{end}')
    if [[ $(grep -c . <<<"$line") -eq 1 && $line =~ ^[a-z0-9.-]+\ [0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\ [0-9]+$ ]]; then
        echo "$line"
    fi
}

# edges_ipv4 VALUE WHAT: VALUE when it is an IPv4 address, else stop.
edges_ipv4() {
    if [[ ! $1 =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "could not discover $2's address ($1)" >&2
        exit 1
    fi
    echo "$1"
}

# edges_run SLOT_NS OTHER_NS RELEASE_NS: one run (see above); stops the
# script on any failure, after cleaning up.
edges_run() {
    local ns=$1 ons=$2 rel=$3 part app n held failed=0
    echo "Edges run in $ns (other slot $ons)" >&2
    if ! ekube get networkpolicy preview-isolation -n "$ns" -o name >/dev/null; then
        echo "there is no slot NetworkPolicy $ns/preview-isolation: is the slot created (preview.enabled)?" >&2
        exit 1
    fi
    for n in "$ns" "$ons"; do
        held=$(ekube get deployments -n "$n" -o name)
        if [[ -n $held ]]; then
            echo "$n is not free: it holds $(tr '\n' ' ' <<<"$held"); run the probe while no Preview holds it" >&2
            exit 1
        fi
    done

    edges_created="$ns $ons"
    ekube create -n "$ns" -f "$fixture" >&2
    for part in "${edges_parts[@]}"; do
        [[ $part == tester ]] || edges_service "$ns" "$part" | ekube create -f - >&2
    done
    # The declared edges, as the policies the API server holds say: a pair
    # whose caller's egress and callee's ingress both name it, on a port.
    local policies declared
    policies=$(ekube get networkpolicy -n "$ns" -l app.kubernetes.io/managed-by=maul-isolation-probe -o json)
    declared=$(jq -r --arg uid "$edges_uid" '
        [.items[] | select(.spec.podSelector.matchLabels["maul.run/preview-uid"] == $uid) |
         {part: .spec.podSelector.matchLabels["maul.run/preview-part"],
          out: [.spec.egress[]? | . as $r | .to[] | .podSelector.matchLabels["maul.run/preview-part"] as $to |
                $r.ports[] | "\($to) \(.port)"],
          in: [.spec.ingress[]? | . as $r | .from[] | .podSelector.matchLabels["maul.run/preview-part"] as $f |
               $r.ports[] | "\($f) \(.port)"]}] as $ps |
        $ps[] as $a | $a.out[] | split(" ") as [$to, $port] |
        select(any($ps[]; .part == $to and any(.in[]; . == "\($a.part) \($port)"))) | "\($a.part) \($to) \($port)"
    ' <<<"$policies")
    local got_parts
    got_parts=$(jq -r '[.items[].spec.podSelector.matchLabels["maul.run/preview-part"]] | sort | join(" ")' \
        <<<"$policies")
    if [[ $got_parts != "$(printf '%s\n' "${edges_parts[@]}" | sort | tr '\n' ' ' | sed 's/ $//')" ||
        -z $declared ]] || grep -qv ' 8080$' <<<"$declared"; then
        echo "the fixture's policies in $ns do not read back as $fixture's parts with edges on 8080" \
            "(parts: $got_parts; edges: $(tr '\n' ',' <<<"$declared"))" >&2
        exit 1
    fi

    for part in "${edges_parts[@]}"; do
        edges_deployment "$ns" "preview-edges-$part" "$part" | ekube create -f - >&2
    done
    edges_deployment "$ons" preview-edges-other | ekube create -f - >&2
    for part in "${edges_parts[@]}"; do
        ekube wait --for=condition=Available "deployment/preview-edges-$part" -n "$ns" --timeout="${timeout}s" >&2
    done
    ekube wait --for=condition=Available deployment/preview-edges-other -n "$ons" --timeout="${timeout}s" >&2

    # Who is where: each stand-in's one running Pod, each component's
    # Service, and the targets outside the slot.
    local -a apps=() outside=()
    for part in "${edges_parts[@]}"; do apps+=("$ns preview-edges-$part $part"); done
    apps+=("$ons preview-edges-other other-slot")
    local entry where pod
    for entry in "${apps[@]}"; do
        read -r where app part <<<"$entry"
        pod=$(edges_pod "$where" "$app")
        if [[ -z $pod ]]; then
            echo "$where/$app has no one running Pod with an IPv4 address" >&2
            exit 1
        fi
        edges_set "pod-$part" "$pod"
    done
    local name svc port ip
    for part in "${edges_parts[@]}"; do
        if [[ $part != tester ]]; then
            ip=$(ekube get service "preview-edges-$part" -n "$ns" -o jsonpath='{.spec.clusterIP}')
            ip=$(edges_ipv4 "$ip" "the Service $ns/preview-edges-$part")
            edges_set "svc-$part" "$ip"
        fi
    done
    for entry in "kubernetes-api default kubernetes 443" "maul-egress-broker $rel maul-egress-broker 8080" \
        "maul-integration-controller $rel maul-integration-controller 8080" \
        "maul-source-controller $rel maul-source-controller 9790" "maul-status-server $rel maul-status-server 8080"; do
        read -r name where svc port <<<"$entry"
        ip=$(ekube get service "$svc" -n "$where" -o jsonpath='{.spec.clusterIP}')
        ip=$(edges_ipv4 "$ip" "the Service $where/$svc")
        outside+=("$name $ip:$port")
    done
    outside+=("imds 169.254.169.254:80")

    # check FROM TO TARGET WANT [FLAG]: one connect from FROM's Pod.
    check() {
        local from=$1 to=$2 target=$3 want=$4 flag=${5:-} got deadline=$((SECONDS + timeout))
        local pod
        pod=$(edges_get "pod-$from")
        pod=${pod%% *}
        while :; do
            got=$(edges_verdict "$(edges_ns "$from")" "$pod" "$target" "$flag")
            [[ $want == REACHED && $got != REACHED && $SECONDS -lt $deadline ]] || break
            sleep 2
        done
        local label="$from -> $to ($target)"
        [[ $to == "$from" ]] && label="$from: its own listener ($target)"
        if [[ $got == "$want" ]]; then
            echo "PASS $(edges_ns "$from") $label: $got" >&2
        else
            echo "FAIL $(edges_ns "$from") $label: $got, want $want" >&2
            failed=1
        fi
    }
    edges_ns() { if [[ $1 == other-slot ]]; then echo "$ons"; else echo "$ns"; fi; }
    alive() {
        local p
        for p in "${edges_parts[@]}" other-slot; do check "$p" "$p" 127.0.0.1:8080 REACHED; done
    }

    local flag='' a b
    [[ $refused_blocks -eq 0 ]] || flag=--refused-is-blocked
    alive
    while read -r a b port; do
        if [[ -n $(edges_get "svc-$b") ]]; then
            check "$a" "$b" "$(edges_get "svc-$b"):80" REACHED
        else
            check "$a" "$b" "$(edges_get "pod-$b" | cut -d' ' -f2):$port" REACHED
        fi
    done <<<"$declared"
    for a in "${edges_parts[@]}"; do
        for b in "${edges_parts[@]}"; do
            if [[ $a != "$b" ]] && ! grep -q "^$a $b " <<<"$declared"; then
                check "$a" "$b" "$(edges_get "pod-$b" | cut -d' ' -f2):8080" BLOCKED "$flag"
            fi
        done
        check "$a" other-slot "$(edges_get pod-other-slot | cut -d' ' -f2):8080" BLOCKED "$flag"
    done
    for entry in "${outside[@]}"; do
        check tester "${entry%% *}" "${entry#* }" BLOCKED "$flag"
    done
    alive
    # The same Pods, never restarted: a refusal counted as blocked was
    # never a restarting listener's own.
    for entry in "${apps[@]}"; do
        read -r where app part <<<"$entry"
        if [[ $(edges_pod "$where" "$app") != "$(edges_get "pod-$part")" ]]; then
            echo "FAIL $where $part: its Pod restarted or was replaced during the run" >&2
            failed=1
        fi
    done
    if [[ $failed -ne 0 ]]; then
        echo "The edge policies in $ns do not admit exactly the declared edges; keep verification off and" \
            "investigate." >&2
        exit 1
    fi
    edges_cleanup
}

if [[ ${1:-} == edges ]]; then
    shift
    edges_main "$@"
    exit 0
fi

repo=${PROBE_REPOSITORY:-}
image=${PROBE_IMAGE:-}
taint_key=${PROBE_TAINT_KEY:-}
node_pool=${PROBE_NODE_POOL:-maul-preview}
node_class=${PROBE_NODE_CLASS:-maul-preview}
dry_run=0
pr_number=
while [[ $# -gt 0 ]]; do
    case $1 in
    -h | --help)
        usage
        exit 0
        ;;
    --dry-run) dry_run=1 ;;
    --repository | --image | --taint-key | --node-pool | --node-class)
        [[ $# -ge 2 ]] || fail "$1 needs a value"
        case $1 in
        --repository) repo=$2 ;;
        --image) image=$2 ;;
        --taint-key) taint_key=$2 ;;
        --node-pool) node_pool=$2 ;;
        --node-class) node_class=$2 ;;
        esac
        shift
        ;;
    -*) fail "unknown option $1" ;;
    *)
        [[ -z $pr_number ]] || fail "one PR number only, not $pr_number and $1"
        pr_number=$1
        ;;
    esac
    shift
done

[[ $pr_number =~ ^[0-9]+$ ]] || fail "the open disposable PR's number is required"
[[ -n $repo ]] || fail "--repository (PROBE_REPOSITORY) is required"
[[ -n $image ]] || fail "--image (PROBE_IMAGE) is required"
[[ -n $taint_key ]] || fail "--taint-key (PROBE_TAINT_KEY) is required"
# Each value lands in a gh, aws or kubectl argument or in the probe's
# manifest, so each must be exactly the shape it names.
if [[ ! $repo =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
    fail "--repository must be <owner>/<name>, not $repo"
fi
if [[ ! $image =~ ^([0-9]{12}\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com)/([a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*)$ ]]; then
    fail "--image must be an ECR repository, <account>.dkr.ecr.<region>.amazonaws.com/<path>, not $image"
fi
registry=${BASH_REMATCH[1]}
region=${BASH_REMATCH[2]}
image_repo=${BASH_REMATCH[3]}
# The account the registry belongs to, which the image lookup is bound to:
# the cluster pulls from that account, whichever account the caller's AWS
# credentials are in.
account=${registry%%.*}
dns_name='[a-z0-9]([-a-z0-9]*[a-z0-9])?'
if [[ ! $taint_key =~ ^($dns_name(\.$dns_name)*/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$ ]]; then
    fail "--taint-key must be a taint key, [<dns prefix>/]<name>, not $taint_key"
fi
for value in "$node_pool" "$node_class"; do
    if [[ ! $value =~ ^$dns_name(\.$dns_name)*$ ]]; then
        fail "--node-pool and --node-class must be Kubernetes object names, not $value"
    fi
done

if [[ $dry_run -eq 1 ]]; then
    cat <<EOF
repository:  $repo (PR #$pr_number, into its default branch)
registry:    $registry (account $account, region $region)
image:       $registry/$image_repo:sha-<PR head SHA>
taint key:   $taint_key
node pool:   $node_pool
node class:  $node_class
assumed:     release maul in namespace maul; slots maul-preview-0 and maul-preview-1
dry run: nothing was called
EOF
    exit 0
fi
deployment=preview-isolation-probe
probe_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# The sibling stage's targets: another component's Pod and Service in the
# probe's slot, and a Pod in the other slot. The Service is
# sibling-service.yaml, which names it this too.
sibling=preview-isolation-sibling
other=preview-isolation-other
created=0

cleanup() {
    if [[ $created -eq 1 ]]; then
        for slot in 0 1; do
            for name in "$deployment" "$sibling" "$other"; do
                kubectl delete deployment "$name" -n "maul-preview-$slot" --ignore-not-found --wait=true >&2 || true
            done
            kubectl delete service "$sibling" -n "maul-preview-$slot" --ignore-not-found --wait=true >&2 || true
        done
    fi
}
trap cleanup EXIT

# The PR must be into the repository's default branch, the one its previews
# are built against; it is the repository's own setting, so it is read here.
base=$(gh repo view "$repo" --json defaultBranchRef | jq -r '.defaultBranchRef.name // empty')
if [[ -z $base ]]; then
    echo "could not read the default branch of $repo" >&2
    exit 1
fi
pr_json=$(gh pr view "$pr_number" --repo "$repo" --json state,isCrossRepository,headRefOid,headRefName,baseRefName)
if ! jq -e --arg base "$base" '.state == "OPEN" and .isCrossRepository == false and .baseRefName == $base and
    (.headRefName | startswith("test/preview-")) and (.headRefOid | test("^[0-9a-f]{40}$"))' <<<"$pr_json" >/dev/null; then
    echo "PR is not an open, same-repository disposable test/preview-* PR into $base" >&2
    exit 1
fi
sha=$(jq -r '.headRefOid' <<<"$pr_json")
# --registry-id: look in the account named in --image, the one the probe Pod
# pulls from, not the default registry of the caller's own account.
digest=$(aws ecr describe-images --registry-id "$account" --region "$region" --repository-name "$image_repo" \
    --image-ids "imageTag=sha-$sha" --query 'imageDetails[0].imageDigest' --output text)
if [[ ! $digest =~ ^sha256:[0-9a-f]{64}$ ]]; then
    echo "trusted publisher has not published the full-PR-head image" >&2
    exit 1
fi
echo "Using disposable PR #$pr_number head $sha ($digest)" >&2

if ! kubectl get nodeclass "$node_class" -o json | jq -e '.spec.networkPolicy == "DefaultDeny"' >/dev/null; then
    echo "preview NodeClass is not DefaultDeny" >&2
    exit 1
fi
for slot in 0 1; do
    ns="maul-preview-$slot"
    kubectl get networkpolicy preview-isolation -n "$ns" >/dev/null
    for name in "$deployment" "$sibling" "$other"; do
        if kubectl get deployment "$name" -n "$ns" >/dev/null 2>&1; then
            echo "existing $ns/$name: refusing to overwrite another probe" >&2
            exit 1
        fi
    done
done

service_ip() {
    kubectl get service "$2" -n "$1" -o jsonpath='{.spec.clusterIP}'
}
api_ip=$(service_ip default kubernetes)
broker_ip=$(service_ip maul maul-egress-broker)
integration_ip=$(service_ip maul maul-integration-controller)
source_ip=$(service_ip maul maul-source-controller)
status_ip=$(service_ip maul maul-status-server)
for ip in "$api_ip" "$broker_ip" "$integration_ip" "$source_ip" "$status_ip"; do
    if [[ ! $ip =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "could not discover every forbidden service ClusterIP" >&2
        exit 1
    fi
done

# create_probe NAMESPACE NAME [EXTRA_ENV_LINES]: a Deployment-managed netprobe
# Pod, in the exact preview Pod shape, labelled app=NAME. EXTRA_ENV_LINES are
# further env entries, already indented. Every probe object is created, never
# client-side applied: apply adds a last-applied annotation, which the slot
# Service policy refuses (it admits only a health-check path).
create_probe() {
    local ns=$1 name=$2 extra_env=${3:-}
    created=1
    kubectl create -n "$ns" -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $name
  namespace: $ns
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector:
    matchLabels: {app: $name}
  template:
    metadata:
      labels: {app: $name}
    spec:
      serviceAccountName: default
      automountServiceAccountToken: false
      nodeSelector:
        karpenter.sh/nodepool: $node_pool
        eks.amazonaws.com/nodeclass: $node_class
      tolerations:
        - key: $taint_key
          operator: Equal
          value: "true"
          effect: NoExecute
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: probe
          image: $registry/$image_repo:sha-$sha
          imagePullPolicy: Always
          env:
            - {name: PROBE_KUBERNETES_API, value: "$api_ip"}
            - {name: PROBE_EGRESS_BROKER, value: "$broker_ip"}
            - {name: PROBE_INTEGRATION_CONTROLLER, value: "$integration_ip"}
            - {name: PROBE_SOURCE_CONTROLLER, value: "$source_ip"}
            - {name: PROBE_STATUS_SERVER, value: "$status_ip"}
$extra_env
          resources:
            requests: {cpu: 25m, memory: 32Mi}
            limits: {cpu: 250m, memory: 256Mi}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
EOF
}

# run_slot SLOT [EXTRA_ENV_LINES] [TARGETS]: run the probe in slot SLOT and
# require every one of its TARGETS (default 8) to be blocked on all 16
# attempts, and DNS to work. The cold-start runs call it on an empty pool.
run_slot() {
    local slot=$1 extra_env=${2:-} targets=${3:-8} ns="maul-preview-$1" pod logs summary tries=0 blocked
    blocked=$((targets * 16))
    echo "Starting Deployment-managed probe in $ns ($targets forbidden targets)" >&2
    create_probe "$ns" "$deployment" "$extra_env"
    while (( tries < 120 )); do
        pod=$(kubectl get pods -n "$ns" -l "app=$deployment" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        if [[ -n $pod ]]; then
            logs=$(kubectl logs "$pod" -n "$ns" -c probe 2>/dev/null || true)
            if [[ -n $logs ]]; then
                if jq -e 'select(.outcome == "REACHABLE" or .outcome == "SECURITY_FAILURE")' <<<"$logs" >/dev/null 2>&1; then
                    echo "SECURITY FAILURE in $ns/$pod; deleting probe now" >&2
                    cleanup
                    exit 1
                fi
                summary=$(jq -r 'select(.name == "summary") | .outcome' <<<"$logs" 2>/dev/null | tail -1)
                if [[ -n $summary ]]; then
                    if [[ $summary != PASS ]] || ! jq -s -e --argjson blocked "$blocked" \
                        '([.[] | select(.outcome == "blocked")] | length) == $blocked and
                         ([.[] | select(.name == "dns" and .outcome == "ok")] | length) == 1 and
                         ([.[] | select(.outcome == "inconclusive" or .outcome == "REACHABLE")] | length) == 0' \
                        <<<"$logs" >/dev/null; then
                        echo "probe $ns/$pod ended $summary, or did not record all $blocked blocked attempts and DNS" >&2
                        cleanup
                        exit 1
                    fi
                    echo "$ns/$pod: $blocked blocked, DNS ok, no forbidden success ($summary)" >&2
                    kubectl delete deployment "$deployment" -n "$ns" --wait=true >&2
                    return 0
                fi
            fi
        fi
        sleep 10
        tries=$((tries + 1))
    done
    echo "probe in $ns produced no complete result within 20 minutes" >&2
    return 1
}

# pod_ip NAMESPACE NAME: the IP of NAME's running Pod, waiting up to 20 minutes.
pod_ip() {
    local ns=$1 name=$2 ip tries=0
    while (( tries < 120 )); do
        ip=$(kubectl get pods -n "$ns" -l "app=$name" -o jsonpath='{.items[0].status.podIP}' 2>/dev/null || true)
        if [[ $ip =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
            echo "$ip"
            return 0
        fi
        sleep 10
        tries=$((tries + 1))
    done
    echo "no Pod IP for $ns/$name within 20 minutes" >&2
    return 1
}

# run_siblings: multi-component isolation. Two more probe Pods stand in for
# a sibling component (with its own Service) in slot 0 and for a Pod in slot
# 1; the probe in slot 0 must then also find the sibling's Pod and Service
# and the other slot's Pod blocked. They listen on nothing, so a broken
# policy shows as a refused (inconclusive) attempt, which fails the run too.
# The pool is warm by now: this checks the slot policy between Pods, while
# the three runs before it check a cold start.
run_siblings() {
    local sibling_ip service_ip other_ip extra_env
    echo "Starting sibling targets in maul-preview-0 and maul-preview-1" >&2
    create_probe maul-preview-0 "$sibling"
    create_probe maul-preview-1 "$other"
    kubectl create -n maul-preview-0 -f "$probe_dir/sibling-service.yaml"
    sibling_ip=$(pod_ip maul-preview-0 "$sibling")
    other_ip=$(pod_ip maul-preview-1 "$other")
    service_ip=$(service_ip maul-preview-0 "$sibling")
    if [[ ! $service_ip =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "could not discover the sibling Service ClusterIP" >&2
        exit 1
    fi
    extra_env="            - {name: PROBE_SIBLING_POD, value: \"$sibling_ip\"}
            - {name: PROBE_SIBLING_SERVICE, value: \"$service_ip\"}
            - {name: PROBE_OTHER_SLOT_POD, value: \"$other_ip\"}"
    run_slot 0 "$extra_env" 11
    kubectl delete deployment "$sibling" -n maul-preview-0 --wait=true >&2
    kubectl delete service "$sibling" -n maul-preview-0 --wait=true >&2
    kubectl delete deployment "$other" -n maul-preview-1 --wait=true >&2
    created=0
    wait_zero
}

for slot in 0 1 0; do
    wait_zero
    run_slot "$slot"
    created=0
done
wait_zero
run_siblings
echo "Cold-start isolation passed three runs and the sibling run. Agent-log check remains open; see README.md." >&2
echo "Close PR #$pr_number unmerged after recording the result." >&2
