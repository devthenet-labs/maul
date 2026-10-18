// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/controller/preview"
)

// CheckPreviewNetpol is the opt-in ingress isolation probe (design §4.4):
// the Go port of hack/preview-isolation-probe/ingress.sh with its
// --refused-is-blocked verdict. It is the one check that writes to the
// cluster: four short-lived probe Pods, every one deleted again.
const CheckPreviewNetpol = "preview-netpol"

// LogReader reads a finished probe Pod's log (its probe container's).
type LogReader func(ctx context.Context, namespace, pod string) ([]byte, error)

// Isolation configures the ingress isolation probe. A nil *Isolation in
// Config is no probe: every other check only reads.
type Isolation struct {
	// Client creates, reads and deletes the probe Pods and reads the slot
	// NetworkPolicy, with the caller's own kubeconfig.
	Client client.Client
	// Logs reads each client Pod's summary.
	Logs LogReader
	// Image is the netprobe image; empty derives it from preview-
	// controller's image prefix as <prefix>netprobe:sha-<Commit>, the tag
	// hack/preview-isolation-probe/README.md builds it under.
	Image  string
	Commit string
	// Slot is the slot the listener runs in.
	Slot int
	// Timeout bounds each wait: the listener starting, and each client
	// finishing. Zero is DefaultIsolationTimeout.
	Timeout time.Duration
	// Poll is how often a Pod is read while waiting; zero is two seconds.
	Poll time.Duration
}

// DefaultIsolationTimeout is ingress.sh's default per-Pod timeout.
const DefaultIsolationTimeout = 5 * time.Minute

// The probe's Pods, named as ingress.sh names them, so the two refuse to
// run over each other.
const (
	probeListener      = "preview-ingress-listener"
	probeAgentClient   = "preview-ingress-agent"
	probeReleaseClient = "preview-ingress-release"
	probeGatewayClient = "preview-ingress-gateway"
	// probePort is the listener's containerPort http, the one port the slot
	// policy admits the gateway on.
	probePort = 8080
	// probeContainer is every probe Pod's one container.
	probeContainer = "probe"
	// slotPolicyName is the chart's slot NetworkPolicy.
	slotPolicyName = "preview-isolation"
	// gatewayLabel is the label the slot policy admits.
	gatewayLabel = "patchy.bitwisemedia.uk/preview-gateway"
	// refusedIsBlocked is netprobe's connect flag that counts a refused
	// connection as blocked.
	refusedIsBlocked = "--refused-is-blocked"
)

// preview-controller's settings and the agents' namespace the probe reads.
const (
	keyPreviewImagePrefix = "PATCHY_PREVIEW_IMAGE_PREFIX"
	keyPreviewSlotCount   = "PATCHY_PREVIEW_SLOT_COUNT"
	keyAgentNamespace     = "PATCHY_AGENT_NAMESPACE"
	defaultAgentNamespace = "patchy-agents"
	defaultSlotCount      = 2
)

// probeImagePattern is the one image shape a slot admits, as ingress.sh
// checks it: <registry>/<path>:sha-<40 hex>.
var probeImagePattern = regexp.MustCompile(
	`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9]+([._-][a-z0-9]+)*)+:sha-[0-9a-f]{40}$`)

// commitPattern is a full commit SHA, the default image's tag.
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// probePlan is where and how the probe runs, decided before any Pod
// exists.
type probePlan struct {
	image                       string
	slotNS, releaseNS, agentsNS string
	// selector and taintKey place the listener on the preview nodes; both
	// empty with node isolation off.
	selector map[string]string
	taintKey string
	// ipBlocks are address ranges the slot policy admits beside the
	// gateway's label (the legacy ALB path's subnets).
	ipBlocks []string
}

// pod is one probe Pod in the slot Pod's shape, which every namespace
// admits: ingress.sh's pod(). Only a Pod in the slot selects and tolerates
// the preview nodes.
func (p probePlan) pod(namespace, name string, args []string, gateway bool) *corev1.Pod {
	labels := map[string]string{"app": name}
	if gateway {
		labels[gatewayLabel] = "true"
	}
	no, yes, user := false, true, int64(65532)
	pod := &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			ServiceAccountName:           "default",
			AutomountServiceAccountToken: &no,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &yes, RunAsUser: &user, RunAsGroup: &user,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name: probeContainer, Image: p.image, ImagePullPolicy: corev1.PullIfNotPresent, Args: args,
				Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: probePort, Protocol: corev1.ProtocolTCP}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"),
						corev1.ResourceMemory: resource.MustParse("32Mi")},
					Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"),
						corev1.ResourceMemory: resource.MustParse("256Mi")},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes,
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}
	if namespace == p.slotNS && len(p.selector) > 0 {
		pod.Spec.NodeSelector = maps.Clone(p.selector)
		pod.Spec.Tolerations = []corev1.Toleration{{Key: p.taintKey, Operator: corev1.TolerationOpEqual,
			Value: "true", Effect: corev1.TaintEffectNoExecute}}
	}
	return pod
}

// listener is the slot's listening Pod.
func (p probePlan) listener() *corev1.Pod {
	return p.pod(p.slotNS, probeListener, []string{"listen"}, false)
}

// client is the client Pod name, connecting to the listener at ip. The
// two that must be refused count a refusal as blocked: the run creates
// them only once the gateway's client has reached the listener.
func (p probePlan) client(name, ip string) *corev1.Pod {
	target := ip + ":" + strconv.Itoa(probePort)
	switch name {
	case probeGatewayClient:
		return p.pod(p.releaseNS, name, []string{"connect", target}, true)
	case probeAgentClient:
		return p.pod(p.agentsNS, name, []string{"connect", refusedIsBlocked, target}, false)
	default:
		return p.pod(p.releaseNS, name, []string{"connect", refusedIsBlocked, target}, false)
	}
}

// probePods are every Pod the run may create, as namespace/name.
func (p probePlan) probePods() []types.NamespacedName {
	return []types.NamespacedName{
		{Namespace: p.slotNS, Name: probeListener},
		{Namespace: p.releaseNS, Name: probeGatewayClient},
		{Namespace: p.agentsNS, Name: probeAgentClient},
		{Namespace: p.releaseNS, Name: probeReleaseClient},
	}
}

// planIsolation decides the probe from the settings and the cluster,
// reading only: the image, the slot and its policy, the agents' namespace,
// the listener's placement, and that no probe Pod exists already. An error
// is why the probe cannot run.
func (c *Checker) planIsolation(ctx context.Context, s Settings, iso *Isolation) (probePlan, error) {
	switch {
	case s.Preview.Err != nil:
		return probePlan{}, fmt.Errorf("cannot read the controllers' settings: %w", s.Preview.Err)
	case !s.Preview.Found():
		return probePlan{}, fmt.Errorf("previews are off: there is no preview-controller ConfigMap in namespace %s",
			c.cfg.Namespace)
	}
	p := probePlan{releaseNS: c.cfg.Namespace, agentsNS: agentNamespace(s)}
	var err error
	if p.image, err = probeImage(s, iso); err != nil {
		return probePlan{}, err
	}
	if p.slotNS, err = probeSlot(s, iso.Slot); err != nil {
		return probePlan{}, err
	}
	for _, ns := range []string{p.releaseNS, p.agentsNS} {
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return probePlan{}, fmt.Errorf("namespace %q is not a DNS label: %s", ns, strings.Join(errs, "; "))
		}
	}
	if p.selector, p.taintKey, err = probePlacement(s); err != nil {
		return probePlan{}, err
	}
	if p.ipBlocks, err = slotPolicy(ctx, iso.Client, p.slotNS, p.releaseNS, s.legacyIngress()); err != nil {
		return probePlan{}, err
	}
	for _, key := range p.probePods() {
		var pod corev1.Pod
		switch err := iso.Client.Get(ctx, key, &pod); {
		case err == nil:
			return probePlan{}, fmt.Errorf("the Pod %s already exists: refusing to overwrite another probe "+
				"(delete it once no probe runs)", key)
		case !apierrors.IsNotFound(err):
			return probePlan{}, fmt.Errorf("cannot read Pod %s: %w", key, err)
		}
	}
	return p, nil
}

// probeImage is the netprobe image: the flag's, else the prefix's tag at
// the CLI's commit; either must be the one shape a slot admits, under the
// prefix.
func probeImage(s Settings, iso *Isolation) (string, error) {
	prefix := strings.TrimSpace(s.Preview.Data[keyPreviewImagePrefix])
	image := iso.Image
	if image == "" {
		if prefix == "" || !commitPattern.MatchString(iso.Commit) {
			return "", fmt.Errorf("no --isolation-image, and no default: the default is "+
				"<preview image prefix>netprobe:sha-<this CLI's commit>, and %s", missingDefault(s, prefix, iso.Commit))
		}
		image = prefix + "netprobe:sha-" + iso.Commit
	}
	if !probeImagePattern.MatchString(image) {
		return "", fmt.Errorf("the probe image %s is not <registry>/<path>:sha-<40 hex>, the one shape a slot admits",
			image)
	}
	if prefix != "" && !strings.HasPrefix(image, prefix) {
		return "", fmt.Errorf("the probe image %s is not under preview-controller's image prefix %s, so the slot "+
			"admission policy would refuse the listener", image, prefix)
	}
	return image, nil
}

// probeSlot is the slot namespace the listener runs in, one of the
// install's slots.
func probeSlot(s Settings, slot int) (string, error) {
	slots := defaultSlotCount
	if v := strings.TrimSpace(s.Preview.Data[keyPreviewSlotCount]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return "", fmt.Errorf("%s sets %s to %q, not a slot count", s.Preview.From, keyPreviewSlotCount, v)
		}
		slots = n
	}
	if slot < 0 || slot >= slots {
		return "", fmt.Errorf("slot %d is not one of the install's %d slots (0 to %d)", slot, slots, slots-1)
	}
	return v1alpha1.PreviewSlotNamespace(int32(slot)), nil //nolint:gosec // bounded by the slot count above
}

// probePlacement is the listener's node selector and taint key, as every
// slot Pod's; both empty with node isolation off.
func probePlacement(s Settings) (map[string]string, string, error) {
	switch mode := s.Preview.Data[keyPreviewNodeIsolation]; mode {
	case preview.NodeIsolationNone:
		return nil, "", nil
	case "", preview.NodeIsolationRequired:
		selector, err := nodeSelector(s.Preview)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", s.Preview.From, err)
		}
		taint := strings.TrimSpace(s.Preview.Data[keyPreviewTaintKey])
		if len(selector) == 0 || taint == "" {
			return nil, "", fmt.Errorf("%s requires node isolation but sets no node selector or taint key, so the "+
				"listener cannot be placed on the preview nodes", s.Preview.From)
		}
		return selector, taint, nil
	default:
		return nil, "", fmt.Errorf("%s sets %s to %q, which preview-controller refuses", s.Preview.From,
			keyPreviewNodeIsolation, mode)
	}
}

// missingDefault says which half of the default image is missing.
func missingDefault(s Settings, prefix, commit string) string {
	if prefix == "" {
		return fmt.Sprintf("%s sets no %s (preview.imageRegistry and preview.imagePathPrefix)", s.Preview.From,
			keyPreviewImagePrefix)
	}
	return fmt.Sprintf("this CLI was built without a commit (%q): pass --isolation-image "+
		"%snetprobe:sha-<40 hex>, built as hack/preview-isolation-probe/README.md says", commit, prefix)
}

// agentNamespace is the agents' namespace: any patchy ConfigMap's
// PATCHY_AGENT_NAMESPACE (the chart's agent.namespace), else the default.
func agentNamespace(s Settings) string {
	for _, cfg := range []ControllerConfig{s.Intent, s.Source, s.Preview} {
		if v := strings.TrimSpace(cfg.Data[keyAgentNamespace]); v != "" {
			return v
		}
	}
	for _, cm := range s.ConfigMaps {
		if v := strings.TrimSpace(cm.Data[keyAgentNamespace]); v != "" {
			return v
		}
	}
	return defaultAgentNamespace
}
