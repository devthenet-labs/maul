## patchy check previews

Check that the preview install is ready, before any Project uses it

### Synopsis

Check the preview install in the release namespace (-n) from your workstation,
before any Project uses it: one line per check, PASS, FAIL, WARN or SKIP, then
the reason. check project judges one Project's previews; this judges what
every Project's previews stand on, with the same checks where they overlap.

preview-config reads the controllers' ConfigMaps: previews and sign-in are
on and agree on the host suffix, the preview gateway alone serves preview
hosts (the legacy load balancer path still serving is a WARN), the sign-in
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

preview-viewers reads the bindings of the chart's <release>-preview-viewer
ClusterRole: at least one User or Group carrying the relay's claim prefix
must be bound, or every sign-in is refused 403. preview-nodes reads the
nodes: with node isolation required, at least one carries preview-
controller's node selector and its <taint key>=true:NoExecute taint, and no
node carries only one of them (with an EKS Auto Mode NodePool, no node yet
is a SKIP; isolation off is a WARN).

The check never reads a Secret. Its cluster reads, with your kubeconfig, are
ConfigMaps, the preview placeholder and gateway Ingresses, the gateway's
Service, Previews, RoleBindings, ClusterRoleBindings and Nodes; a read you
may not make is a SKIP. The gateway's edge may admit only
previewAuth.gateway.inboundCIDRs, so from any other address the host checks
time out or are refused, and that is a SKIP, not a FAIL.

-o json or -o yaml prints the whole report as data. The exit status is 1 when
any check fails; a WARN never fails the run.

```
patchy check previews [flags]
```

### Examples

```
  patchy check previews -n patchy
  patchy check previews -n patchy -o json | jq '.checks[] | select(.status != "PASS")'
```

### Options

```
  -h, --help   help for previews
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

