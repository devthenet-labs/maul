// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewdetect

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/bitwise-media-group/patchy/cmd/patchy/internal/previewcheck"
)

// The CRDs whose presence says an upstream component the recipes write
// values for already runs.
const (
	// CertManagerCRD is cert-manager's Certificate.
	CertManagerCRD = "certificates.cert-manager.io"
	// TailscaleCRD is the Tailscale operator's ProxyClass, which every
	// release of its chart installs.
	TailscaleCRD = "proxyclasses.tailscale.com"
)

// The objects detection reads by name.
const (
	apiServiceNamespace = "default"
	apiServiceName      = "kubernetes"
	systemNamespace     = "kube-system"
	clusterDNSService   = "kube-dns"
	// vpcCNIConfigMap is the VPC CNI's own ConfigMap in kube-system, and
	// KeyVPCCNIPolicyController its switch for the network policy
	// controller, without which EKS enforces no NetworkPolicy.
	vpcCNIConfigMap           = "amazon-vpc-cni"
	KeyVPCCNIPolicyController = "enable-network-policy-controller"
	// policyAgentImage is the VPC CNI network policy agent's image name.
	policyAgentImage = "aws-network-policy-agent"
)

// IngressClassDefault is the annotation that marks the cluster's default
// IngressClass.
const IngressClassDefault = "ingressclass.kubernetes.io/is-default-class"

var (
	crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1",
		Kind: "CustomResourceDefinition"}
	gatewayClassGVK = schema.GroupVersionKind{Group: "gateway.networking.k8s.io", Version: "v1",
		Kind: "GatewayClassList"}
)

// Node is what detection keeps of a Node: never its status or addresses.
type Node struct {
	Name       string
	ProviderID string
	Labels     map[string]string
	Taints     []corev1.Taint
}

// IngressClass is one IngressClass: its name, controller and whether it is
// the cluster's default.
type IngressClass struct {
	Name       string
	Controller string
	Default    bool
}

// Presence is whether something was found: Unknown when the read failed.
type Presence int

const (
	Unknown Presence = iota
	Absent
	Present
)

// Observation is what Observe read: the raw facts Judge decides from.
type Observation struct {
	Nodes []Node
	// APIServiceIPs are the default/kubernetes Service's cluster IPs, and
	// APIEndpoints its EndpointSlices' IPv4 addresses.
	APIServiceIPs []string
	APIEndpoints  []string
	// ClusterDNS is the kube-system/kube-dns Service's cluster IP.
	ClusterDNS     string
	IngressClasses []IngressClass
	GatewayClasses []string
	CertManager    Presence
	Tailscale      Presence
	// Images are the kube-system Pods' container images, sorted, each once.
	Images []string
	// PolicyAgentArgs are the command and arguments of each kube-system
	// container running the VPC CNI network policy agent, which ships
	// switched off (--enable-network-policy=false).
	PolicyAgentArgs [][]string
	// VPCCNIPolicyController is the kube-system/amazon-vpc-cni ConfigMap's
	// enable-network-policy-controller setting, "" when unset or absent.
	VPCCNIPolicyController string
	// Release is the patchy release's preview settings, from its ConfigMaps.
	Release previewcheck.Settings
	// Notes are the reads that failed or were refused.
	Notes []string
	// Unreachable is set when no read reached the API server at all (every
	// one failed before the server answered): the first such failure. A
	// refusal (Forbidden) is an answer, so a cluster that refuses every
	// read is reached.
	Unreachable error
}

// Observe reads the cluster for detection. It reads Nodes, two Services and
// one EndpointSlice by name, IngressClasses, GatewayClasses, two CRDs by
// name, kube-system's Pods, the kube-system/amazon-vpc-cni ConfigMap and
// namespace's patchy ConfigMaps: never a Secret, and it writes nothing. A
// failed read becomes a note; when none reached the API server,
// Unreachable says so.
func Observe(ctx context.Context, r client.Reader, namespace string) Observation {
	o := &Observation{}
	rr := &reachReader{Reader: r}
	o.observeNodes(ctx, rr)
	o.observeAPIServer(ctx, rr)
	o.observeEdges(ctx, rr)
	o.CertManager = o.crdPresence(ctx, rr, CertManagerCRD)
	o.Tailscale = o.crdPresence(ctx, rr, TailscaleCRD)
	o.observeSystemPods(ctx, rr)
	o.observeVPCCNI(ctx, rr)
	o.Release = previewcheck.LoadSettings(ctx, rr, namespace)
	if err := o.Release.Preview.Err; err != nil {
		o.Notes = append(o.Notes, err.Error())
	}
	if !rr.reached {
		o.Unreachable = rr.first
	}
	return *o
}

// reachReader records whether any read reached the API server: one that
// succeeded, or that the server itself answered with a status (a refusal,
// a not found) or whose kind discovery found unserved.
type reachReader struct {
	client.Reader
	reached bool
	first   error
}

func (r *reachReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object,
	opts ...client.GetOption) error {
	return r.saw(r.Reader.Get(ctx, key, obj, opts...))
}

func (r *reachReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return r.saw(r.Reader.List(ctx, list, opts...))
}

func (r *reachReader) saw(err error) error {
	var status apierrors.APIStatus
	switch {
	case err == nil, errors.As(err, &status), meta.IsNoMatchError(err):
		r.reached = true
	case r.first == nil:
		r.first = err
	}
	return err
}

// note records a read that failed.
func (o *Observation) note(what string, err error) {
	o.Notes = append(o.Notes, fmt.Sprintf("could not read %s: %v", what, err))
}

// observeNodes keeps each node's name, provider ID, labels and taints.
func (o *Observation) observeNodes(ctx context.Context, r client.Reader) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		o.note("the nodes", err)
	}
	for _, n := range nodes.Items {
		o.Nodes = append(o.Nodes, Node{Name: n.Name, ProviderID: n.Spec.ProviderID, Labels: n.Labels,
			Taints: n.Spec.Taints})
	}
}

// observeAPIServer reads the kubernetes Service, its EndpointSlices and the
// cluster DNS Service.
func (o *Observation) observeAPIServer(ctx context.Context, r client.Reader) {
	var svc corev1.Service
	if err := r.Get(ctx, client.ObjectKey{Namespace: apiServiceNamespace, Name: apiServiceName}, &svc); err != nil {
		o.note("the default/kubernetes Service", err)
	} else {
		o.APIServiceIPs = serviceIPs(svc)
	}
	var eps discoveryv1.EndpointSliceList
	if err := r.List(ctx, &eps, client.InNamespace(apiServiceNamespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: apiServiceName}); err != nil {
		o.note("the default/kubernetes EndpointSlice", err)
	}
	for _, s := range eps.Items {
		if s.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}
		for _, ep := range s.Endpoints {
			o.APIEndpoints = append(o.APIEndpoints, ep.Addresses...)
		}
	}
	o.APIEndpoints = sortedUnique(o.APIEndpoints)

	var dns corev1.Service
	switch err := r.Get(ctx, client.ObjectKey{Namespace: systemNamespace, Name: clusterDNSService}, &dns); {
	case apierrors.IsNotFound(err):
	case err != nil:
		o.note("the kube-system/kube-dns Service", err)
	default:
		if ip := dns.Spec.ClusterIP; ip != "" && ip != corev1.ClusterIPNone {
			o.ClusterDNS = ip
		}
	}
}

// observeEdges lists the IngressClasses and GatewayClasses: the edges the
// cluster already runs.
func (o *Observation) observeEdges(ctx context.Context, r client.Reader) {
	var classes networkingv1.IngressClassList
	if err := r.List(ctx, &classes); err != nil {
		o.note("the IngressClasses", err)
	}
	for _, c := range classes.Items {
		o.IngressClasses = append(o.IngressClasses, IngressClass{Name: c.Name, Controller: c.Spec.Controller,
			Default: c.Annotations[IngressClassDefault] == "true"})
	}
	slices.SortFunc(o.IngressClasses, func(a, b IngressClass) int { return strings.Compare(a.Name, b.Name) })

	gcs := &unstructured.UnstructuredList{}
	gcs.SetGroupVersionKind(gatewayClassGVK)
	switch err := r.List(ctx, gcs); {
	case meta.IsNoMatchError(err), apierrors.IsNotFound(err):
	case err != nil:
		o.note("the GatewayClasses", err)
	default:
		for _, g := range gcs.Items {
			o.GatewayClasses = append(o.GatewayClasses, g.GetName())
		}
		slices.Sort(o.GatewayClasses)
	}
}

// observeSystemPods keeps kube-system's container images, nothing else of
// its Pods.
func (o *Observation) observeSystemPods(ctx context.Context, r client.Reader) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(systemNamespace)); err != nil {
		o.note("the kube-system Pods", err)
	}
	for _, p := range pods.Items {
		for _, c := range slices.Concat(p.Spec.InitContainers, p.Spec.Containers) {
			o.Images = append(o.Images, c.Image)
			if strings.Contains(imageName(c.Image), policyAgentImage) {
				o.PolicyAgentArgs = append(o.PolicyAgentArgs, slices.Concat(c.Command, c.Args))
			}
		}
	}
	o.Images = sortedUnique(o.Images)
}

// observeVPCCNI reads the VPC CNI's network policy controller switch from
// its ConfigMap, that one key only. A cluster without the VPC CNI has no
// such ConfigMap, which is no note.
func (o *Observation) observeVPCCNI(ctx context.Context, r client.Reader) {
	var cm corev1.ConfigMap
	switch err := r.Get(ctx, client.ObjectKey{Namespace: systemNamespace, Name: vpcCNIConfigMap}, &cm); {
	case apierrors.IsNotFound(err):
	case err != nil:
		o.note("the kube-system/amazon-vpc-cni ConfigMap", err)
	default:
		o.VPCCNIPolicyController = strings.TrimSpace(cm.Data[KeyVPCCNIPolicyController])
	}
}

// crdPresence reads one CRD by name, metadata only.
func (o *Observation) crdPresence(ctx context.Context, r client.Reader, name string) Presence {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGVK)
	switch err := r.Get(ctx, client.ObjectKey{Name: name}, crd); {
	case err == nil:
		return Present
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return Absent
	default:
		o.note("the CustomResourceDefinition "+name, err)
		return Unknown
	}
}

// serviceIPs are a Service's cluster IPs, each once.
func serviceIPs(svc corev1.Service) []string {
	ips := slices.Clone(svc.Spec.ClusterIPs)
	if svc.Spec.ClusterIP != "" {
		ips = append(ips, svc.Spec.ClusterIP)
	}
	out := ips[:0]
	for _, ip := range ips {
		if ip != corev1.ClusterIPNone {
			out = append(out, ip)
		}
	}
	return sortedUnique(out)
}

func sortedUnique(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
