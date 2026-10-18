## patchy setup previews

Write the files that expose previews and sign viewers in to them

### Synopsis

Write the files that expose patchy's previews at https://<project>-<issue>.<host suffix>
and sign viewers in to them: patchy's chart values, the manifests the edge needs, the
relay's Dex client secret as two Secret manifests, answers.yaml and NEXT-STEPS.md, all
in -o (default ./patchy-previews, created 0700, every file 0600). Nothing is applied and
no provider is contacted: NEXT-STEPS.md lists the commands, in order.

Every answer is a flag. On a terminal, the command asks for each one the flags leave
unanswered, naming the flag that answers it, so a session can be repeated with
--non-interactive; without a terminal (or with --non-interactive) a missing answer is
an error naming its flag. A credential is never typed at a prompt: only the path of the
file that holds it.

No cluster is contacted unless --detect is given. --detect reads the cluster your
kubeconfig names, read-only and never a Secret: the nodes (the platform, and which carry
the preview label and taint), the kubernetes Service and the API server's EndpointSlice,
the kube-dns Service, IngressClasses, GatewayClasses, the cert-manager and Tailscale
operator CRDs, kube-system's Pod images (what enforces NetworkPolicy), the
kube-system/amazon-vpc-cni ConfigMap (whether the VPC CNI enforces it) and the release's
ConfigMaps in -n (an install's existing preview settings). A cluster it cannot reach at
all is an error. It prints what it found, and fills in only answers still open, even on
a re-run (an empty one, --api-server-cidr auto, or a false --cert-manager-installed or
--tailscale-operator-installed), never one a flag gives: the platform, the host suffix,
sign-in host and Dex issuer of an existing install, the kubernetes Service's /32 as
--api-server-cidr on EKS, and --cert-manager-installed and
--tailscale-operator-installed.

--edge existing routes through
an Ingress class or a Gateway API parent already in the cluster; --edge gateway-tls makes
the gateway's Service a LoadBalancer that
terminates TLS itself; --edge tailscale gives that Service a tailnet device through the
Tailscale operator. The last two write a cert-manager Certificate and, unless --dns manual,
the ACME Issuer that answers its DNS-01 challenge (a ClusterIssuer when cert-manager's own
identity answers, with no stored credential), plus values for cert-manager
and the Tailscale operator at the chart versions patchy pins. A DNS credential is read
from --dns-credentials-file or --dns-credentials-from-stdin, never a flag value, and is
written only into a Secret manifest.

--edge cloudflare-tunnel runs cloudflared in a namespace of its own, sending both names to
the gateway's Service through a locally-managed tunnel; Cloudflare's edge holds the
certificates. Create the tunnel first (cloudflared tunnel login, then cloudflared tunnel
create patchy-previews) and give its credentials file to --tunnel-credentials: it is
written only into a Secret manifest in cloudflared's namespace (cert.pem is refused, and
never leaves your workstation). Cloudflare's free Universal SSL covers a zone's first-level
names only, so the --host-suffix must be a registrable domain itself and the
--sign-in-host at most one label below its own, unless --cloudflare-acm says the zones
have Advanced Certificate Manager.

Previews must sit on a registrable domain of their own: a --sign-in-host or --dex-issuer
on the --host-suffix's registrable domain is refused, because preview code would run
same-site with sign-in. With only one domain, register a second, cheap one for previews.

A re-run starts from answers.yaml; flags given override it. It shows how each file would
change and writes nothing changed without --force or -y (on a terminal it asks). The
Dex client secret is read back
from the Secret files, never drawn again, unless --rotate dex-client-secret. --dry-run
prints the files and their differences (never a secret) and writes nothing.

```
patchy setup previews [flags]
```

### Examples

```
  patchy setup previews --detect -n patchy

  patchy setup previews --platform k3s --edge existing --ingress-class traefik \
    --host-suffix acme-previews.dev --sign-in-host preview-auth.patchy.acme.dev \
    --dex-issuer https://dex.patchy.acme.dev --viewer-team acme:qa --registry ghcr.io \
    --image-path-prefix acme/patchy/previews

  patchy setup previews --platform k3d --edge tailscale --dns route53 \
    --dns-option region=us-east-1 --dns-option accessKeyID=AKIAIOSFODNN7EXAMPLE \
    --dns-credentials-file ~/.config/dns01-secret-access-key ...

  patchy setup previews --platform k3s --edge cloudflare-tunnel \
    --tunnel-credentials ~/.cloudflared/<tunnel id>.json --host-suffix acme-previews.dev \
    --sign-in-host preview-auth.acme.dev ...
```

### Options

```
      --acme-email string               the ACME account's contact address (optional)
      --api-server-cidr string          the kubernetes Service's /32 the preview-controller may reach, or auto (a render-time lookup) (default "auto")
      --cert-manager-installed          cert-manager already runs: write no values for it
      --certificate-issuer string       --edge existing, or --dns manual: your cert-manager issuer, Issuer/<name> or ClusterIssuer/<name> (default Issuer/preview-dns01)
      --cloudflare-acm                  --edge cloudflare-tunnel: the zones have Advanced Certificate Manager, so names deeper than Universal SSL's first level are allowed
      --cloudflared-namespace string    --edge cloudflare-tunnel: the namespace cloudflared runs in, a new one its manifest creates and rolling back deletes (default patchy-cloudflared)
      --detect                          read the cluster (read-only, never a Secret) to fill in answers
      --dex-client-id string            the relay's static client at Dex (default "patchy-preview-auth")
      --dex-issuer string               your Dex's issuer URL (https)
      --dex-namespace string            the namespace Dex runs in (default "dex")
      --dns string                      --edge gateway-tls or tailscale: the DNS-01 solver of the Issuer written: route53, cloudflare, google, azure, digitalocean, rfc2136, manual (manual: your own --certificate-issuer)
      --dns-credentials-file string     a file holding the DNS-01 solver's credential (a token, a secret key, or google's JSON key), written only into a Secret manifest
      --dns-credentials-from-stdin      read the DNS-01 solver's credential from standard input instead
      --dns-option stringArray          a setting of the DNS-01 solver, key=value, never a secret (repeatable; route53: region, accessKeyID; google: project, serviceAccountKey; azure: subscriptionID, resourceGroupName, environment, clientID, tenantID; rfc2136: nameserver, tsigKeyName, tsigAlgorithm)
      --dry-run                         print the files and their differences; write nothing
      --edge string                     how previews reach viewers: cloudflare-tunnel, gateway-tls, tailscale, existing
      --edge-tls string                 --edge existing: certificate (a cert-manager Certificate in the release namespace) or edge (the edge holds it); default from the edge
      --eks-auto-mode string            --platform eks: the EKS Auto Mode NodePool and NodeClass preview nodes come from, <nodePool>/<nodeClass> (instead of --node-label)
      --force                           replace files that would change
      --gateway-parent string           --edge existing: the Gateway API parent that routes both names, <namespace>/<name>[/<section>]
  -h, --help                            help for previews
      --host-suffix string              previews are https://<label>.<host suffix>: a domain of their own
      --image-path-prefix string        the registry path preview images sit directly under (default "patchy/previews")
      --inbound-cidr stringArray        --edge gateway-tls: a client CIDR that may open previews, kept at the load balancer (repeatable; default all)
      --ingress-class string            --edge existing: the Ingress class that routes both names (traefik, nginx, alb, ...)
      --load-balancer-class string      --edge gateway-tls: the gateway Service's loadBalancerClass, where the cluster has more than one implementation
      --no-viewers                      name no viewer yet: every sign-in is refused until you do
      --node-isolation string           required or none (default: required, except on --platform other, which must say)
      --node-label string               the label preview nodes carry, key=value (default "patchy.bitwisemedia.uk/previews=true")
      --non-interactive                 ask nothing, even on a terminal: a missing answer is an error
  -o, --output string                   the directory to write to (default "patchy-previews")
      --platform string                 the cluster: k3d, k3s, eks, gke, aks, other
      --registry string                 the registry host preview images come from
      --release string                  the patchy Helm release (default "patchy")
      --rotate string                   draw a new secret although one is written: dex-client-secret
      --sign-in-host string             where viewers sign in, on another registrable domain
      --tailscale-address stringArray   --edge tailscale: the device's tailnet address once it has appeared, what the DNS records hold (repeatable)
      --tailscale-hostname string       --edge tailscale: the gateway's device name (default patchy-previews)
      --tailscale-operator-installed    --edge tailscale: the Tailscale operator already runs: write no values for it
      --tailscale-tag string            --edge tailscale: the device's ACL tag (default tag:patchy-previews)
      --taint-key string                the NoExecute taint preview nodes carry (default "patchy.bitwisemedia.uk/preview-only")
      --tunnel-credentials string       --edge cloudflare-tunnel: the locally-managed tunnel's credentials file (~/.cloudflared/<tunnel id>.json), written only into a Secret manifest
      --viewer-team stringArray         a Dex GitHub group that may view previews, <org>:<team-slug> (repeatable)
      --viewer-user stringArray         a GitHub login that may view previews (repeatable)
  -y, --yes                             write the changes shown without asking (as --force)
```

### Options inherited from parent commands

```
  -A, --all-namespaces             work across every namespace
      --context string             kubeconfig context to use
      --kubeconfig string          path to the kubeconfig file
  -n, --namespace string           namespace to work in (default: the context's)
      --no-color                   disable colour and styling
      --request-timeout duration   timeout for a single API call (default 30s)
  -v, --verbose                    log what the CLI is doing to stderr
```

### SEE ALSO

* [patchy setup](patchy_setup.md)	 - Create what patchy needs outside the cluster

