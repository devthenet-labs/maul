## patchy check previews

Check that the preview install is ready, before any Project uses it

### Synopsis

Check the preview install in the release namespace (-n) from your workstation,
before any Project uses it: one line per check, PASS, FAIL, WARN or SKIP, then
the reason. check project judges one Project's previews; this judges what
every Project's previews stand on, with the same checks where they overlap.

preview-config reads the controllers' ConfigMaps: previews and sign-in are
on and agree on the host suffix, the preview gateway alone serves preview
hosts (the legacy load balancer path still serving is a WARN, and serving
without sign-in, previewAuth.stage permit, a FAIL), the sign-in
host sits outside the suffix, and the suffix is a registrable domain of its
own, shared with neither the sign-in host, the relay's issuer nor Dex (a
WARN: preview code would run same-site with sign-in).

preview-dns, preview-tls, preview-auth, preview-auth-dex, preview-auth-host and
preview-gateway-allowlist are check project's, asked of
patchy-probe-0.<host suffix> (the relay's own probe host, which no Preview
owns) and of every Ready Preview, whatever its Project: the host resolves
to the gateway's edge and serves a trusted certificate, the relay and Dex
answer, the probe host is the gateway's 404 page, each Ready Preview
redirects to sign in, and a spoofed X-Forwarded-For cannot get past
previewAuth.gateway.inboundCIDRs.

On a Cloudflare Tunnel edge (the cloudflare-tunnel recipe), whose gateway
publishes no address, preview-dns passes when the record is a CNAME to the
tunnel cloudflared serves (<tunnel id>.cfargotunnel.com). A proxied record
shows only Cloudflare's addresses and hides its target: a SKIP, and
preview-auth-host proves the routing. Anything else is a WARN or a SKIP
saying what to look at. cloudflared's ConfigMap names the tunnel; its
credentials are never read.

preview-viewers reads the bindings of the chart's <release>-preview-viewer
ClusterRole: at least one User or Group carrying the relay's claim prefix
should be bound, or every sign-in is refused 403 unless another role grants
get on projects/previews, which this cannot see (a WARN). preview-nodes
reads the nodes: with node isolation required, at least one carries preview-
controller's node selector and its <taint key>=true:NoExecute taint, and no
node carries the selector without that taint (a FAIL: any Pod could run
beside preview code there). A node with the taint but not the selector is
another install's or a label typo (a WARN); with an EKS Auto Mode NodePool,
no node yet is a SKIP; isolation off is a WARN.

preview-upstreams compares cert-manager, the Tailscale operator and
cloudflared, where a Deployment runs them, with the versions setup previews
pins, by the image's tag or the app.kubernetes.io/version label: older is a
WARN, and none running reports no line.

The check never reads a Secret, and without --isolation it never writes. Its
cluster reads, with your kubeconfig, are ConfigMaps, the preview placeholder
and gateway Ingresses, the gateway's Service, Previews, RoleBindings,
ClusterRoleBindings, Nodes and Deployments; a read you may not make is a
SKIP. The gateway's edge may admit only
previewAuth.gateway.inboundCIDRs, so from any other address the host checks
time out or are refused, and that is a SKIP, not a FAIL.

--isolation adds preview-netpol, the one check that writes to the cluster:
it proves the slot NetworkPolicy lets only the preview gateway reach a slot
Pod, as hack/preview-isolation-probe/ingress.sh --refused-is-blocked does.
A netprobe listener starts in patchy-preview-<--isolation-slot> (on the
preview nodes); a client labelled as the gateway must reach it first; only
then do a client in the agents' namespace and an unlabelled one in the
release namespace run, and both must be blocked (a refused connection counts
as blocked only after that reach); and the listener must still be the same
running Pod afterwards, so a dead listener never passes as a block. All four
Pods are deleted at the end, on failure and on interrupt too. The image must
sit under preview-controller's image prefix as <prefix>netprobe:sha-<40 hex>
(--isolation-image; by default that tag at this CLI's commit): build it from
hack/preview-isolation-probe at the same commit. Anything short of a proven
block is a FAIL; slot policy ipBlocks (the legacy load balancer's subnets)
or a Pod left behind is a WARN.

-o json or -o yaml prints the whole report as data. The exit status is 1 when
any check fails; a WARN never fails the run.

```
patchy check previews [flags]
```

### Examples

```
  patchy check previews -n patchy
  patchy check previews -n patchy -o json | jq '.checks[] | select(.status != "PASS")'
  patchy check previews -n patchy --isolation \
    --isolation-image registry.example/patchy/previews/netprobe:sha-0123456789abcdef0123456789abcdef01234567
```

### Options

```
  -h, --help                         help for previews
      --isolation                    also run preview-netpol: create four short-lived probe Pods (a listener in a slot, three clients) to prove only the preview gateway reaches a slot, then delete them; the one part of the check that writes to the cluster
      --isolation-image string       the netprobe image for --isolation, under the preview image prefix as <prefix>netprobe:sha-<40 hex> (default: that tag at this CLI's commit)
      --isolation-slot int           the slot the --isolation listener runs in
      --isolation-timeout duration   how long each --isolation Pod may take to start or finish (default 5m0s)
```

### Options inherited from parent commands

```
  -A, --all-namespaces             work across every namespace
      --context string             kubeconfig context to use
      --kubeconfig string          path to the kubeconfig file
  -n, --namespace string           namespace to work in (default: the context's)
      --no-color                   disable colour and styling
  -o, --output string              output format: table, wide, json, yaml, name, or markdown (default "table")
      --request-timeout duration   timeout for a single API call (default 30s)
  -v, --verbose                    log what the CLI is doing to stderr
```

### SEE ALSO

* [patchy check](patchy_check.md)	 - Check something the way patchy will judge it, before the pipeline does

