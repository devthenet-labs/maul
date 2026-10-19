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

!!! tip "Setting up a new install"

    Start with [Set up previews](../intents/set-up-previews.md): `patchy setup previews` writes the values, manifests
    and Secret manifests for one of four recipes, and the ordered steps for the rest. This page is the reference for
    what it writes and for building an exposure by hand.

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
  # no stage: a gateway-only install takes one helm upgrade (below)
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

**A gateway-only install sets no `previewAuth.stage`.** The permit and require stages, and the two `helm upgrade`s
between them, exist for the preview ALB: Helm applies a slot Ingress before the admission policies that judge its
sign-in annotations. With `preview.legacyIngress: false` from the start no slot Ingress is ever rendered, so leave
`stage` at its default and turn everything above on in one upgrade. What `require` adds there (the ALB's annotation sets
in preview-controller's configuration, and a kept admission policy for slot Ingresses) has nothing to act on, so the
chart treats the stage as inert while no preview ALB renders (`preview.albName` unset too): `require` there also takes
one upgrade, with no permit stage first, and the install NOTES stage nothing. An install that already reached `require`
on the ALB path keeps it while it moves: stepping back to `permit` is the
[rollback runbook](../intents/preview-sign-in.md#rolling-back), not a cleanup.

`preview.nodeIsolation.mode: none` is an explicit opt-out for a cluster with no dedicated preview nodes. Preview code
then shares nodes with everything else. Keep the default `required` wherever you can.

Rules that hold on every platform:

- **The host suffix gets a registrable domain of its own, unless you accept sharing one.** Put `*.<hostSuffix>` on a
  different registrable domain from the sign-in host and the status page (`preview.acme-apps.dev` beside
  `patchy.acme.dev`). Every preview host is same-site with every other, and only the gateway's `__Host-` cookies and its
  origin rules keep them apart. The chart does not enforce this; `patchy check previews` WARNs (its `preview-config`
  line) when the suffix shares a registrable domain with the sign-in host, the relay's issuer or Dex. With only one
  domain, accept the sharing explicitly: see
  [Previews on the sign-in host's domain](#previews-on-the-sign-in-hosts-domain) for what that leaves exposed. The
  [Public Suffix List](https://publicsuffix.org/) decides what is registrable: `acme-apps.dev` is, and so is
  `acme.github.io`, because `github.io` is a public suffix.
- **The sign-in host sits outside the host suffix.** The chart refuses one under it.
- **`forwardedHops` is the number of proxies that append to `X-Forwarded-For` in front of the gateway.** The rate limit
  and the inbound allowlist judge the address it names, so a value too high lets a client choose its own.
  `patchy check project` sends a spoofed `X-Forwarded-For` to catch that (below).
- **No edge caching on preview hosts.** The gateway marks every proxied answer `private` and `Vary: Cookie`, but a CDN
  that caches by rule would still serve one viewer's page to another.
- **Edge health check:** `/.patchy/healthz` answers `200` on any Host and is never forwarded to a preview.
- **The CNI must enforce NetworkPolicy.** Slot isolation is NetworkPolicy, and a CNI that ignores it leaves every slot
  open to the cluster network. Prove it with the [isolation probe](#the-isolation-probe).

### Previews on the sign-in host's domain

One registrable domain for the previews, the sign-in host and Dex (`*.previews.acme.dev`, `preview-auth.acme.dev`,
`dex.acme.dev`) works, but it has a cost, so it is an explicit choice.
`patchy setup previews --shared-domain acknowledged` makes it (on a terminal the command explains it and asks you to
type the word), and the values it writes then carry:

```yaml
previewAuth:
  sharedSiteAcknowledged: acme.dev # the registrable domain previews share with sign-in
```

The chart records the value as an annotation on the relay's ConfigMap and acts on nothing else. It must be
`preview.hostSuffix` or a parent of it. `patchy check previews` then reports the sharing as INFO instead of WARN, but
only while the value names the suffix's registrable domain: move the previews to another domain and the WARN returns.

**What changes.** Preview code (any page a preview serves, which is pull-request code) is then same-site, though never
same-origin, with the sign-in host, with Dex and with every other host on `acme.dev`. A site boundary would otherwise
stop two things:

- _Cookie tossing._ A preview can set a cookie for the whole of `acme.dev` (`Domain=acme.dev`, from script or a response
  header), which the browser then sends to every host on it. It can also fill that domain's cookie jar.
- _Same-site requests._ A preview's background requests (`fetch`, an image, an iframe, a form post) to any host on
  `acme.dev` are same-site, so they carry that host's `SameSite=Lax` and `SameSite=Strict` cookies, and `SameSite` stops
  nothing.

**What patchy holds.**

- Every cookie the relay and the gateway set is `__Host-` (`__Host-patchy-pa`, `__Host-patchy-pa-login-*`,
  `__Host-patchy-pv`, `__Host-patchy-pv-state-*`): host-only, `Secure`, `Path=/`. No other host, a preview included, can
  set, overwrite or shadow one in a browser that enforces the prefix, which every current browser does. The status
  page's cookies are `__Host-` too.
- The sign-in endpoints that read the relay session or set a cookie (`/authorize`, `/start`, `/dex/callback`) refuse a
  request from another site that is not a top-level navigation: `Sec-Fetch-Site` `same-site` or `cross-site` with
  anything but `Sec-Fetch-Mode: navigate` and `Sec-Fetch-Dest: document` gets a `403` page, with no code and no cookie.
  `/logout` takes only `Sec-Fetch-Site: same-origin`. A navigation a preview starts (a link, `window.open`) still signs
  the viewer in, as one from another site would. The handoff code goes only to the preview host the sealed state names,
  and is spent only with that host's own state cookie, so no preview can take another's code or fix a viewer's session.
- The status page refuses a same-site write (`Sec-Fetch-Site` `same-site` or `cross-site`, or, from a browser that sends
  no `Sec-Fetch-Site`, an `Origin` other than its own), and same-site requests on its API and streams too while its
  intents or usage views are on, when it also refuses a write with no `Sec-Fetch-Site` at all. Its session cookie is
  `SameSite=Lax`, so these checks, not `SameSite`, are what keep a preview on its registrable domain from approving,
  retrying or resetting with a viewer's session.

**What patchy cannot hold.**

- _Every other app on `acme.dev`_ (a wiki, Grafana, Argo CD, the status page's own neighbours). None may rely on a
  cookie set for the parent domain, nor on `SameSite` alone for CSRF protection: each needs host-only (ideally
  `__Host-`) session cookies and its own CSRF defence (tokens, or `Origin` and `Sec-Fetch-Site` checks). Audit them
  before you accept the sharing.
- _Dex_, when it is on `acme.dev`: its cookies and forms are Dex's, held to whatever Dex holds them to.
- _Cookie-jar flooding._ A preview that sets many cookies for `acme.dev` can push the relay's session out of the
  browser's jar, or make requests to the sign-in host too large to serve. Viewers then sign in again, or cannot until
  the cookies expire. This denies service; it grants no access.
- _A browser without `Sec-Fetch-*` headers_ (Safari before 16.4): the sign-in endpoints cannot tell its background
  requests from navigations. Its `SameSite=Lax` relay session cookie then reaches them from a preview, which can make a
  sign-in happen in the background but gains nothing by it: the code still goes only to the preview host the sealed
  state names, and is spent only with that host's own `HttpOnly` state cookie. The status page, when it is on
  `acme.dev`, falls back to the `Origin` header such a browser still sends on a `POST`; with its intents and usage views
  off, a browser that sent neither header could still make a same-site write there with the viewer's session.

What stays refused however acknowledged: a sign-in host or a Dex issuer under the host suffix (same-origin with a
preview, which the chart refuses too). On Cloudflare, a shared domain needs Advanced Certificate Manager, since the
sign-in host must sit outside the suffix and every preview host is then a second-level name
([Certificate depth](#cloudflare-tunnel)).

## Platform recipes

`patchy setup previews` writes four of these, one per `--edge`: `cloudflare-tunnel` is
[Cloudflare Tunnel](#cloudflare-tunnel), `gateway-tls` is
[the gateway's own TLS](#the-gateways-own-tls-on-a-load-balancer), `tailscale` is
[a tailnet device](#a-tailnet-device-through-the-tailscale-operator), and `existing` is the [ALB](#eks-an-alb-ingress),
[ingress-nginx](#ingress-nginx), [Traefik](#traefik) or [Gateway API](#gateway-api) recipe for the class or parent you
name. Every recipe that ends TLS in the cluster uses [the wildcard certificate](#the-wildcard-certificate).

Each recipe routes `*.<hostSuffix>` and the sign-in host. When the sign-in host is `previewAuth.host` (the default), the
chart's gateway exposure carries the wildcard only, and the sign-in host reaches the relay some other way:

- **Through the relay's own Ingress** (EKS, ingress-nginx, Traefik). It takes the edge class (`edgeIngressClass`, an ALB
  on EKS) unless `previewAuth.ingress` names another, so every Ingress recipe off the edge ALB sets
  `previewAuth.ingress.className` and its TLS as well as the gateway's.
- **Through the gateway's Service** (Gateway API, Cloudflare Tunnel, a LoadBalancer). The gateway's listener answers the
  relay's host as the relay does, so the recipe sends the sign-in host there too: an HTTPRoute of your own, a
  cloudflared rule, or the same DNS address. The relay's Ingress then renders with no class and nothing serves it.

A sign-in host of its own (`previewAuth.gateway.signInHost`) is carried by the gateway's Ingress or HTTPRoute beside the
wildcard. Register `https://<sign-in host>/dex/callback` at Dex for the relay's client first.

### The wildcard certificate

Every recipe where TLS ends in the cluster (ingress-nginx, Traefik, k3d, and the gateway's own TLS) uses one explicit
cert-manager `Certificate` in the release namespace. It covers `*.<hostSuffix>` and the sign-in host, and its Secret is
named in values. Do not also put a `cert-manager.io/cluster-issuer` or `cert-manager.io/issuer` annotation on either
Ingress: cert-manager would then create a second Certificate for the same Secret from the Ingress (ingress-shim), and
the two would fight over it. On the edge ALB, Gateway API with a listener certificate, and Cloudflare, the edge holds
the certificate and none of this applies, but it must hold one for the sign-in host as well as `*.<hostSuffix>`: the
wildcard never covers the sign-in host, which sits outside the suffix. On Gateway API that is a listener of its own
(below).

```yaml
# preview-certificate.yaml, applied in the release namespace once cert-manager runs
apiVersion: cert-manager.io/v1
kind: Issuer # namespaced: the DNS credential lives beside the one Certificate that uses it
metadata:
  name: preview-dns01
  namespace: patchy
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: ops@acme.dev
    privateKeySecretRef:
      name: preview-dns01-account
    solvers:
      - dns01: # a wildcard needs DNS-01; any solver cert-manager supports, here Cloudflare
          cloudflare:
            apiTokenSecretRef:
              name: preview-dns01-token # DNS edit and zone read on the two names' zones only
              key: api-token
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: preview-wildcard
  namespace: patchy
spec:
  secretName: preview-wildcard-tls
  issuerRef:
    kind: Issuer
    name: preview-dns01
  dnsNames:
    - "*.preview.acme-apps.dev"
    - preview-auth.patchy.acme.dev # the sign-in host
  privateKey:
    algorithm: ECDSA
    size: 256
    rotationPolicy: Always
```

The two names sit on different registrable domains (the first rule above), so usually in two DNS zones: the solver's
credential must be able to answer for both, or give each zone a solver of its own (`selector.dnsZones`). A
`ClusterIssuer` works too, but it lets any namespace's Certificate use the DNS credential.

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

With [the wildcard certificate](#the-wildcard-certificate) issued:

```yaml
previewAuth:
  ingress: # the relay's own Ingress, for the sign-in host
    className: nginx
    tls:
      - hosts: [preview-auth.patchy.acme.dev]
        secretName: preview-wildcard-tls
  gateway:
    enabled: true
    forwardedHops: 1
    inboundCIDRs: [203.0.113.10/32]
    ingress:
      enabled: true
      className: nginx
      inboundCIDRsAnnotation: nginx.ingress.kubernetes.io/whitelist-source-range
      annotations:
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
ingress-nginx passes WebSockets through without further annotations. The relay's Ingress also carries the chart's ALB
annotations; ingress-nginx ignores them.

### Traefik

With [the wildcard certificate](#the-wildcard-certificate) issued:

```yaml
previewAuth:
  ingress: # the relay's own Ingress, for the sign-in host
    className: traefik
    annotations:
      traefik.ingress.kubernetes.io/router.entrypoints: websecure
    tls:
      - hosts: [preview-auth.patchy.acme.dev]
        secretName: preview-wildcard-tls
  gateway:
    enabled: true
    forwardedHops: 1
    inboundCIDRs: [203.0.113.10/32]
    ingress:
      enabled: true
      className: traefik
      annotations:
        traefik.ingress.kubernetes.io/router.entrypoints: websecure
        # traefik.ingress.kubernetes.io/router.middlewares: patchy-preview-allowlist@kubernetescrd
      tls:
        - hosts: ["*.preview.acme-apps.dev"]
          secretName: preview-wildcard-tls
```

Traefik reads no allowlist from an annotation value, so leave `inboundCIDRsAnnotation` empty. To hold the allowlist at
the edge too, create an `IPAllowList` Middleware (`ipWhiteList` before Traefik v3) in the release namespace and name it
in `router.middlewares`. The gateway checks `inboundCIDRs` either way. Like ingress-nginx, Traefik sees the client's
address only with `externalTrafficPolicy: Local` or the PROXY protocol from its load balancer. For streams and
WebSockets, lift the `websecure` entrypoint's read and write timeouts (`ports.websecure.transport.respondingTimeouts` in
Traefik's chart, `0` for none), as the proxy timeouts do for ingress-nginx above.

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

The chart renders one HTTPRoute for `*.<hostSuffix>` (and for a sign-in host of its own, which then needs a listener
under the same `parentRefs`). The default sign-in host, `previewAuth.host`, is not on it, and on a cluster with no
ingress controller the relay's own Ingress routes nothing, so route that host to the gateway's Service with an HTTPRoute
of your own. TLS is the listeners': give the Gateway one HTTPS listener for `*.<hostSuffix>` and one for the sign-in
host, each with a certificate (the wildcard never covers the sign-in host), and allow routes from the release namespace.

```yaml
# The Gateway's listeners (in the Gateway the platform team owns) and the sign-in host's route
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: edge
  namespace: gateway-system
spec:
  gatewayClassName: eg # your implementation's class
  listeners:
    - name: https-previews
      hostname: "*.preview.acme-apps.dev"
      port: 443
      protocol: HTTPS
      tls:
        certificateRefs:
          - name: preview-wildcard-tls
      allowedRoutes:
        namespaces:
          from: Selector
          selector:
            matchLabels:
              kubernetes.io/metadata.name: patchy
    - name: https-sign-in
      hostname: preview-auth.patchy.acme.dev
      port: 443
      protocol: HTTPS
      tls:
        certificateRefs:
          - name: preview-sign-in-tls
      allowedRoutes:
        namespaces:
          from: Selector
          selector:
            matchLabels:
              kubernetes.io/metadata.name: patchy
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: preview-sign-in
  namespace: patchy
spec:
  parentRefs:
    - name: edge
      namespace: gateway-system
      sectionName: https-sign-in
  hostnames:
    - preview-auth.patchy.acme.dev
  rules:
    - backendRefs:
        - name: patchy-preview-gateway # the gateway answers the relay's host as the relay does
          port: 80
```

A listener's certificate Secret lives in the Gateway's namespace, or in another one through a `ReferenceGrant`. Check
`forwardedHops` against your implementation: Envoy-based ones (Envoy Gateway, Istio, Contour) append one entry. An
allowlist at the edge is the implementation's own policy resource, where it has one. An HTTPRoute publishes no address
of its own, so set `publishedAddresses` to the Gateway's for `patchy check project`.

### Cloudflare Tunnel

No exposure in the chart. `cloudflared` runs in the cluster and connects out to Cloudflare. Leave `ingress`, `httpRoute`
and `service.type` at their defaults, and route both hosts to the gateway's Service in cloudflared's configuration. The
sign-in host goes there too: the gateway answers the relay's host as the relay does, and nothing else routes it, because
the relay's own Ingress has no class off the edge ALB. Cloudflare holds both certificates.

```yaml
# cloudflared config.yaml
ingress:
  - hostname: "*.preview.acme-apps.dev"
    service: http://patchy-preview-gateway.patchy.svc.cluster.local:80
  - hostname: preview-auth.patchy.acme.dev # the sign-in host
    service: http://patchy-preview-gateway.patchy.svc.cluster.local:80
  - service: http_status:404
```

```yaml
previewAuth:
  gateway:
    enabled: true
    forwardedHops: 1 # Cloudflare's edge appends the client's address; cloudflared adds none
```

- Two proxied DNS records, `*.preview.acme-apps.dev` and the sign-in host, are each a CNAME to
  `<tunnel id>.cfargotunnel.com`.
- **Bypass the cache** for the preview hostnames: a Cache Rule matching `http.host` under `.preview.acme-apps.dev` with
  "Bypass cache". Cloudflare honours `private`, but a rule that caches by extension does not.
- The relay's NetworkPolicy admits `previewAuth.ingressFrom` (empty is anywhere) on the gateway's port 8082. If you set
  it, include cloudflared's Pod addresses, and let cloudflared's own egress reach that port. The setup command's values
  set it to `[]`, overriding a narrowing in your own values.
- Hold the allowlist at Cloudflare (a WAF custom rule or Cloudflare Access) and in `inboundCIDRs`. The setup command
  refuses `--inbound-cidr` on this edge and leaves `inboundCIDRs` empty: keep the allowlist at Cloudflare.
- A proxied record resolves to Cloudflare's addresses, not the tunnel's. `patchy check project` and `check previews`
  recognise the tunnel from the setup command's cloudflared ConfigMap. The DNS line passes only for a visible CNAME to
  that tunnel. A proxied record answers with Cloudflare's addresses whatever it targets, so it is a SKIP, and
  `preview-auth-host` (the gateway's own `404` for the probe host) proves the routing. An address off Cloudflare, or a
  CNAME to anything else, is a WARN naming the `cloudflared tunnel route dns` command. Without that ConfigMap (a
  cloudflared of your own) it is a SKIP: check the record in the Cloudflare dashboard instead.

`patchy setup previews --edge cloudflare-tunnel --tunnel-credentials ~/.cloudflared/<tunnel id>.json`
([Setting up previews](../cli.md#setting-up-previews)) writes all of this: the values above, cloudflared's namespace,
config, Deployment (two replicas, the image pinned by digest, no ServiceAccount token, a read-only root) and a
NetworkPolicy that lets it reach only DNS, the gateway's Pods on 8082 and any public address on 443 and 7844 (where
Cloudflare's edge answers; the policy names no Cloudflare range), plus the credentials file as a Secret manifest in
cloudflared's namespace. It never reads `cert.pem`, which stays on your workstation.

**Certificate depth.** Cloudflare's free Universal SSL certificate covers a zone's apex and its first-level names only
(`acme-previews.dev` and `*.acme-previews.dev`). A preview host is `<label>.<hostSuffix>`, so with
`hostSuffix: preview.acme-apps.dev` every preview host is a second-level name that Universal SSL does not cover, and
browsers are shown an invalid certificate unless the zone has Advanced Certificate Manager (a paid add-on: an advanced
certificate for the wildcard, or Total TLS). The same holds for the sign-in host, which the tunnel also serves:
`preview-auth.acme.dev` is covered, `preview-auth.patchy.acme.dev` is not. Put previews on a domain of their own, which
keeps preview code off sign-in's site too, and make `hostSuffix` that domain itself. `setup previews` refuses deeper
names unless `--cloudflare-acm` says the zones have Advanced Certificate Manager. One registrable domain for both names
is refused unless acknowledged (`--shared-domain acknowledged`,
[Previews on the sign-in host's domain](#previews-on-the-sign-in-hosts-domain)), and then needs Advanced Certificate
Manager whatever the names: the sign-in host must sit outside the suffix, so the suffix cannot be the zone itself, and
every preview host is a second-level name.

**Locally-managed, not remotely-managed.** The recipe uses a locally-managed tunnel: its ingress rules live in
cloudflared's own config (a ConfigMap here), which accepts a wildcard hostname, first match wins, ending in a catch-all.
Whether a remotely-managed tunnel (configured in the dashboard or the API, run with a token) accepts a wildcard
published-application hostname was checked on 2026-10-17 and is not settled by Cloudflare's documentation: the
[routing docs](https://developers.cloudflare.com/tunnel/routing/) show single hostnames only and say the dashboard
creates each hostname's DNS record. Community reports say the dashboard does accept `*` as the subdomain but then
creates no wildcard record, so the proxied `*` CNAME is added by hand. Not verified against a live account (patchy calls
no provider API, owner decision Q2). Until it is, a remotely-managed tunnel works only if you add that record yourself,
and its rules then live outside anything patchy writes or `setup previews` can diff.

### The gateway's own TLS on a load balancer

No ingress controller: the gateway's Service is the load balancer, and the gateway terminates TLS itself from
[the wildcard certificate](#the-wildcard-certificate)'s Secret, re-reading it at every renewal. It suits MetalLB, k3s's
ServiceLB and a cloud L4 load balancer (`setup previews --edge gateway-tls`).

```yaml
previewAuth:
  gateway:
    enabled: true
    forwardedHops: 0 # the client connects to the gateway directly
    inboundCIDRs: [203.0.113.10/32] # optional; also the Service's loadBalancerSourceRanges
    ingress:
      enabled: false
    service:
      type: LoadBalancer
      externalTrafficPolicy: Local # keeps the client's address as the peer
      loadBalancerClass: "" # name one where the cluster has several implementations
    tls:
      secrets:
        - secretName: preview-wildcard-tls
          hosts: ["*.preview.acme-apps.dev", preview-auth.patchy.acme.dev]
```

Point both records, `*.<hostSuffix>` and the sign-in host, at the load balancer's address: the gateway answers the
sign-in host as the relay does. Under `externalTrafficPolicy: Cluster` kube-proxy source-NATs to a node address, which
`inboundCIDRs` would refuse and the rate limit would share among every viewer, so keep `Local`.

### A tailnet device through the Tailscale operator

The [Tailscale operator](https://tailscale.com/kb/1439/kubernetes-operator-cluster-ingress) gives the gateway's
ClusterIP Service a tailnet device of its own, and the gateway terminates TLS as in the
[load balancer recipe](#the-gateways-own-tls-on-a-load-balancer) (`setup previews --edge tailscale`). Only the tailnet
reaches the device, so this suits a desktop or home box; it needs no ingress controller and no port on the host, and
does not collide with Tailscale Funnel or `tailscale serve` there.

```yaml
previewAuth:
  gateway:
    enabled: true
    forwardedHops: 0 # the gateway sees the tailnet proxy's address, not the viewer's
    inboundCIDRs: [] # the tailnet policy is the allowlist
    ingress:
      enabled: false
    service:
      type: ClusterIP
      annotations:
        tailscale.com/expose: "true"
        tailscale.com/hostname: patchy-previews
        tailscale.com/tags: tag:patchy-previews
    tls:
      secrets:
        - secretName: preview-wildcard-tls
          hosts: ["*.preview.acme-apps.dev", preview-auth.patchy.acme.dev]
```

- **DNS.** MagicDNS takes no wildcard or custom records. Create both records in public DNS, pointing at the device's
  tailnet address (`tailscale ip -4 patchy-previews`): only the tailnet can reach it. Set `publishedAddresses` to that
  address for `patchy check previews`.
- **The tailnet policy is the allowlist.** Let viewers' group reach `tag:patchy-previews` on `tcp:443` alone. Grants
  only add access, so a new tailnet's allow-all rule must go first: while it stands, every member reaches the device on
  every port, the gateway's plain listener on 80 included. Viewers still sign in.
- **The operator's OAuth client** is its own Secret, created per Tailscale's guide; patchy's values never hold it.

### k3d with wildcard DNS and cert-manager

[A tailnet device](#a-tailnet-device-through-the-tailscale-operator) is the simpler way to run previews on a desktop k3d
cluster. This recipe is the alternative with an ingress controller.

A desktop cluster with no public load balancer. An ingress controller terminates TLS with
[the wildcard certificate](#the-wildcard-certificate), k3d's serverlb publishes it on the host's loopback, and something
that owns an address viewers reach (a tailnet device of its own, or the host's LAN address) forwards its `:443` there. A
public wildcard record points at that address. Nothing here is desktop-only patchy configuration: it is the
[Traefik](#traefik) recipe plus an address.

**The ingress controller.** k3s bundles Traefik, but k3d clusters are often created without it
(`--k3s-arg "--disable=traefik@server:*"`). Check with `kubectl get ingressclass`. If there is none, install one
yourself (the Traefik chart with only the `websecure` entrypoint, as a NodePort, or ingress-nginx) and use its recipe.

```sh
# The cluster, its load balancer bound to loopback only. With k3s's bundled Traefik, map 443; with a Traefik you
# installed as a NodePort, map that NodePort instead (127.0.0.1:18443:30443@loadbalancer).
k3d cluster create patchy -p "127.0.0.1:18443:443@loadbalancer"
# The dedicated preview node: added any time, without a rebuild. Its label and NoExecute taint are set when it
# registers, so no other Pod can land on it before the taint (a `kubectl taint` afterwards leaves that window open).
k3d node create preview --cluster patchy --role agent \
  --k3s-node-label patchy.acme.dev/previews=true \
  --k3s-arg "--node-taint=patchy.acme.dev/preview-only=true:NoExecute"
```

**The address viewers reach.** Do not bind the serverlb to a tailnet address's `:443` on a host that also uses Tailscale
Funnel or `tailscale serve` on 443: tailscaled already holds that port on its own address, and the two collide. Give the
previews a tailnet device of their own instead, for example the `tailscale/tailscale` container on the host network with
a serve configuration that forwards TCP, so TLS and SNI pass through to Traefik untouched:

```json
{ "TCP": { "443": { "TCPForward": "127.0.0.1:18443" } } }
```

On a LAN, binding the serverlb to the host's LAN address works the same, as long as nothing else holds its `:443`.

- DNS: an A record `*.preview.desktop.acme-apps.dev` (and the sign-in host) to that device's address, here
  `100.101.102.103`. MagicDNS has no wildcard records, so this is a public record pointing at a tailnet address: only
  the tailnet can reach it.
- Values: the Traefik recipe, with `preview.hostSuffix: preview.desktop.acme-apps.dev`, `preview.legacyIngress: false`
  and `publishedAddresses: [100.101.102.103]`.
- k3d's serverlb and a TCP forward are L4 proxies, so Traefik sees their address rather than the viewer's. Leave
  `inboundCIDRs` empty and restrict who reaches the address instead (tailnet ACLs, the host firewall). Traefik appends
  the address it sees, so `forwardedHops` stays `1`.
- k3s's bundled network policy controller enforces NetworkPolicy. On the desktop, node isolation is scheduling only (one
  host): it keeps other workloads off the preview node, not off the machine. Run the
  [isolation probe](#the-isolation-probe) with `--refused-is-blocked`.
- kube-router judges NetworkPolicy after the Service address is translated, so the Kubernetes API Service `/32` alone
  leaves the preview-controller unable to reach the API. Set `previewController.config.apiServerCIDR: auto`: a
  `helm upgrade` looks up the Service's address and the API server's endpoint addresses and admits both.

A [LoadBalancer exposure](#the-gateways-own-tls-on-a-load-balancer) needs no ingress controller at all.

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

| Check                       | What passes                                                                                                                                                                                                                                                                                                                                                                             |
| --------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `preview-dns`               | `<project>-0.<suffix>` resolves to the gateway's edge: `publishedAddresses`, else the gateway Ingress's or LoadBalancer Service's address. While both paths serve, the legacy preview load balancer passes too. A private edge not in this resolver's answer is a SKIP. A `signInHost` other than `previewAuth.host` gets a line of its own: it must resolve to the gateway's edge too. |
| `preview-tls`               | A certificate trusted for the probe host, and one trusted for a `signInHost` other than `previewAuth.host`                                                                                                                                                                                                                                                                              |
| `preview-auth-dex`          | Dex accepts the relay's client with `<issuer>/dex/callback`, as before                                                                                                                                                                                                                                                                                                                  |
| `preview-auth-host`         | `<project>-0.<suffix>`, which no Preview owns, answers with the gateway's own `404` page. Every Ready preview answers an unauthenticated navigation with the `302` to `https://<sign-in host>/start` for its own label. A host the gateway refuses (outside `inboundCIDRs`) is a SKIP.                                                                                                  |
| `preview-gateway-allowlist` | From an address the gateway refuses, a request whose `X-Forwarded-For` names an address inside `inboundCIDRs` is still refused. From an admitted address it is a SKIP: run it from outside.                                                                                                                                                                                             |

`patchy check previews -n <release namespace>` runs the same checks for the install as a whole, before any Project
exists: it asks `patchy-probe-0.<suffix>` (the relay's own probe host) and every Ready preview whatever its Project, and
adds `preview-config` (gateway-only, the sign-in host outside the suffix, the suffix on a registrable domain of its
own), `preview-viewers` (someone the relay can sign in is bound to the viewer role), `preview-nodes` (the preview nodes
carry both the selector and the taint) and `preview-upstreams` (cert-manager, the Tailscale operator and cloudflared,
where they run, are not older than the versions `setup previews` pins; older is a WARN). With `--isolation` it also runs
`preview-netpol`, the isolation probe below, from the CLI. See [the CLI guide](../cli.md#checking-the-preview-install).

The relay's background probe applies the same verdicts every minute (`patchy.preview_auth.unprotected_hosts`). In
gateway mode it also asks for `patchy-probe-0.<suffix>`, which must be the gateway's `404`.

## The isolation probe

`hack/preview-isolation-probe/ingress.sh` proves the slot NetworkPolicy from inside the cluster with kubectl alone. A
netprobe Pod listens in a slot. Three client Pods each try to connect to it: one in the agents namespace, one in the
release namespace without the gateway label, and one with it. The first two must be refused and the third admitted. The
image (under the preview image prefix, `sha-` tagged), the node selector and the taint key are flags, so it runs on k3d
as on EKS. On k3s and k3d add `--refused-is-blocked`: their policy controller, kube-router, rejects a denied connection
(the client sees "connection refused") rather than dropping it, and the flag counts that as blocked only after the
gateway's client has shown the listener answers. Run it after turning the gateway on and after any CNI or cluster
upgrade. `patchy check previews -n <release namespace> --isolation` runs the same probe from the CLI, always in the
`--refused-is-blocked` order, reading the placement, the namespaces and the image prefix from the install, and deletes
its Pods when done ([how](../cli.md#proving-slot-isolation---isolation)). Build the netprobe image from the same commit
as the script or the CLI. See its
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
- Previews on the sign-in host's registrable domain are same-site with it and with every other host there. patchy's own
  endpoints hold up to that; other apps on the domain may not. See
  [Previews on the sign-in host's domain](#previews-on-the-sign-in-hosts-domain).
