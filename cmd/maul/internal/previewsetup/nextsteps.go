// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"bytes"
	"embed"
	"fmt"
	"slices"
	"strings"
	"text/template"
)

//go:embed templates/next-steps.md.tmpl
var templates embed.FS

var nextStepsTemplate = template.Must(template.ParseFS(templates, "templates/next-steps.md.tmpl"))

// fileEntry is one row of NEXT-STEPS.md's file table.
type fileEntry struct{ Path, What string }

// nextSteps is NEXT-STEPS.md's data: plain values, every one validated by
// Answers.Validate before it gets here.
type nextSteps struct {
	Version, EdgeTitle, Platform                                   string
	Release, Namespace, Suffix, SignIn, Registry, Prefix           string
	ClientID, SecretEnv, DexSecret, DexSecretKey, DexSecretFile    string
	RelaySecretFile, ValuesFile, ChartRef, ChartVersion            string
	APIServerCIDR, GatewayService                                  string
	NetworkPolicy, Nodes, Pulls                                    string
	Certificate                                                    bool
	IssuerKind, IssuerName, CertificateFile, CertificateName       string
	CertificateSecret                                              string
	Route                                                          bool
	EdgeClass                                                      bool
	GatewayNamespace, GatewayName, GatewaySection, SignInRouteFile string
	Files                                                          []fileEntry

	// The gateway-tls and tailscale recipes: the gateway terminates TLS.
	GatewayCert, GatewayTLS, Tailscale bool
	Issuer                             bool // the recipe writes the Issuer
	IssuerFile, DNS, DNSSecret         string
	DNSSecretFile, DNSSecretKey        string
	DNSCredentialNeeded                bool // the solver reads DNSSecret
	DNSCredentialWritten               bool // and the wizard wrote it
	DNSLeastPrivilege                  string
	AmbientIdentity                    string // how cert-manager gets the identity a ClusterIssuer spends
	AmbientStored                      string // the options that give the solver a stored credential instead
	CertManager, TailscaleOperator     *Pin   // nil: already installed, or not needed
	LBClass                            string
	InboundCIDRs                       []string
	ServiceLB                          bool // k3s's ServiceLB gives the LoadBalancer its address
	TailscaleHostname, TailscaleTag    string
	TailscaleOperatorTag, OAuthSecret  string
	TailscaleNamespace                 string
	TailscaleAddresses                 []string
	TailscaleRecords                   []string
	TailscalePolicy                    string
	PolicyOwner, PolicyViewer          string
	TailscaleOAuthFile                 string
	TailscaleOAuthWritten              bool // --tailscale-oauth-dir gave the operator's OAuth client

	// SharedSite is the registrable domain previews share with sign-in,
	// acknowledged ("" for none); SharedWith names what shares it, the
	// sign-in host, Dex or both (SharedBoth), and SharedDex says Dex does.
	SharedSite, SharedWith string
	// SignInShared is whether the sign-in host itself is on SharedSite, so
	// that one zone serves both names; Dex alone sharing leaves two.
	SignInShared          bool
	SharedBoth, SharedDex bool

	// The cloudflare-tunnel recipe: cloudflared and Cloudflare's edge.
	Cloudflare, AdvancedCertificates             bool
	SuffixSite, SignInSite, TunnelID, TunnelName string
	CloudflaredFile, CloudflaredSecretFile       string
	CloudflaredNamespace, CloudflaredSecret      string
}

// fileWhat says what each file a plan can write is.
var fileWhat = map[string]string{
	AnswersFile:         "Every answer, the edge and the CLI version: a re-run starts from it. No secret.",
	ValuesFile:          "maul's chart values for previews, layered after your own.",
	NextStepsFile:       "This file.",
	CertificateFile:     "The cert-manager Certificate for `*.<suffix>` and the sign-in host.",
	SignInRouteFile:     "The HTTPRoute that sends the sign-in host to the preview gateway.",
	RelayDexSecretFile:  "The relay's Dex client secret, in the release namespace (0600).",
	DexClientSecretFile: "The same secret, in Dex's namespace, for its static client (0600).",
	IssuerFile: "The ACME Issuer that answers the Certificate's DNS-01 challenge (a ClusterIssuer when " +
		"cert-manager's own identity answers).",
	DNSSecretFile: "The DNS-01 solver's credential, in the release namespace (0600).",
	CertManager.ValuesFile: "cert-manager's values, for the chart version maul pins (" + CertManager.Version +
		").",
	TailscaleOperator.ValuesFile: "The Tailscale operator's values, for the chart version maul pins (" +
		TailscaleOperator.Version + ").",
	CloudflaredFile: "cloudflared's namespace, config, Deployment (" + Cloudflared.Tag + ", by digest) and " +
		"NetworkPolicy.",
	CloudflaredSecretFile: "The tunnel's credentials, in cloudflared's namespace (0600).",
	TailscaleOAuthFile: "The Tailscale operator's OAuth client, Secret " + tailscaleOAuthSecret + " in its " +
		"namespace " + TailscaleOperator.Namespace + " (0600).",
}

// renderNextSteps renders NEXT-STEPS.md for a and the recipe's own files;
// secrets are the secret files the plan writes.
func renderNextSteps(a Answers, recipeFiles, secrets []File) ([]byte, error) {
	paths := make([]string, 0, 3+len(recipeFiles)+len(secrets))
	paths = append(paths, AnswersFile, ValuesFile, NextStepsFile)
	for _, f := range append(slices.Clone(recipeFiles), secrets...) {
		paths = append(paths, f.Path)
	}
	slices.Sort(paths)
	d := nextSteps{
		Version: a.CLIVersion, EdgeTitle: edgeTitle(a), Platform: string(a.Platform),
		Release: a.Release, Namespace: a.Namespace, Suffix: a.HostSuffix, SignIn: a.SignInHost,
		Registry: a.Registry, Prefix: a.ImagePathPrefix,
		ClientID: a.Dex.ClientID, SecretEnv: DexSecretEnv, DexSecret: DexClientSecret,
		DexSecretKey: DexClientSecretKey, DexSecretFile: DexClientSecretFile,
		RelaySecretFile: RelayDexSecretFile, ValuesFile: ValuesFile, ChartRef: ChartRef,
		ChartVersion: ChartVersion(a.CLIVersion), APIServerCIDR: a.APIServerCIDR, GatewayService: a.GatewayService(),
		NetworkPolicy: networkPolicyNote(a.Platform), Nodes: nodesNote(a), Pulls: pullsNote(a),
		CertificateFile: CertificateFile, CertificateName: CertificateName, CertificateSecret: CertificateSecret,
		SignInRouteFile: SignInRouteFile, EdgeClass: a.ChartEdgeClass(),
	}
	for _, p := range paths {
		d.Files = append(d.Files, fileEntry{Path: p, What: fileWhat[p]})
	}
	sharedSiteSteps(a, &d)
	if a.GatewayTerminatesTLS() {
		gatewayCertSteps(a, &d, paths)
	}
	if a.Edge == EdgeCloudflareTunnel {
		d.Cloudflare, d.AdvancedCertificates = true, a.Cloudflare.AdvancedCertificates
		d.SuffixSite, _ = Site(a.HostSuffix)
		d.SignInSite, _ = Site(a.SignInHost)
		d.TunnelID, d.TunnelName = a.Cloudflare.TunnelID, TunnelName
		d.CloudflaredFile, d.CloudflaredSecretFile = CloudflaredFile, CloudflaredSecretFile
		d.CloudflaredNamespace, d.CloudflaredSecret = a.CloudflaredNamespace(), CloudflaredSecret
	}
	if a.Edge == EdgeExisting {
		if a.Existing.GatewayParent != "" {
			d.Route = true
			d.GatewayNamespace, d.GatewayName, d.GatewaySection, _ = ParseGatewayParent(a.Existing.GatewayParent)
		} else if a.EdgeTLS() == TLSCertificate {
			d.Certificate = true
			d.IssuerKind, d.IssuerName = a.Issuer()
		}
	}
	var buf bytes.Buffer
	if err := nextStepsTemplate.Execute(&buf, d); err != nil {
		return nil, fmt.Errorf("render %s: %w", NextStepsFile, err)
	}
	return buf.Bytes(), nil
}

// sharedSiteSteps fills NEXT-STEPS.md's data for an acknowledged shared
// registrable domain: what shares it.
func sharedSiteSteps(a Answers, d *nextSteps) {
	if d.SharedSite = a.SharedSite(); d.SharedSite == "" {
		return
	}
	var with []string
	if SharesSite(a.HostSuffix, a.SignInHost) {
		with = append(with, "the sign-in host `"+a.SignInHost+"`")
		d.SignInShared = true
	}
	if dex := issuerHost(a.Dex.Issuer); SharesSite(a.HostSuffix, dex) {
		with = append(with, "Dex (`"+dex+"`)")
		d.SharedDex = true
	}
	d.SharedWith, d.SharedBoth = strings.Join(with, " and "), len(with) > 1
}

// gatewayCertSteps fills NEXT-STEPS.md's data for the recipes in which the
// gateway terminates TLS with the Certificate they write.
func gatewayCertSteps(a Answers, d *nextSteps, paths []string) {
	d.GatewayCert, d.Certificate = true, true
	d.GatewayTLS, d.Tailscale = a.Edge == EdgeGatewayTLS, a.Edge == EdgeTailscale
	d.IssuerKind, d.IssuerName = a.Issuer()
	d.Issuer, d.IssuerFile, d.DNS = a.WritesIssuer(), IssuerFile, a.Certificate.DNS
	d.DNSSecret, d.DNSSecretFile, d.DNSSecretKey = DNSSecret, DNSSecretFile, a.DNSCredentialKey()
	d.DNSCredentialNeeded = d.DNSSecretKey != ""
	d.DNSCredentialWritten = slices.Contains(paths, DNSSecretFile)
	d.DNSLeastPrivilege = leastPrivilegeNote(a)
	if a.AmbientIssuer() {
		d.AmbientIdentity = ambientIdentityNote(a)
		d.AmbientStored = storedCredentialOptions[a.Certificate.DNS]
	}
	if !a.Certificate.CertManagerInstalled {
		d.CertManager = &CertManager
	}
	d.LBClass, d.InboundCIDRs = a.LoadBalancer.Class, a.LoadBalancer.InboundCIDRs
	d.ServiceLB = a.Platform == PlatformK3s || a.Platform == PlatformK3d
	if d.Tailscale {
		if !a.Tailscale.OperatorInstalled {
			d.TailscaleOperator = &TailscaleOperator
		}
		d.TailscaleHostname, d.TailscaleTag = a.TailscaleHostname(), a.TailscaleTag()
		d.TailscaleOperatorTag, d.OAuthSecret = TailscaleOperatorTag, tailscaleOAuthSecret
		d.TailscaleNamespace = TailscaleOperator.Namespace
		d.TailscaleAddresses = a.Tailscale.Addresses
		d.TailscaleRecords = DNSRecords(a.HostSuffix, a.SignInHost, a.Tailscale.Addresses)
		d.TailscalePolicy = tailscalePolicy(a)
		d.PolicyOwner, d.PolicyViewer = policyOwner, policyViewer
		d.TailscaleOAuthFile = TailscaleOAuthFile
		d.TailscaleOAuthWritten = slices.Contains(paths, TailscaleOAuthFile)
	}
}

// leastPrivilegeNote is the narrowest grant the DNS-01 solver needs
// (design §6): only the two challenge names' TXT records, in only their
// zones. Printed, never applied (owner decision Q2).
func leastPrivilegeNote(a Answers) string {
	suffix, signIn := "_acme-challenge."+a.HostSuffix, "_acme-challenge."+a.SignInHost
	who := "cert-manager's own identity (its ServiceAccount's IAM role)"
	switch a.Certificate.DNS {
	case "route53":
		if a.Certificate.DNSOptions["accessKeyID"] != "" {
			who = "the IAM user whose access key you give"
		}
		return "Grant " + who + " this policy and nothing else: only TXT records, only the two challenge " +
			"names, in only the zones that hold them.\n\n```json\n" + route53Policy(suffix, signIn) + "```"
	case "cloudflare":
		return "Create an API token from Cloudflare's \"Edit zone DNS\" template with exactly `Zone:DNS:Edit` " +
			"and `Zone:Zone:Read`, its zone resources limited to the zones holding `" + a.HostSuffix + "` and `" +
			a.SignInHost + "` (never \"All zones\", never the Global API Key)."
	case "google":
		return "Give the solver's service account `roles/dns.admin` on only the managed zones holding `" +
			a.HostSuffix + "` and `" + a.SignInHost + "` where your organisation allows zone-level IAM, else on " +
			"a project that holds DNS and nothing else."
	case "azure":
		return "Give the solver's identity `DNS Zone Contributor` on only the DNS zones holding `" + a.HostSuffix +
			"` and `" + a.SignInHost + "`, never on the resource group or the subscription."
	case "digitalocean":
		return "Create a custom-scoped token with only `domain:read` and `domain:update`. DigitalOcean cannot " +
			"scope a token to one domain: it can change every record of every domain in the account."
	case "rfc2136":
		return "Let the TSIG key update only the TXT records `" + suffix + "` and `" + signIn + "` (BIND: " +
			"`update-policy { grant " + a.Certificate.DNSOptions["tsigKeyName"] + " name " + suffix + ". TXT; " +
			"grant " + a.Certificate.DNSOptions["tsigKeyName"] + " name " + signIn + ". TXT; };`)."
	}
	return ""
}

// storedCredentialOptions are the --dns-option values that turn an ambient
// solver into one reading a stored credential.
var storedCredentialOptions = map[string]string{
	"route53": "--dns-option accessKeyID=<AKIA...>",
	"google":  "--dns-option serviceAccountKey=true",
	"azure":   "--dns-option clientID=<id> --dns-option tenantID=<id>",
}

// ambientIdentityNote is how cert-manager's own ServiceAccount (release
// and namespace as NEXT-STEPS.md installs it) gets the identity an
// ambient solver answers with. Printed, never applied (owner decision Q2).
func ambientIdentityNote(a Answers) string {
	sa := "`" + CertManager.Namespace + "/" + CertManager.Release + "`"
	switch a.Certificate.DNS {
	case "route53":
		return "Give cert-manager's ServiceAccount " + sa + " an IAM role: an EKS Pod Identity association, or " +
			"IRSA (`serviceAccount.annotations: {eks.amazonaws.com/role-arn: <the role's ARN>}` in cert-manager's " +
			"values)."
	case "google":
		return "Give cert-manager's ServiceAccount " + sa + " a Google service account through GKE Workload " +
			"Identity: `serviceAccount.annotations: {iam.gke.io/gcp-service-account: <its email>}` in cert-manager's " +
			"values, and `roles/iam.workloadIdentityUser` on that service account for `serviceAccount:" +
			a.Certificate.DNSOptions["project"] + ".svc.id.goog[" + CertManager.Namespace + "/" +
			CertManager.Release + "]` (the cluster's own project, if DNS is in another)."
	case "azure":
		return "Give cert-manager's ServiceAccount " + sa + " a managed identity through Azure Workload Identity: " +
			"`podLabels` and `serviceAccount.labels` `{azure.workload.identity/use: \"true\"}` in cert-manager's " +
			"values, and a federated credential on the identity for `system:serviceaccount:" +
			CertManager.Namespace + ":" + CertManager.Release + "`."
	}
	return ""
}

// route53Policy is the desktop's DNS-01 policy (terraform-devthenet
// maul-desktop-previews.tf), parameterised by the two challenge names.
func route53Policy(suffix, signIn string) string {
	return `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "AcmeChallengeRecordsOnly",
      "Effect": "Allow",
      "Action": ["route53:ChangeResourceRecordSets"],
      "Resource": [
        "arn:aws:route53:::hostedzone/<the zone holding ` + strings.TrimPrefix(suffix, "_acme-challenge.") + `>",
        "arn:aws:route53:::hostedzone/<the zone holding ` + strings.TrimPrefix(signIn, "_acme-challenge.") + `>"
      ],
      "Condition": {
        "ForAllValues:StringEquals": {
          "route53:ChangeResourceRecordSetsNormalizedRecordNames": [
            "` + suffix + `",
            "` + signIn + `"
          ],
          "route53:ChangeResourceRecordSetsRecordTypes": ["TXT"],
          "route53:ChangeResourceRecordSetsActions": ["UPSERT", "DELETE"]
        }
      }
    },
    {
      "Sid": "ChangeStatus",
      "Effect": "Allow",
      "Action": ["route53:GetChange"],
      "Resource": "arn:aws:route53:::change/*"
    },
    {
      "Sid": "FindZone",
      "Effect": "Allow",
      "Action": ["route53:ListHostedZonesByName"],
      "Resource": "*"
    }
  ]
}
`
}

// The placeholders the tailnet policy carries: tailnet logins maul
// cannot know (viewers sign in to previews with GitHub, not with these).
const (
	policyOwner  = "<your tailnet login>"
	policyViewer = "<a viewer's tailnet login>"
)

// tailscalePolicy is the complete, safe tailnet policy the tailscale
// recipe needs (design §6, setup-previews-easy (c)), in Tailscale's HuJSON:
// it replaces allow-all with every member reaching their own devices
// (autogroup:self, so Tailscale SSH keeps its network access) and the
// viewers reaching the previews' device on 443 alone, lets the operator
// own its tags, and carries tests Tailscale runs on every save, so a
// policy that locks the owner out, or leaves the device open on 80 or to
// another tagged device, is refused before it applies.
func tailscalePolicy(a Answers) string {
	tag := a.TailscaleTag()
	return `{
  // Who may reach the previews' device: tailnet logins (each still signs in).
  "groups": {
    "group:preview-viewers": ["` + policyViewer + `"]
  },
  "tagOwners": {
    "` + TailscaleOperatorTag + `": [],
    "` + TailscaleProxyTag + `": ["` + TailscaleOperatorTag + `"],
    "` + tag + `": ["` + TailscaleOperatorTag + `"]
  },
  // These replace allow-all: nothing else is reachable unless granted here.
  "grants": [
    // Every member keeps reaching their own devices, Tailscale SSH included.
    {"src": ["autogroup:member"], "dst": ["autogroup:self"], "ip": ["*"]},
    // Viewers reach the previews' device on 443 alone.
    {"src": ["group:preview-viewers"], "dst": ["` + tag + `"], "ip": ["tcp:443"]}
  ],
  // Keep your "ssh" and "nodeAttrs" sections, and any other, as they are.
  // Tailscale runs these on every save and refuses a policy that fails one.
  "tests": [
    {"src": "` + policyOwner + `", "accept": ["` + policyOwner + `:22"]},
    {"src": "` + policyViewer + `", "accept": ["` + tag + `:443"], "deny": ["` + tag + `:80"]},
    {"src": "` + TailscaleProxyTag + `", "deny": ["` + tag + `:443", "` + tag + `:80"]}
  ]
}
`
}

// edgeTitle names the recipe as NEXT-STEPS.md's heading does.
func edgeTitle(a Answers) string {
	switch {
	case a.Edge == EdgeGatewayTLS:
		return "gateway TLS on a LoadBalancer"
	case a.Edge == EdgeTailscale:
		return "tailnet device (Tailscale operator)"
	case a.Edge == EdgeCloudflareTunnel:
		return "Cloudflare Tunnel"
	case a.Edge != EdgeExisting:
		return string(a.Edge)
	case a.Existing.GatewayParent != "":
		return "existing edge: Gateway API"
	case a.Existing.IngressClass != "":
		return "existing edge: Ingress class " + a.Existing.IngressClass
	}
	return "existing edge: the chart's edge ALB"
}

// networkPolicyNote is how the platform enforces NetworkPolicy, which the
// slot isolation relies on.
func networkPolicyNote(p Platform) string {
	switch p {
	case PlatformK3d, PlatformK3s:
		return "k3s enforces it with its embedded kube-router unless the server runs with " +
			"`--disable-network-policy`."
	case PlatformEKS:
		return "On EKS, turn on the VPC CNI's network policy support (on Auto Mode, " +
			"`enable-network-policy-controller: \"true\"` in the `amazon-vpc-cni` ConfigMap in kube-system), or " +
			"run a CNI that enforces it."
	case PlatformGKE:
		return "On GKE, use GKE Dataplane V2, or enable network policy enforcement on the cluster."
	case PlatformAKS:
		return "On AKS, the network policy engine (Azure, Calico or Cilium) is chosen when the cluster is " +
			"created: check `networkProfile.networkPolicy` is not none."
	}
	return "Check that your CNI enforces it: without enforcement, preview code can reach the rest of the cluster."
}

// nodesNote is how to add the dedicated, tainted preview nodes, with the
// label and taint set at registration (G4), so nothing else schedules
// onto them in between. No cloud API is called (owner decision Q2).
func nodesNote(a Answers) string {
	n := a.NodeIsolation
	label := n.Label + "=" + n.LabelValue
	taint := n.TaintKey + "=true:NoExecute"
	if a.IsolationMode() == IsolationNone {
		return "Node isolation is off (`--node-isolation none`): preview Pods share nodes with everything else. " +
			"Remove any `preview.nodeIsolation.selector` from your own values, which the chart refuses with " +
			"mode none. Add a dedicated node and run the command again with `--node-isolation required` as soon " +
			"as you can."
	}
	// merged is what layering the file cannot undo: Helm merges a selector
	// map, so a key in the operator's own values stays required.
	merged := "\n\n`" + ValuesFile + "` sets `preview.nodeIsolation` over your own values, but Helm merges its " +
		"`selector` map: remove any `preview.nodeIsolation.selector` from your own values, or Pods require those " +
		"labels too."
	if eks := n.EKSAutoMode; eks != (EKSAutoMode{}) {
		return fmt.Sprintf("Every slot Pod selects the EKS Auto Mode NodePool `%s` and NodeClass `%s` and "+
			"tolerates the taint `%s`. Either the NodePool is yours, and taints its nodes with exactly that, or "+
			"set `preview.nodeIsolation.create: true` with `role`, `subnetIDs` and `securityGroupIDs` in your own "+
			"values and the chart renders both. Auto Mode also needs `preview.dnsCIDR` (the node-local cluster "+
			"DNS address, a /32) in your own values: the chart refuses to render without it.",
			eks.NodePool, eks.NodeClass, taint) + merged
	}
	intro := fmt.Sprintf("Every slot Pod selects `%s` and tolerates the taint `%s`. Add at least one node with "+
		"both, set when it registers so no other Pod lands on it first.", label, taint)
	block := func(cmd string) string { return intro + "\n\n```sh\n" + cmd + "```" + merged }
	switch a.Platform {
	case PlatformK3d:
		return block(fmt.Sprintf("k3d node create preview --cluster <cluster> --role agent "+
			"--k3s-node-label %s \\\n  --k3s-arg \"--node-taint=%s\"\n", label, taint)) + "\n\n" +
			"On one host this isolates scheduling only: the preview node shares the host's kernel."
	case PlatformK3s:
		return block(fmt.Sprintf("curl -sfL https://get.k3s.io | K3S_URL=https://<server>:6443 "+
			"K3S_TOKEN=<token> sh -s - agent \\\n  --node-label %s --node-taint %s\n", label, taint)) + "\n\n" +
			"Never `kubectl taint` a node that already runs Pods: those Pods would be evicted, and new ones could " +
			"land before the taint."
	case PlatformEKS:
		return intro + " Use a managed node group with `labels: {" + n.Label + ": \"" + n.LabelValue + "\"}` and " +
			"`taints: [{key: " + n.TaintKey + ", value: \"true\", effect: NoExecute}]` (eksctl's " +
			"`managedNodeGroups`, or terraform's `aws_eks_node_group`). On EKS Auto Mode, run the command again " +
			"with `--eks-auto-mode <nodePool>/<nodeClass>`: this file clears `preview.nodeIsolation.eksAutoMode`, " +
			"so a pair in your own values no longer applies." + merged
	case PlatformGKE:
		return block(fmt.Sprintf("gcloud container node-pools create maul-previews "+
			"--cluster <cluster> \\\n  --node-labels=%s --node-taints=%s\n", label, taint))
	case PlatformAKS:
		return block(fmt.Sprintf("az aks nodepool add --resource-group <group> --cluster-name "+
			"<cluster> --name previews \\\n  --labels %s --node-taints %s\n", label, taint))
	}
	return intro + " Add it however your platform adds nodes: the label and the taint must both be there before the " +
		"node is Ready." + merged
}

// pullsNote is how preview nodes pull preview images (owner decision Q4:
// node-level only).
func pullsNote(a Answers) string {
	scope := fmt.Sprintf("`%s/*`", a.ImagePathPrefix)
	switch a.Platform {
	case PlatformEKS:
		if strings.Contains(a.Registry, ".dkr.ecr.") {
			return "give the preview nodes' IAM role ECR pull on " + scope + " only."
		}
	case PlatformGKE:
		return "give the preview node pool's service account Artifact Registry Reader on the repository " +
			"holding " + scope + " only."
	case PlatformAKS:
		return "give the preview node pool's kubelet identity AcrPull on the repository holding " + scope + " only."
	case PlatformK3d, PlatformK3s:
		return "a private registry needs credentials on the preview nodes only: k3s's `registries.yaml` " +
			"(`configs.\"" + a.Registry + "\".auth`) or a kubelet credential provider, with a key that can pull " +
			scope + " and nothing else. Public images need nothing, but make preview images of private code public " +
			"only knowingly."
	}
	return "a key the preview nodes' container runtime holds, able to pull " + scope + " and nothing else, or " +
		"public images (only knowingly, for private code)."
}
