// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// PreviewPhase is the local state of a disposable runtime preview. It never
// participates in Finding or Intent phase transitions.
// +kubebuilder:validation:Enum=Pending;Queued;Deploying;Ready;Failed;Expired
type PreviewPhase string

// Preview phases.
const (
	PreviewPending   PreviewPhase = "Pending"
	PreviewQueued    PreviewPhase = "Queued"
	PreviewDeploying PreviewPhase = "Deploying"
	PreviewReady     PreviewPhase = "Ready"
	PreviewFailed    PreviewPhase = "Failed"
	PreviewExpired   PreviewPhase = "Expired"
)

// PreviewComponent is the fixed application shape copied from an operator's
// Project, with only Revision taken from the Intent's recorded state: the PR
// head of its repository, or the default-branch head recorded once for a
// previewed repository the intent did not change. DesiredPreviewComponents
// derives the whole list; the writer and the preview-controller both use it.
type PreviewComponent struct {
	// Name identifies the component and its rendered resources: the Project
	// repository's key.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=16
	Name string `json:"name"`
	// ImageRepository is the operator-configured repository, without a tag:
	// ProjectPreview's shape. The preview-controller and the slot admission
	// policy hold it to the configured image prefix.
	// +kubebuilder:validation:Pattern=`^[a-z0-9][a-z0-9.:-]*(/[a-z0-9]+([._-][a-z0-9]+)*)+/[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=255
	ImageRepository string `json:"imageRepository"`
	// Revision is a full commit SHA, never a branch name: the PR head, or a
	// recorded default-branch head (Intent status.previewBases).
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Revision string `json:"revision"`
	// Port and ReadinessPath are operator configuration, not agent input.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
	// +kubebuilder:validation:Pattern=`^/[a-zA-Z0-9/_-]*$`
	// +kubebuilder:validation:MaxLength=128
	ReadinessPath string `json:"readinessPath"`
	// Path is the URL path prefix the component is served under on the
	// preview host. Omitted means "/", the form every single-repository
	// Preview has always been written in: it is deliberately not a schema
	// default, so a writer comparing specs never finds a defaulted "/" it
	// did not write and rewrites a live Preview. PreviewComponentPath reads
	// it.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^/([a-z0-9-]+(/[a-z0-9-]+)*)?$`
	Path string `json:"path,omitempty"`
}

// PreviewComponentPath is the path a component is served under: its Path, or
// "/" when omitted.
func PreviewComponentPath(c PreviewComponent) string {
	if c.Path == "" {
		return "/"
	}
	return c.Path
}

// PreviewSpec is authored by intent-controller in the release namespace.
// A PR head change updates Revision; an operator's Project change may also
// update the component runtime configuration.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.project) || (has(self.project) && self.project == oldSelf.project)",message="spec.project is set once and is then immutable"
// +kubebuilder:validation:XValidation:rule="has(self.environment) == has(oldSelf.environment)",message="spec.environment is set at creation only: a changed environment is a new Preview"
// +kubebuilder:validation:XValidation:rule="(has(self.resetGeneration) ? self.resetGeneration : 0) >= (has(oldSelf.resetGeneration) ? oldSelf.resetGeneration : 0)",message="spec.resetGeneration never decreases"
type PreviewSpec struct {
	// IntentRef pins the exact Intent; a recycled name cannot adopt a preview.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="intentRef is immutable"
	IntentRef ObjectReference `json:"intentRef"`
	// Project is the pinned Intent's spec.project, copied by the writer so a
	// reader that authorises a viewer per Project (the preview sign-in relay)
	// needs only Previews, never Intents. It is set once and never changed:
	// written at create, or once onto a Preview written before the field
	// existed, and then immutable, as the Intent's own spec.project is.
	// preview-controller renders nothing whose Project is not its Intent's.
	// An empty value names no Project, and a reader that needs one fails
	// closed on it.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=25
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`
	Project string `json:"project,omitempty"`
	// HostLabel is the single DNS label before the operator's host suffix.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="hostLabel is immutable"
	HostLabel string `json:"hostLabel"`
	// Components are the runtime components the one preview host routes
	// to, one per previewed Project repository, in Project order, each
	// under its own path (at most MaxPreviewComponents). A one-repository
	// Project has exactly one, at "/".
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:XValidation:rule="self.all(c, self.exists_one(d, (has(d.path) ? d.path : '/') == (has(c.path) ? c.path : '/')))",message="two components have the same path (an omitted path is /)"
	Components []PreviewComponent `json:"components"`
	// TTL is at most 72 hours since the last successful deployment. It is
	// also a cap on queued/deploying previews, so they cannot hold a slot
	// forever if the image never becomes available.
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('1h') && duration(self) <= duration('72h')",message="ttl must be between 1h and 72h"
	TTL metav1.Duration `json:"ttl"`
	// Environment makes the Preview verification-capable: the parts of its
	// slot beside the components and the edges between them that the
	// slot's NetworkPolicies admit (DesiredPreviewEnvironment). It is set
	// when the Preview is created and never changed, added or removed
	// (CEL-enforced): a Project change that alters it is a new Preview. A
	// Preview without it renders exactly as before.
	// +optional
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec.environment is immutable"
	Environment *PreviewEnvironment `json:"environment,omitempty"`
	// ResetGeneration is bumped by an approver's /maul verify --reset: the
	// slot's parts restart from scratch. It never decreases (CEL-enforced).
	// +optional
	// +kubebuilder:validation:Minimum=0
	ResetGeneration int64 `json:"resetGeneration,omitempty"`
}

// PreviewPartTester is the part name of the verification tester in a
// Preview's slot: the part every slice-1 edge starts at, and that no edge
// may end at.
const PreviewPartTester = "tester"

// PreviewEnvironment is the operator-derived shape of a verification-capable
// Preview's slot beyond its components.
type PreviewEnvironment struct {
	// Verification marks a Preview whose acceptance criteria are verified
	// in its slot.
	Verification bool `json:"verification"`
	// Edges are the connections between the slot's parts that the
	// preview-controller's NetworkPolicies admit, beside the isolation
	// policy's DNS and gateway ingress. Nothing else in the slot connects.
	// No edge ends at the tester, and no two edges are the same. There is
	// at least one: the tester's to a component.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	// +kubebuilder:validation:XValidation:rule="self.all(e, e.to != 'tester')",message="nothing may connect to the tester"
	// +kubebuilder:validation:XValidation:rule="self.all(e, self.exists_one(f, f.from == e.from && f.to == e.to && f.port == e.port))",message="edges are distinct"
	Edges []PreviewEdge `json:"edges"`
}

// PreviewEdge admits TCP connections from one part of a Preview's slot to
// another, on one numeric port.
// +kubebuilder:validation:XValidation:rule="self.from != self.to",message="an edge connects two different parts"
type PreviewEdge struct {
	// From is the calling part: a component's key, or tester.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=20
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	From string `json:"from"`
	// To is the called part.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=20
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	To string `json:"to"`
	// Port is the called part's container port: numeric, since a named
	// port's handling varies between network policy implementations.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// PreviewComponentStatus records the image the kubelet actually ran.
type PreviewComponentStatus struct {
	Name string `json:"name"`
	// Revision is the commit the component serves: the spec revision its
	// Ready Pod runs.
	// +optional
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Revision string `json:"revision,omitempty"`
	// ImageID is the runtime's digest-bearing image ID once a Pod is Ready.
	// +optional
	ImageID string `json:"imageID,omitempty"`
}

// PreviewServingComponent is one component of what a Preview serves: where
// the preview gateway sends a request whose path falls under Path.
type PreviewServingComponent struct {
	// Name is the component's name (its Project repository's key).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=16
	Name string `json:"name"`
	// Service is the component's Service in the Preview's slot namespace
	// (PreviewComponentServiceName).
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Service string `json:"service"`
	// Path is the URL path prefix the component is served under, "/"
	// written out (PreviewComponentPath), matched with PreviewPathMatches.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^/([a-z0-9-]+(/[a-z0-9-]+)*)?$`
	Path string `json:"path"`
	// Port is the Service's port (PreviewServicePort), not the container's.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port"`
}

// PreviewServing is the snapshot of what a Preview serves, written when it
// turns Ready. It is kept while the Preview redeploys (Deploying at a new
// generation), because the rolling update keeps the previous revision's Pods
// serving until the new ones are Ready, less any component whose Service the
// new spec no longer renders for it (pruned or handed to another component),
// and it is cleared when nothing may
// serve any more: a retry that deletes the Deployments, Failed, Expired, or
// the slot's release. A router serves a Preview only from this snapshot, with
// the Preview's UID, slot and host label, never from its spec.
type PreviewServing struct {
	// Generation is the Preview generation these components turned Ready at.
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
	// UID is the Preview's UID when the snapshot was written; a router
	// refuses a snapshot whose UID is not the Preview's own.
	UID types.UID `json:"uid"`
	// Components are the served components, in spec order.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=4
	// +listType=atomic
	Components []PreviewServingComponent `json:"components"`
}

// PreviewStatus is written only by preview-controller. Slot is held until
// finalizer cleanup has removed every rendered object from that slot.
type PreviewStatus struct {
	// +optional
	Phase PreviewPhase `json:"phase,omitempty"`
	// ObservedGeneration distinguishes an expired or failed revision from a
	// later PR-head update that may legitimately start a fresh deployment.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	Slot *int32 `json:"slot,omitempty"`
	// +optional
	URL string `json:"url,omitempty"`
	// ObservedRevision is the revision this slot currently serves or tried:
	// the first component's (each component's own is in Components).
	// +optional
	ObservedRevision string `json:"observedRevision,omitempty"`
	// +optional
	// +kubebuilder:validation:Maximum=3
	Retries int32 `json:"retries,omitempty"`
	// +optional
	AttemptStartedAt *metav1.Time `json:"attemptStartedAt,omitempty"`
	// +optional
	LastDeployedAt *metav1.Time `json:"lastDeployedAt,omitempty"`
	// Components records, once the Preview is Ready, what each component
	// serves, in spec order.
	// +optional
	// +kubebuilder:validation:MaxItems=4
	Components []PreviewComponentStatus `json:"components,omitempty"`
	// Serving is what the Preview serves, for a router to route by. Unlike
	// URL and Components, it survives a redeploy (see PreviewServing).
	// +optional
	Serving *PreviewServing `json:"serving,omitempty"`
	// ObservedResetGeneration is the spec.resetGeneration the slot's parts
	// were last restarted for.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ObservedResetGeneration int64 `json:"observedResetGeneration,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// Preview deploys one approved PR head into one of the chart's fixed,
// isolated slots. It owns no namespace or infrastructure resource.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=maul
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Slot",type=integer,JSONPath=`.status.slot`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type Preview struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PreviewSpec   `json:"spec"`
	Status            PreviewStatus `json:"status,omitempty"`
}

// PreviewList is a list of Previews.
// +kubebuilder:object:root=true
type PreviewList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Preview `json:"items"`
}
