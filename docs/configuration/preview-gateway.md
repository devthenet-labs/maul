# Preview gateway

The preview gateway puts every preview behind one patchy-owned entry point that runs on any Kubernetes cluster. It is
the [preview-auth](preview-auth.md) relay's Pods, with `--preview-auth-gateway` on. The gateway signs viewers in itself
and proxies each `https://<label>.<host suffix>` to that Preview's slot Services. The platform's edge only has to route
one wildcard and one sign-in host to a single Service. The edge can be an ALB, ingress-nginx, Traefik, a Gateway API
implementation, a LoadBalancer Service or a tunnel. Nothing about previews depends on EKS Auto Mode any more.

```text
viewer --https--> [your edge: one route for *.<hostSuffix> and the sign-in host]
                      |  TLS ends at the edge (or at the gateway, from a mounted Secret)
                      v
            Service <release>-preview-gateway (port 80; 443 with the gateway's own TLS)
                      |
            preview-auth Pods (the gateway)
               |- sign-in host: Dex, the viewer's relay session, the access review, a handoff code
               '- preview hosts: a host session per host; routed by status.serving, longest path first
                      |  NetworkPolicy: only Pods labelled patchy.bitwisemedia.uk/preview-gateway
                      v
            patchy-preview-<n>: ClusterIP Services and Deployments only
```

This page covers turning the gateway on, exposing it on each platform, moving an EKS install off the preview ALB,
checking the result, and what changes for security. The protocol, the proxy's rules, the flags and the metrics are in
[preview-auth](preview-auth.md#the-preview-gateway-off-by-default). preview-controller's side is described in
[preview-controller](preview-controller.md#without-the-legacy-ingress).

!!! note "Release N of two"

    This release ships the gateway **beside** the legacy path (each preview's own Ingress on the EKS preview ALB,
    signed in by the ALB through the relay). Both are on or unchanged by default: the gateway is off
    (`previewAuth.gateway.enabled: false`), and preview-controller keeps rendering slot Ingresses
    (`preview.legacyIngress: true`). An install that sets neither renders exactly as before. The next release removes
    the legacy path and its values.

## Turning it on

The gateway needs [preview sign-in](../intents/preview-sign-in.md) (`previewAuth.enabled`, Dex and the viewers), the
preview foundation and preview-controller. On top of those:

```yaml
# patchy-previews.yaml
preview:
  enabled: true
  hostSuffix: preview.acme-apps.dev
  imageRegistry: registry.acme.dev # any registry host; preview images are <registry>/<imagePathPrefix>/<leaf>
  legacyIngress: false # only on a cluster with no legacy preview ALB; see "Moving off the preview ALB" for EKS
  placeholder:
    enabled: false # the legacy preview ALB's placeholder
  nodeIsolation:
    selector: { patchy.acme.dev/previews: "true" } # the dedicated preview nodes' labels
    taintKey: patchy.acme.dev/preview-only # their NoExecute taint, <key>=true
previewController:
  enabled: true
previewAuth:
  enabled: true
  host: preview-auth.patchy.acme.dev # the sign-in host by default
  gateway:
    enabled: true
    forwardedHops: 1 # proxies in front that append X-Forwarded-For: required with any exposure
    inboundCIDRs: [203.0.113.10/32] # optional; at the edge where it can, and again at the gateway
    ingress:
      enabled: true
      className: nginx
      # ... one exposure, from the recipes below
```

With it on:

- The relay's Pods carry `patchy.bitwisemedia.uk/preview-gateway: "true"`. The slot NetworkPolicy admits Pods in the
  release namespace with that label on port `http` only. The relay's own policy gains egress to that port. While
  `preview.legacyIngress` is `true` the policy also admits the preview ALB's subnets (`preview.albSubnetCIDRs`), every
  address in them, as before. Once it is `false`, nothing else reaches a slot Pod: no Ingress, no load balancer, no
  subnet, no other namespace, even with the ALB values still set.
- The Service `<release>-preview-gateway` (port 80, and 443 with the gateway's own TLS) is what every exposure points
  at.
- The relay keeps every endpoint it had, so the legacy ALB path keeps working while both run.
- The relay's host probe expects the path in force: either path's redirect while `preview.legacyIngress` is `true`, and
  only the gateway's redirect, plus its `404` for a host no Preview owns, once it is `false`
  (`--preview-auth-probe-mode`, which the chart sets).

`preview.nodeIsolation.mode: none` is an explicit opt-out for a cluster with no dedicated preview nodes. Preview code
then shares nodes with everything else. Keep the default `required` wherever you can.

Rules that hold on every platform:

- **The host suffix gets a registrable domain of its own.** Put `*.<hostSuffix>` on a different registrable domain from
  the sign-in host and the status page (`preview.acme-apps.dev` beside `patchy.acme.dev`). Every preview host is
  same-site with every other, and only the gateway's `__Host-` cookies and its origin rules keep them apart.
- **The sign-in host sits outside the host suffix.** The chart refuses one under it.
- **`forwardedHops` is the number of proxies that append to `X-Forwarded-For` in front of the gateway.** The rate limit
  and the inbound allowlist judge the address it names, so a value too high lets a client choose its own.
  `patchy check project` sends a spoofed `X-Forwarded-For` to catch that (below).
- **No edge caching on preview hosts.** The gateway marks every proxied answer `private` and `Vary: Cookie`, but a CDN
  that caches by rule would still serve one viewer's page to another.
- **Edge health check:** `/.patchy/healthz` answers `200` on any Host and is never forwarded to a preview.
- **The CNI must enforce NetworkPolicy.** Slot isolation is NetworkPolicy, and a CNI that ignores it leaves every slot
  open to the cluster network. Prove it with the [isolation probe](#the-isolation-probe).

## Platform recipes

Each recipe routes `*.<hostSuffix>` and the sign-in host to the gateway. When the sign-in host is `previewAuth.host`,
the relay's own Ingress already routes it, and the exposure carries the wildcard only. The same holds when you set
`previewAuth.gateway.signInHost` to a host of its own: register `https://<sign-in host>/dex/callback` at Dex for the
relay's client first.

### EKS: an ALB Ingress

Use the edge ALB the webhook and status page already share. Its certificates must cover `*.<hostSuffix>`: add the
preview wildcard certificate's ARN to `edgeIngressClass.certificateARNs` (the ALB picks a certificate by SNI). The
[terraform module](../intents/terraform-module.md) issues that certificate today for the preview ALB; moving it onto the
edge in the module is a follow-up.

```yaml
previewAuth:
  gateway:
    enabled: true
    forwardedHops: 1
    inboundCIDRs: [203.0.113.10/32]
    ingress:
      enabled: true # className empty: the edge class
```

The chart adds `target-type: ip` and the health-check path. On the shared edge ALB the allowlist is the gateway's alone:
an ALB's `inbound-cidrs` applies to the whole Ingress group, so it cannot restrict preview hosts without also
restricting the webhook. To hold it at the edge, give the gateway an ALB of its own: set `className` to another
IngressClass and `ingress.inboundCIDRsAnnotation: alb.ingress.kubernetes.io/inbound-cidrs`. The chart then also sets
`scheme: internet-facing`. The ALB keeps the client's address in `X-Forwarded-For`, so `forwardedHops` is `1`.

### ingress-nginx

```yaml
previewAuth:
  gateway:
    enabled: true
    forwardedHops: 1
    inboundCIDRs: [203.0.113.10/32]
    ingress:
      enabled: true
      className: nginx
      inboundCIDRsAnnotation: nginx.ingress.kubernetes.io/whitelist-source-range
      annotations:
        cert-manager.io/cluster-issuer: letsencrypt-dns # a DNS-01 issuer: a wildcard needs it
        nginx.ingress.kubernetes.io/proxy-body-size: 32m # the gateway's own body limit
        nginx.ingress.kubernetes.io/proxy-read-timeout: "3600" # streams and WebSockets
        nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
        nginx.ingress.kubernetes.io/proxy-buffering: "off"
      tls:
        - hosts: ["*.preview.acme-apps.dev"]
          secretName: preview-wildcard-tls
```

`forwardedHops: 1` holds when the controller sees the client's address: its Service has `externalTrafficPolicy: Local`,
or it trusts the PROXY protocol from its load balancer. Behind a cloud L7 load balancer that also appends, it is `2`.
ingress-nginx passes WebSockets through without further annotations.

### Traefik

```yaml
previewAuth:
  gateway:
    enabled: true
    forwardedHops: 1
    inboundCIDRs: [203.0.113.10/32]
    ingress:
      enabled: true
      className: traefik
      annotations:
        cert-manager.io/cluster-issuer: letsencrypt-dns
        traefik.ingress.kubernetes.io/router.entrypoints: websecure
        # traefik.ingress.kubernetes.io/router.middlewares: patchy-preview-allowlist@kubernetescrd
      tls:
        - hosts: ["*.preview.acme-apps.dev"]
          secretName: preview-wildcard-tls
```

Traefik reads no allowlist from an annotation value, so leave `inboundCIDRsAnnotation` empty. To hold the allowlist at
the edge too, create an `IPAllowList` Middleware (`ipWhiteList` before Traefik v3) in the release namespace and name it
in `router.middlewares`. The gateway checks `inboundCIDRs` either way. Like ingress-nginx, Traefik sees the client's
address only with `externalTrafficPolicy: Local` or the PROXY protocol from its load balancer.

### Gateway API

```yaml
previewAuth:
  gateway:
    enabled: true
    forwardedHops: 1
    ingress:
      enabled: false
    httpRoute:
      parentRefs:
        - name: edge
          namespace: gateway-system
          sectionName: https-previews
```

The chart renders one HTTPRoute for `*.<hostSuffix>` (and a sign-in host of its own). TLS is the listener's: give the
Gateway an HTTPS listener whose hostname is `*.<hostSuffix>`, with a wildcard certificate, and allow routes from the
release namespace. Check `forwardedHops` against your implementation: Envoy-based ones (Envoy Gateway, Istio, Contour)
append one entry. An allowlist at the edge is the implementation's own policy resource, where it has one. An HTTPRoute
publishes no address of its own, so set `publishedAddresses` to the Gateway's for `patchy check project`.

### Cloudflare Tunnel

No exposure in the chart. `cloudflared` runs in the cluster and connects out to Cloudflare. Leave `ingress`, `httpRoute`
and `service.type` at their defaults, and route both hosts to the gateway's Service in cloudflared's configuration:

```yaml
# cloudflared config.yaml
ingress:
  - hostname: "*.preview.acme-apps.dev"
    service: http://patchy-preview-gateway.patchy.svc.cluster.local:80
  - hostname: preview-auth.patchy.acme.dev # only when it is not already routed to the relay
    service: http://patchy-preview-gateway.patchy.svc.cluster.local:80
  - service: http_status:404
```

```yaml
previewAuth:
  gateway:
    enabled: true
    forwardedHops: 1 # Cloudflare's edge appends the client's address; cloudflared adds none
```

- A proxied wildcard DNS record, `*.preview.acme-apps.dev`, is a CNAME to `<tunnel id>.cfargotunnel.com`.
- **Bypass the cache** for the preview hostnames: a Cache Rule matching `http.host` under `.preview.acme-apps.dev` with
  "Bypass cache". Cloudflare honours `private`, but a rule that caches by extension does not.
- The relay's NetworkPolicy admits `previewAuth.ingressFrom` (empty is anywhere) on the gateway's port 8082. If you set
  it, include cloudflared's Pod addresses, and let cloudflared's own egress reach that port.
- Hold the allowlist at Cloudflare (a WAF custom rule or Cloudflare Access) and in `inboundCIDRs`.
- A proxied record resolves to Cloudflare's addresses, not the tunnel's. `patchy check project`'s DNS line then SKIPs
  with no `publishedAddresses`; check the record in the Cloudflare dashboard instead.

### k3d with wildcard DNS and cert-manager

A desktop cluster with no public load balancer: k3d's serverlb publishes the bundled Traefik on an address you choose (a
tailnet or LAN address), a public wildcard record points at it, and cert-manager issues the wildcard certificate by
DNS-01. Nothing here is desktop-only patchy configuration: it is the Traefik recipe plus an address.

```sh
# The cluster, its load balancer bound to the address viewers reach (a tailnet address here). Firewall the host, or
# use `tailscale serve`, so nothing else reaches it.
k3d cluster create patchy -p "100.101.102.103:443:443@loadbalancer" -p "100.101.102.103:80:80@loadbalancer"
# The dedicated preview node: added any time, without a rebuild.
k3d node create preview --cluster patchy --role agent
kubectl label node k3d-preview-0 patchy.acme.dev/previews=true
kubectl taint node k3d-preview-0 patchy.acme.dev/preview-only=true:NoExecute
```

- DNS: an A record `*.preview.desktop.acme.dev` (and the sign-in host) to `100.101.102.103`.
- cert-manager: a ClusterIssuer that answers DNS-01 in your zone (Route 53, Cloudflare, ...) and a `Certificate` for
  `*.preview.desktop.acme.dev` in the release namespace. Name its Secret in `previewAuth.gateway.ingress.tls`.
- Values: the Traefik recipe, with `preview.hostSuffix: preview.desktop.acme.dev`, `preview.legacyIngress: false` and
  `publishedAddresses: [100.101.102.103]`.
- k3d's serverlb is an L4 proxy, so Traefik sees the serverlb's address rather than the viewer's. Leave `inboundCIDRs`
  empty and restrict who reaches the address instead (tailnet ACLs, the host firewall).
- k3s's bundled network policy controller enforces NetworkPolicy. On the desktop, node isolation is scheduling only (one
  host): it keeps other workloads off the preview node, not off the machine.

A LoadBalancer exposure (`service.type: LoadBalancer`, with `tls.secrets` so the gateway terminates TLS itself) suits
MetalLB or a cloud L4 load balancer. Keep `externalTrafficPolicy: Local` and `forwardedHops: 0` there.

## Moving off the preview ALB (EKS, release N)

An EKS install that serves previews through the preview ALB moves in three steps. Both paths sign viewers in, so the
wildcard record can point at either one, and DNS is the rollback until the last step.

1. **Deploy the gateway beside the ALB.** Enable `previewAuth.gateway` with the [ALB Ingress](#eks-an-alb-ingress)
   exposure and keep `preview.legacyIngress: true`. Keep the sign-in host the relay's (`previewAuth.host`), so Dex needs
   no change. The wildcard record still points at the preview ALB, so viewers see nothing new. Check:

   ```sh
   kubectl -n patchy get ingress patchy-preview-gateway   # an address on the edge ALB
   patchy check project <project> -n patchy               # preview-dns PASSes on the legacy load balancer
   bash hack/preview-isolation-probe/ingress.sh ...       # the gateway reaches a slot; nothing else does
   ```

   To try the gateway before moving the record, send a request with the preview host to the edge ALB:
   `curl -sk --resolve <project>-0.<suffix>:443:<edge ALB address> https://<project>-0.<suffix>/` must return the
   gateway's `404` page ("No live preview").

2. **Flip the wildcard record** to the edge ALB. With the terraform module, the preview alias moves from the preview ALB
   to the edge (a terraform PR). Wait out the record's TTL, then run `patchy check project` again. `preview-dns` names
   the gateway's edge, `preview-auth-host` sees the gateway's redirect to `/start` on every Ready preview and its `404`
   on `<project>-0.<suffix>`, from an address inside `inboundCIDRs`. Run it again from outside them:
   `preview-gateway-allowlist` passes, and `preview-auth-host` skips every host the gateway refuses there. Sign in to a
   live preview in a browser.

   **Rollback:** point the record back at the preview ALB. Nothing else changed.

3. **Retire the slot Ingresses.** Set `preview.legacyIngress: false` and upgrade. preview-controller deletes the slot
   Ingresses it rendered, the relay's probe switches to gateway mode, `patchy check project` judges the gateway alone,
   and the slot NetworkPolicy stops admitting the ALB's subnets, so only the gateway reaches a slot. Keep
   `preview.placeholder` and the ALB values for now: they keep the idle preview ALB, and so a way back, until the next
   release removes them. **Rollback:** set `preview.legacyIngress: true`, wait until every Ready preview has its slot
   Ingress again (`kubectl get ingress -A -l app.kubernetes.io/managed-by=patchy-preview-controller`), then point the
   record back.

The next release removes the legacy path: the ALB values, the relay's provider endpoints and the placeholder. Its
release notes list the kept objects to delete by hand.

## Checking it

`patchy check project` reads the mode in force from the relay's and preview-controller's ConfigMaps and, in gateway
mode, checks:

| Check                       | What passes                                                                                                                                                                                                                                                                            |
| --------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `preview-dns`               | `<project>-0.<suffix>` resolves to the gateway's edge: `publishedAddresses`, else the gateway Ingress's or LoadBalancer Service's address. While both paths serve, the legacy preview load balancer passes too. A private edge not in this resolver's answer is a SKIP.                |
| `preview-tls`               | A certificate trusted for the probe host                                                                                                                                                                                                                                               |
| `preview-auth-dex`          | Dex accepts the relay's client with `<issuer>/dex/callback`, as before                                                                                                                                                                                                                 |
| `preview-auth-host`         | `<project>-0.<suffix>`, which no Preview owns, answers with the gateway's own `404` page. Every Ready preview answers an unauthenticated navigation with the `302` to `https://<sign-in host>/start` for its own label. A host the gateway refuses (outside `inboundCIDRs`) is a SKIP. |
| `preview-gateway-allowlist` | From an address the gateway refuses, a request whose `X-Forwarded-For` names an address inside `inboundCIDRs` is still refused. From an admitted address it is a SKIP: run it from outside.                                                                                            |

The relay's background probe applies the same verdicts every minute (`patchy.preview_auth.unprotected_hosts`). In
gateway mode it also asks for `patchy-probe-0.<suffix>`, which must be the gateway's `404`.

## The isolation probe

`hack/preview-isolation-probe/ingress.sh` proves the slot NetworkPolicy from inside the cluster with kubectl alone. A
netprobe Pod listens in a slot. Three client Pods each try to connect to it: one in the agents namespace, one in the
release namespace without the gateway label, and one with it. The first two must be refused and the third admitted. The
image (under the preview image prefix, `sha-` tagged), the node selector and the taint key are flags, so it runs on k3d
as on EKS. Run it after turning the gateway on and after any CNI or cluster upgrade. See its
[README](https://github.com/devthenet-labs/patchy/blob/main/hack/preview-isolation-probe/README.md#the-ingress-direction-only-the-preview-gateway-reaches-a-slot).
On EKS, the egress run (`run.sh`) still covers the cold-start egress direction.

## Security

Stronger than the ALB path:

- No slot renders an Ingress once `preview.legacyIngress` is `false`. "A preview host without sign-in" is then not a
  state any controller, rollback or foreign object can reach.
- Once `preview.legacyIngress` is `false`, a slot admits only gateway Pods, not a subnet. Until then it also admits the
  preview ALB's subnets, and with them anything else running there, without sign-in.
- Revocation takes effect within about 40 seconds: the access-review cache plus a re-check that also closes open
  streams.
- No OIDC client Secret lives in a slot namespace.

What to keep in mind:

- The gateway is on the data path of hostile code. Its proxy rules (cookie and header stripping, `Set-Cookie` filtering,
  the reserved `/.patchy/` prefix, the Host grammar and the origin rules) are security-critical and pinned by seeded
  properties ([preview-auth](preview-auth.md#the-preview-gateway-off-by-default)).
- One compromised gateway replica sees every preview's traffic and its viewers' pairwise ids. The ALB did before. Its
  Pod is hardened like the relay's and reads no Secret through the API.
- A change to a viewer's IdP groups applies when their host session ends (`--preview-auth-gateway-session-timeout`, 15
  minutes by default). A removed RoleBinding applies within the review cache.
- Anyone on the cluster network who can bypass NetworkPolicy can reach a preview. A CNI that does not enforce
  NetworkPolicy is not supported.
