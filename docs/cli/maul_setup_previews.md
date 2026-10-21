## maul setup previews

Write the files that expose previews and sign viewers in to them

### Synopsis

Write the files that expose maul's previews at https://<project>-<issue>.<host suffix>
and sign viewers in to them: maul's chart values, the manifests the edge needs, the
relay's Dex client secret as two Secret manifests, answers.yaml and NEXT-STEPS.md, all
in -o (default ./maul-previews, created 0700, every file 0600). Nothing is applied
(unless --apply, below) and no provider is contacted: NEXT-STEPS.md lists the commands,
in order.

Every answer is a flag. On a terminal, the command asks for each one the flags leave
unanswered, naming the flag that answers it, so a session can be repeated with
--non-interactive; without a terminal (or with --non-interactive) a missing answer is
an error naming its flag. A credential is never typed at a prompt: only the path of the
file that holds it.

No cluster is contacted unless --detect or --apply is given. --detect reads the cluster your
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
--tailscale-operator-installed. With --edge tailscale, --detect also asks this machine's
own Tailscale client (tailscale status --json, read-only) for the gateway's tailnet device,
fills --tailscale-address from it while unanswered and prints the exact DNS records to
create; the operator keeps that address in the cluster only in a Secret, never read.

--edge existing routes through
an Ingress class or a Gateway API parent already in the cluster; --edge gateway-tls makes
the gateway's Service a LoadBalancer that
terminates TLS itself; --edge tailscale gives that Service a tailnet device through the
Tailscale operator. The last two write a cert-manager Certificate and, unless --dns manual,
the ACME Issuer that answers its DNS-01 challenge (a ClusterIssuer when cert-manager's own
identity answers, with no stored credential), plus values for cert-manager
and the Tailscale operator at the chart versions maul pins. A DNS credential is read
from --dns-credentials-file or --dns-credentials-from-stdin, never a flag value, and is
written only into a Secret manifest. For route53, --dns-aws-profile reads one profile's
access key out of an AWS shared credentials file (that file, else $AWS_SHARED_CREDENTIALS_FILE,
else ~/.aws/credentials) and sets accessKeyID from it; no other profile is read.
--tailscale-oauth-dir names a directory holding the
Tailscale operator's OAuth client as two files, client_id and client_secret, which are
written only into the operator's Secret manifest (secrets/tailscale-operator-oauth.secret.yaml);
NEXT-STEPS.md gives the client's exact scopes and tag.

--edge cloudflare-tunnel runs cloudflared in a namespace of its own, sending both names to
the gateway's Service through a locally-managed tunnel; Cloudflare's edge holds the
certificates. Create the tunnel first (cloudflared tunnel login, then cloudflared tunnel
create maul-previews) and give its credentials file to --tunnel-credentials: it is
written only into a Secret manifest in cloudflared's namespace (cert.pem is refused, and
never leaves your workstation). Cloudflare's free Universal SSL covers a zone's first-level
names only, so the --host-suffix must be a registrable domain itself and the
--sign-in-host at most one label below its own, unless --cloudflare-acm says the zones
have Advanced Certificate Manager.

Previews are best on a registrable domain of their own: a --sign-in-host or --dex-issuer
on the --host-suffix's registrable domain is refused unless you accept it with
--shared-domain acknowledged (on a terminal the command explains and asks), because
preview code then runs same-site with sign-in. maul's own side holds (its cookies are
__Host-, its sign-in endpoints and the status page's writes refuse same-site background
requests); what does not is
every other app on that domain, which must not rely on parent-domain cookies or on
SameSite for CSRF. The chart values then record the domain
(previewAuth.sharedSiteAcknowledged), and maul check previews reports the sharing as
INFO. A sign-in host under the --host-suffix is refused however acknowledged.

A re-run starts from answers.yaml; flags given override it. It shows how each file would
change and writes nothing changed without --force or -y (on a terminal it asks). The
Dex client secret is read back
from the Secret files, never drawn again, unless --rotate dex-client-secret. --dry-run
prints the files and their differences (never a secret) and writes nothing.

```
maul setup previews [flags]
```

### Examples

```
  maul setup previews --detect -n maul

  maul setup previews --platform k3s --edge existing --ingress-class traefik \
    --host-suffix acme-previews.dev --sign-in-host preview-auth.maul.acme.dev \
    --dex-issuer https://dex.maul.acme.dev --viewer-team acme:qa --registry ghcr.io \
    --image-path-prefix acme/maul/previews

  maul setup previews --platform k3d --edge tailscale --dns route53 \
    --dns-option region=us-east-1 --dns-option accessKeyID=AKIAIOSFODNN7EXAMPLE \
    --dns-credentials-file ~/.config/dns01-secret-access-key ...

  maul setup previews --platform k3d --edge tailscale --dns route53 \
    --dns-option region=us-east-1 --dns-aws-profile dns01 ...

  maul setup previews --platform k3s --edge cloudflare-tunnel \
    --tunnel-credentials ~/.cloudflared/<tunnel id>.json --host-suffix acme-previews.dev \
    --sign-in-host preview-auth.acme.dev ...

  maul setup previews --platform k3d --edge tailscale \
    --tailscale-oauth-dir ~/.config/maul/tailscale-oauth ...

  maul setup previews -n maul --apply --values values/maul.yaml --chart-version 0.12.42 --dry-run
```

### Options

```
      --acme-email string               the ACME account's contact address (optional)
      --api-server-cidr string          the kubernetes Service's /32 the preview-controller may reach, or auto (a render-time lookup) (default "auto")
      --apply                           after writing the files, run the in-cluster steps of NEXT-STEPS.md with your kubeconfig and helm: the Secrets, cert-manager and the Tailscale operator at the pinned versions, the Certificate, the maul upgrade, then the checks (with --dry-run: print them)
      --cert-manager-installed          cert-manager already runs: write no values for it
      --certificate-issuer string       --edge existing, or --dns manual: your cert-manager issuer, Issuer/<name> or ClusterIssuer/<name> (default Issuer/preview-dns01)
      --chart-version string            --apply: the maul chart version to upgrade the release to, such as 0.12.41
      --cloudflare-acm                  --edge cloudflare-tunnel: the zones have Advanced Certificate Manager, so names deeper than Universal SSL's first level are allowed
      --cloudflared-namespace string    --edge cloudflare-tunnel: the namespace cloudflared runs in, a new one its manifest creates and rolling back deletes (default maul-cloudflared)
      --detect                          read the cluster (read-only, never a Secret), and for --edge tailscale this machine's Tailscale client, to fill in answers
      --dex-client-id string            the relay's static client at Dex (default "maul-preview-auth")
      --dex-issuer string               your Dex's issuer URL (https)
      --dex-namespace string            the namespace Dex runs in (default "dex")
      --dns string                      --edge gateway-tls or tailscale: the DNS-01 solver of the Issuer written: route53, cloudflare, google, azure, digitalocean, rfc2136, manual (manual: your own --certificate-issuer)
      --dns-aws-profile string          --dns route53: read the access key of this profile of an AWS shared credentials file (--dns-credentials-file, else $AWS_SHARED_CREDENTIALS_FILE, else ~/.aws/credentials), and set accessKeyID from it
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
      --image-path-prefix string        the registry path preview images sit directly under (default "maul/previews")
      --inbound-cidr stringArray        --edge gateway-tls: a client CIDR that may open previews, kept at the load balancer (repeatable; default all)
      --ingress-class string            --edge existing: the Ingress class that routes both names (traefik, nginx, alb, ...)
      --load-balancer-class string      --edge gateway-tls: the gateway Service's loadBalancerClass, where the cluster has more than one implementation
      --no-viewers                      name no viewer yet: every sign-in is refused until you do
      --node-isolation string           required or none (default: required, except on --platform other, which must say)
      --node-label string               the label preview nodes carry, key=value (default "maul.run/previews=true")
      --non-interactive                 ask nothing, even on a terminal: a missing answer is an error
  -o, --output string                   the directory to write to (default "maul-previews")
      --platform string                 the cluster: k3d, k3s, eks, gke, aks, other
      --registry string                 the registry host preview images come from
      --release string                  the maul Helm release (default "maul")
      --rotate string                   draw a new secret although one is written: dex-client-secret
      --shared-domain string            acknowledged: accept the sign-in host or Dex on the --host-suffix's registrable domain, preview code then running same-site with them ("" withdraws it)
      --sign-in-host string             where viewers sign in, on another registrable domain unless --shared-domain
      --tailscale-address stringArray   --edge tailscale: the device's tailnet address once it has appeared, what the DNS records hold (repeatable)
      --tailscale-hostname string       --edge tailscale: the gateway's device name (default maul-previews)
      --tailscale-oauth-dir string      --edge tailscale: a directory holding the Tailscale operator's OAuth client as files client_id and client_secret, written only into its Secret manifest
      --tailscale-operator-installed    --edge tailscale: the Tailscale operator already runs: write no values for it
      --tailscale-tag string            --edge tailscale: the device's ACL tag (default tag:maul-previews)
      --taint-key string                the NoExecute taint preview nodes carry (default "maul.run/preview-only")
      --tunnel-credentials string       --edge cloudflare-tunnel: the locally-managed tunnel's credentials file (~/.cloudflared/<tunnel id>.json), written only into a Secret manifest
      --values stringArray              --apply: a values file the maul release already runs with (repeatable, in order); the upgrade layers maul-previews.values.yaml after them
      --viewer-team stringArray         a Dex GitHub group that may view previews, <org>:<team-slug> (repeatable)
      --viewer-user stringArray         a GitHub login that may view previews (repeatable)
      --wait-timeout duration           --apply: how long each Helm install and each wait may take (default 10m0s)
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

* [maul setup](maul_setup.md)	 - Create what maul needs outside the cluster

