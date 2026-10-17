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
	GatewayNamespace, GatewayName, GatewaySection, SignInRouteFile string
	Files                                                          []fileEntry
}

// fileWhat says what each file a plan can write is.
var fileWhat = map[string]string{
	AnswersFile:         "Every answer, the edge and the CLI version: a re-run starts from it. No secret.",
	ValuesFile:          "patchy's chart values for previews, layered after your own.",
	NextStepsFile:       "This file.",
	CertificateFile:     "The cert-manager Certificate for `*.<suffix>` and the sign-in host.",
	SignInRouteFile:     "The HTTPRoute that sends the sign-in host to the preview gateway.",
	RelayDexSecretFile:  "The relay's Dex client secret, in the release namespace (0600).",
	DexClientSecretFile: "The same secret, in Dex's namespace, for its static client (0600).",
}

// renderNextSteps renders NEXT-STEPS.md for a and the recipe's own files.
func renderNextSteps(a Answers, recipeFiles []File) ([]byte, error) {
	paths := make([]string, 0, 5+len(recipeFiles))
	paths = append(paths, AnswersFile, ValuesFile, NextStepsFile, RelayDexSecretFile, DexClientSecretFile)
	for _, f := range recipeFiles {
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
		SignInRouteFile: SignInRouteFile,
	}
	for _, p := range paths {
		d.Files = append(d.Files, fileEntry{Path: p, What: fileWhat[p]})
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

// edgeTitle names the recipe as NEXT-STEPS.md's heading does.
func edgeTitle(a Answers) string {
	switch {
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
	intro := fmt.Sprintf("Every slot Pod selects `%s` and tolerates the taint `%s`. Add at least one node with "+
		"both, set when it registers so no other Pod lands on it first.", label, taint)
	block := func(cmd string) string { return intro + "\n\n```sh\n" + cmd + "```" }
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
			"`managedNodeGroups`, or terraform's `aws_eks_node_group`). On EKS Auto Mode, see " +
			"`preview.nodeIsolation.eksAutoMode` in the chart's values: it can render the NodePool and NodeClass."
	case PlatformGKE:
		return block(fmt.Sprintf("gcloud container node-pools create patchy-previews "+
			"--cluster <cluster> \\\n  --node-labels=%s --node-taints=%s\n", label, taint))
	case PlatformAKS:
		return block(fmt.Sprintf("az aks nodepool add --resource-group <group> --cluster-name "+
			"<cluster> --name previews \\\n  --labels %s --node-taints %s\n", label, taint))
	}
	return intro + " Add it however your platform adds nodes: the label and the taint must both be there before the " +
		"node is Ready."
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
