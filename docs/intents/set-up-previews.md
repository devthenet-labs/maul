# Set up previews

Every intent's pull request can run as a preview at its own URL, `https://<project>-<issue>.<host suffix>`, behind
sign-in. This page sets that up on any Kubernetes cluster with one command, `patchy setup previews`, which writes every
file an install needs and an ordered `NEXT-STEPS.md` for the rest. It replaces reading the
[preview gateway](../configuration/preview-gateway.md), [preview-auth](../configuration/preview-auth.md) and
[preview sign-in](preview-sign-in.md) pages end to end: those are now the reference for what the command writes.

The command calls no provider API. You (or your GitOps) apply the files, or `--apply` runs the in-cluster steps for you
([below](#applying-it-for-you)). Without `--detect` or `--apply` it does not read the cluster either.

## What every install needs

Previews run agent-written code, so each one gets its own browser origin: a preview can then never read another's
cookies, storage or service workers. That makes three things necessary on every install, whatever the platform:

1. **A wildcard DNS name**, `*.<host suffix>`, and one sign-in host outside it.
2. **A certificate for `*.<host suffix>`** and the sign-in host. A wildcard needs a DNS-01 challenge, or an edge that
   holds one (an ACM certificate, Cloudflare).
3. **One route on :443** that sends both names to the gateway's Service, `<release>-preview-gateway`.

Previews on paths of one host (`https://previews.acme.dev/<label>/`) are not offered and never will be: every preview
would share one origin.

Before you run the command, have these ready:

- **Two registrable domains, ideally.** The previews get one of their own, such as `acme-previews.dev`, and the sign-in
  host and Dex sit on another, such as `preview-auth.patchy.acme.dev`. Preview code is same-site with every name on its
  own registrable domain, so sign-in there would share its site with code you did not write. The command refuses one
  domain for both unless you accept it with `--shared-domain acknowledged`; read
  [Previews on the sign-in host's domain](../configuration/preview-gateway.md#previews-on-the-sign-in-hosts-domain)
  first, since every other app on that domain is then exposed to preview code. On Cloudflare a shared domain also needs
  Advanced Certificate Manager.
- **patchy installed with intents on** (`intentController.enabled: true`). Previews are built for intents. See
  [Deploying intents and previews](deploying.md) for the install itself.
- **Dex**, with its GitHub connector, as the [status page](../status-ui.md) already uses. The command generates the
  relay's client secret and prints the static client to add. Dex itself stays yours.
- **A CNI that enforces NetworkPolicy.** Slot isolation is NetworkPolicy. k3s enforces it with its bundled kube-router;
  on EKS, turn on the VPC CNI's network policy support; GKE needs Dataplane V2 or network policy enabled; AKS chooses
  its network policy when the cluster is created.
- **Somewhere to put preview nodes.** Slot Pods select a label and tolerate a `NoExecute` taint, by default
  `patchy.bitwisemedia.uk/previews=true` and `patchy.bitwisemedia.uk/preview-only`. `NEXT-STEPS.md` prints the node or
  node-pool command for your platform, with both set at registration.
- **Who may view previews:** GitHub teams (`--viewer-team acme:qa`) or logins (`--viewer-user octocat`). Every recipe
  ends with sign-in on; tailnet-only installs too.

## Pick a recipe

The edge (`--edge`) picks the recipe. Most installs need no ingress controller at all.

| Recipe                   | `--edge`            | Fits                                                                                  | TLS ends at                                    |
| ------------------------ | ------------------- | ------------------------------------------------------------------------------------- | ---------------------------------------------- |
| R1 Cloudflare Tunnel     | `cloudflare-tunnel` | A domain on Cloudflare, any cluster with no inbound port (a home box, public)         | Cloudflare's edge                              |
| R2 the gateway's own TLS | `gateway-tls`       | A cluster with a load balancer: a public IP, MetalLB, k3s ServiceLB, a cloud L4 LB    | The gateway, from a cert-manager DNS-01 Secret |
| R3 a tailnet device      | `tailscale`         | A home box or desktop, reachable on your tailnet only                                 | The gateway, as in R2                          |
| R4 your existing edge    | `existing`          | An Ingress class you run (ALB on EKS, ingress-nginx, Traefik) or a Gateway API parent | The edge, or a cert-manager Certificate        |

`--platform` (`k3d`, `k3s`, `eks`, `gke`, `aks`, `other`) only changes what `NEXT-STEPS.md` says about nodes,
NetworkPolicy and image pulls. With `--detect`, the command reads the cluster (read-only, never a Secret) and fills in
the platform, the API server's address and what is already installed; see
[the CLI guide](../cli.md#setting-up-previews).

## Run it

On a terminal, the command asks for each answer the flags leave open and names the flag that answers it, so a session
can be repeated with `--non-interactive`:

```sh
patchy setup previews --detect -n patchy
```

The same thing, answered in full for a home k3d box on a tailnet (R3), with a Route53 zone for the DNS-01 challenge:

```sh
patchy setup previews --non-interactive -n patchy --platform k3d --edge tailscale \
  --host-suffix acme-previews.dev --sign-in-host preview-auth.patchy.acme.dev \
  --dex-issuer https://dex.patchy.acme.dev --viewer-team acme:qa \
  --registry ghcr.io --image-path-prefix acme/patchy/previews \
  --dns route53 --dns-option region=us-east-1 --dns-option accessKeyID=AKIAIOSFODNN7EXAMPLE \
  --dns-credentials-file ~/.config/patchy/dns01-secret-access-key \
  --tailscale-oauth-dir ~/.config/patchy/tailscale-oauth
```

If the IAM user's key is already a profile in your AWS shared credentials file, name the profile instead of giving
`--dns-option accessKeyID` and a file of the secret:
`--dns route53 --dns-option region=us-east-1 --dns-aws-profile dns01`. The wizard reads that one profile (from
`--dns-credentials-file` if you give it, else `$AWS_SHARED_CREDENTIALS_FILE`, else `~/.aws/credentials`), sets
`accessKeyID` from it and writes only its secret access key into the Secret manifest; the file's other profiles are
never read. It refuses a profile holding a session token: temporary credentials expire, and cert-manager cannot renew
them.

It writes `./patchy-previews/` (created 0700, every file 0600):

| File                          | What it is                                                                                       |
| ----------------------------- | ------------------------------------------------------------------------------------------------ |
| `NEXT-STEPS.md`               | The steps in order, for your recipe and platform, ending with `patchy check previews`            |
| `answers.yaml`                | Every answer and the CLI version; a re-run starts from it. No secret                             |
| `patchy-previews.values.yaml` | patchy's chart values, layered after your own: `-f <your values> -f patchy-previews.values.yaml` |
| `manifests/`                  | The cert-manager Issuer and Certificate (R2, R3, R4 with in-cluster TLS) or cloudflared (R1)     |
| `upstream/`                   | Values for cert-manager and the Tailscale operator at the chart versions patchy pins             |
| `secrets/`                    | The relay's Dex client secret (twice: Dex's namespace and patchy's), any credential you gave     |

The files under `secrets/` hold the only copy of the Dex client secret and of any credential you gave. Apply them or
encrypt them (sops, sealed-secrets); never commit them as they are. A credential is only ever read from a file
(`--dns-credentials-file`, `--tunnel-credentials`, `--tailscale-oauth-dir`) or standard input, never from a flag value
or a prompt.

Then follow `NEXT-STEPS.md`. Every recipe has the same eight steps:

1. **Before you start:** what must already hold.
2. **Preview nodes:** the command that adds a labelled, tainted node or node pool.
3. **Preview image pulls:** slot Pods may not reference a Secret, so pulls are the node's own (a node role, a node pool
   identity, or k3s `registries.yaml` on preview nodes only), scoped to the preview image prefix.
4. **Dex:** the static client and its one redirect URI, `https://<sign-in host>/dex/callback`.
5. **TLS:** cert-manager, the Issuer and the Certificate, with the least-privilege DNS policy or token template for your
   DNS provider. Nothing to do where the edge holds the certificate.
6. **Install:** the edge's own pieces, then one `helm upgrade` of patchy. A gateway-only install has no permit stage.
7. **DNS:** the two records, `*.<host suffix>` and the sign-in host, and where they point.
8. **Check:** `patchy check previews -n <release namespace>`. Every line should pass before the first intent's preview
   is opened ([what each check reads](../cli.md#checking-the-preview-install)). Then prove slot isolation once with
   `--isolation`, which creates four short-lived probe Pods and deletes them
   ([how](../cli.md#proving-slot-isolation---isolation)).

It ends with a rollback section for the recipe, all by deletion in reverse order.

### Applying it for you

`--apply` runs the in-cluster parts of steps 4 to 8 itself, with your kubeconfig and the `helm` on your `PATH`: the
Secrets, cert-manager and the Tailscale operator at the pinned versions, the Issuer and the Certificate (waiting until
it is Ready) or cloudflared, one `helm upgrade` of patchy, then the checks. Name the values files the release already
runs with and the chart version to upgrade to; it never guesses either, and it upgrades a release that is already
installed, never installs one:

```sh
patchy setup previews -n patchy --apply --values values/patchy.yaml --chart-version 0.12.42 --dry-run
patchy setup previews -n patchy --apply --values values/patchy.yaml --chart-version 0.12.42
```

The first prints the steps as the commands that do them by hand. Each step is safe to repeat, and the first failure
stops the run with the commands that undo what it changed. Preview nodes (step 2), image pulls (step 3), Dex's static
client (step 4), the tailnet policy (step 6, saved before `--apply` runs) and the DNS records (step 7) stay yours. The
[CLI reference](../cli.md#setting-up-previews) lists every step.

## The recipes

### R1: Cloudflare Tunnel

cloudflared runs in a namespace of its own and connects out to Cloudflare, so the cluster needs no open port, no public
IP, no ingress controller and no cert-manager. Create the tunnel on your workstation first, then give the command its
credentials file:

```sh
cloudflared tunnel login
cloudflared tunnel create patchy-previews
patchy setup previews --edge cloudflare-tunnel --tunnel-credentials ~/.cloudflared/<tunnel id>.json ...
```

The command reads only the credentials file, into a Secret manifest in cloudflared's namespace. It never reads
`cert.pem`, which `tunnel login` writes and which can act on the whole zone: that file stays on your workstation. The
tunnel is locally managed (its rules live in a ConfigMap the command writes), because a wildcard rule is documented for
that kind only. Reference: [Cloudflare Tunnel](../configuration/preview-gateway.md#cloudflare-tunnel).

!!! warning "Cloudflare: certificate depth"

    Cloudflare's free Universal SSL certificate covers a zone's apex and its first-level names only. A preview host is
    `<label>.<host suffix>`, so make the host suffix a whole zone (`acme-previews.dev`, giving
    `shop-1.acme-previews.dev`), and the sign-in host at most one label below its own zone (`preview-auth.acme.dev`).
    With `--host-suffix preview.acme-apps.dev`, every preview host is a second-level name that Universal SSL does not
    cover, and browsers are shown an invalid certificate. The command refuses those names unless `--cloudflare-acm`
    says the zones have Advanced Certificate Manager, Cloudflare's paid add-on.

Cloudflare's edge terminates TLS, so Cloudflare sees preview traffic in the clear. Previews should hold staging data
only. `NEXT-STEPS.md` also has you add a Cache Rule that bypasses the cache for preview hosts, and leaves any allowlist
to Cloudflare (a WAF rule or Cloudflare Access).

### R2: the gateway's own TLS

The gateway's Service becomes a `LoadBalancer` with `externalTrafficPolicy: Local`, and the gateway terminates TLS
itself from a cert-manager Certificate for `*.<host suffix>` and the sign-in host, re-read at every renewal. No ingress
controller. The DNS-01 Issuer is namespaced, in the release namespace beside its one Certificate, so the DNS credential
serves nothing else; `--dns` picks its solver (`route53`, `cloudflare`, `google`, `azure`, `digitalocean`, `rfc2136`, or
`manual` with your own `--certificate-issuer`). `--inbound-cidr` keeps an allowlist at the load balancer, and
`--load-balancer-class` picks an implementation where the cluster has more than one. Both DNS records point at the load
balancer's address once it has one. Reference:
[the wildcard certificate](../configuration/preview-gateway.md#the-wildcard-certificate) and
[a LoadBalancer exposure](../configuration/preview-gateway.md#the-gateways-own-tls-on-a-load-balancer).

### R3: a tailnet device

R2's certificate, but instead of a load balancer the Tailscale operator gives the gateway's Service a tailnet device of
its own (`patchy-previews`, tagged `tag:patchy-previews`). Only the tailnet reaches it, and the tailnet policy is the
allowlist: `NEXT-STEPS.md` prints a complete policy that replaces a new tailnet's allow-all rule, so every member keeps
reaching their own devices (Tailscale SSH included) and your viewers' group reaches the device on 443 alone, with a
`tests` block Tailscale runs on every save: a policy that would lock you out of your own devices, or leave the device
open on port 80 or to another tagged device, is refused before it applies. Viewers still sign in. This is the recipe for
a desktop or home box: it needs no Traefik, no k3d port mapping and no second tailscaled, and it does not collide with
Tailscale Funnel or `tailscale serve` on the host. Reference:
[a tailnet device](../configuration/preview-gateway.md#a-tailnet-device-through-the-tailscale-operator).

The Tailscale operator needs an OAuth client of its own. `NEXT-STEPS.md` gives its exact recipe: the scopes **Devices >
Core**, **Keys > Auth Keys** and **General > Services**, each Write and tagged `tag:k8s-operator`, and a description of
letters, digits and spaces only (at most 50 characters). Save its ID and secret as the files `client_id` and
`client_secret` in a directory only you can read and pass it with `--tailscale-oauth-dir`: the command writes them only
into `secrets/tailscale-operator-oauth.secret.yaml`, the operator's `operator-oauth` Secret in namespace `tailscale`,
and reads them back on a re-run. It refuses an auth key or an API token given by mistake, and never prints either value.

!!! note "Tailscale: no wildcard in MagicDNS"

    MagicDNS takes no wildcard or custom records, so the two names are records in your **public** DNS that point at the
    device's tailnet address (`100.x.y.z`): only the tailnet can reach that address. The device exists once step 6 has
    installed the operator and patchy. Nothing needs running again: on a machine on the tailnet,
    `patchy check previews` asks its own Tailscale client (`tailscale status --json`, read-only) for the device and,
    while a record is missing or wrong, prints the exact records to create. `patchy setup previews --detect` does the
    same and records the address as `previewAuth.gateway.publishedAddresses`, which is optional. patchy never reads the
    address from the cluster: the operator keeps it only in a Secret, beside the device's keys.

### R4: your existing edge

The gateway gets an Ingress for `--ingress-class` (`alb`, `nginx`, `traefik`, ...) or an HTTPRoute on
`--gateway-parent <namespace>/<name>[/<section>]`, and the relay's own Ingress gets the same class and TLS, so the
sign-in host routes off EKS too. TLS is the edge's where it holds certificates (an ALB with ACM, a Gateway listener),
and otherwise a cert-manager Certificate the command writes; `--edge-tls certificate|edge` overrides that. Use it for
k3s's bundled Traefik, which `--detect` finds when it is enabled. Reference:
[EKS](../configuration/preview-gateway.md#eks-an-alb-ingress),
[ingress-nginx](../configuration/preview-gateway.md#ingress-nginx),
[Traefik](../configuration/preview-gateway.md#traefik) and
[Gateway API](../configuration/preview-gateway.md#gateway-api).

An EKS install that already serves previews through the preview ALB moves to the gateway in
[three steps, rolled back by DNS](../configuration/preview-gateway.md#moving-off-the-preview-alb-eks-release-n). The
command does not automate them; `--detect` says when the release is still on that path. An install managed by the
[terraform module](terraform-module.md) keeps the module's `helm_values` as its source of values: layer the generated
file after it only for what the module does not set.

## Re-running and changing recipe

A re-run starts from `answers.yaml`, shows how each file would change, and replaces a changed file only with `--force`
or `-y` (on a terminal it asks). `--dry-run` prints the files and their differences, never a secret, and writes nothing.
The Dex client secret is read back from the Secret files and never drawn again unless you pass
`--rotate dex-client-secret`. After a patchy upgrade, run it again to pick up changed values and newer pinned upstream
versions.

To move between recipes (say R3 to R1), run it again with the new `--edge`. It shows every change before it writes one,
and `NEXT-STEPS.md` then lists the new edge's steps; keep a copy of the old directory, which with the old DNS records is
the way back.

## Security

- Sign-in is not optional on any recipe, the tailnet one included: a tailnet often includes shared devices and other
  people's nodes.
- The command handles three kinds of credential: the Dex client secret it draws, a DNS-01 credential and a tunnel
  credentials file. It holds each in a redacted type, writes it only into a 0600 Secret manifest, never prints it, and
  never records it in `answers.yaml`. It warns when a credential file, or the output directory, is readable by other
  users.
- It ships no in-cluster component. cloudflared, cert-manager and the Tailscale operator are upstream projects you
  install; the command writes their values at versions patchy pins and tests, and `check previews` is how you prove the
  result. Its `preview-upstreams` line WARNs when one you run is older than the pin.
- `forwardedHops`, the number of proxies in front of the gateway that append `X-Forwarded-For`, is fixed per recipe (R1:
  1, R2 and R3: 0, R4: 1) rather than asked. A wrong value lets a client choose the address the rate limit and the
  allowlist judge.
- [The preview gateway's security notes](../configuration/preview-gateway.md#security) apply to every recipe.
