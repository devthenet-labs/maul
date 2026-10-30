// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
)

// Sweeper runs even when there are no Preview events, so orphaned rendered
// objects left by a crash, old controller, or removed CR cannot keep a slot
// or ALB target alive indefinitely. It is leader-elected like Reconciler.
type Sweeper struct {
	client.Client
	Settings Settings
	Log      *slog.Logger
	// Events records Warning Events on the Preview owning an unauthenticated
	// Ingress (nil records none).
	Events events.EventRecorder
	// Now is the clock the unauthenticated-Ingress grace period runs on
	// (nil is time.Now).
	Now func() time.Time

	// unauthSince is when each unauthenticated slot Ingress (by UID) was
	// first seen so. It is in memory on purpose: a new leader starts the
	// grace period afresh, which only ever delays a deletion.
	unauthSince map[types.UID]time.Time
}

// unauthenticatedGrace is how many poll intervals a slot Ingress may stay
// without the pinned sign-in set before the sweeper deletes it: long enough
// for the reconciler, polling at the same interval, to patch a live
// Preview's Ingress at a rollout or a key rotation first.
const unauthenticatedGrace = 3

// placeholderName is the chart's Helm-owned placeholder Ingress in slot 0,
// which the sweeper reports but never deletes.
const placeholderName = "maul-preview-placeholder"

// previewIngressClass is the IngressClass of every slot Ingress.
const previewIngressClass = "alb-preview"

func (*Sweeper) NeedLeaderElection() bool { return true }

func (s *Sweeper) Start(ctx context.Context) error {
	ticker := time.NewTicker(s.Settings.PollInterval)
	defer ticker.Stop()
	for {
		if err := s.SweepOnce(ctx); err != nil && s.Log != nil {
			s.Log.LogAttrs(ctx, slog.LevelWarn, "preview orphan sweep failed", slog.Any("error", err))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Sweeper) SweepOnce(ctx context.Context) error {
	var previews v1alpha1.PreviewList
	if err := s.List(ctx, &previews, client.InNamespace(s.Settings.Namespace)); err != nil {
		return err
	}
	live := make(map[string]int32, len(previews.Items))
	owners := make(map[string]*v1alpha1.Preview, len(previews.Items))
	for i, p := range previews.Items {
		if p.Status.Slot != nil {
			live[string(p.UID)] = *p.Status.Slot
			owners[string(p.UID)] = &previews.Items[i]
		}
	}
	for i := range s.Settings.SlotCount {
		ns := s.Settings.slotName(int32(i))
		if err := s.sweepSlot(ctx, ns, int32(i), live); err != nil {
			return err
		}
	}
	if !s.Settings.Auth.Required {
		return nil
	}
	seen := make(map[types.UID]bool)
	defer func() {
		for uid := range s.unauthSince {
			if !seen[uid] {
				delete(s.unauthSince, uid)
			}
		}
	}()
	for i := range s.Settings.SlotCount {
		if err := s.sweepUnauthenticated(ctx, int32(i), owners, seen); err != nil {
			return err
		}
	}
	return nil
}

// sweepUnauthenticated is the backstop behind the slot admission policy,
// which never re-checks an Ingress admitted before it required sign-in. It
// lists every Ingress in the slot, labelled or not, and reports each of the
// preview class that carries neither key generation's pinned set: the
// unauthenticated gauge, a log line and a Warning Event on the Preview owning
// it. It deletes one only once it has stayed so for unauthenticatedGrace poll
// intervals, which the owning reconcile, patching at the same interval, never
// lets a live Preview's Ingress reach unless its patch keeps being refused;
// and never the Helm-owned placeholder, nor one merely on the previous
// generation, which conforms.
func (s *Sweeper) sweepUnauthenticated(ctx context.Context, slot int32, owners map[string]*v1alpha1.Preview,
	seen map[types.UID]bool) error {
	ns := s.Settings.slotName(slot)
	var ingresses networkingv1.IngressList
	if err := s.List(ctx, &ingresses, client.InNamespace(ns)); err != nil {
		return err
	}
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	if s.unauthSince == nil {
		s.unauthSince = make(map[types.UID]time.Time)
	}
	unauthenticated := 0
	for i := range ingresses.Items {
		ing := &ingresses.Items[i]
		if !previewClass(ing) || !ing.DeletionTimestamp.IsZero() || s.Settings.authConforms(ns, ing.Annotations) {
			continue
		}
		unauthenticated++
		seen[ing.UID] = true
		since, ok := s.unauthSince[ing.UID]
		if !ok {
			since = now
			s.unauthSince[ing.UID] = now
		}
		due := ing.Name != placeholderName &&
			now.Sub(since) >= unauthenticatedGrace*s.Settings.PollInterval
		s.reportUnauthenticated(ctx, ing, owners[ing.Labels[labelPreviewUID]], slot, now.Sub(since), due)
		if !due {
			continue
		}
		if err := s.Delete(ctx, ing, client.Preconditions{UID: &ing.UID}); err != nil &&
			!kerrors.IsNotFound(err) && !kerrors.IsConflict(err) {
			return err
		}
		recordUnauthenticatedDeleted(ctx, slot)
	}
	recordUnauthenticated(ctx, slot, unauthenticated)
	return nil
}

// reportUnauthenticated logs an unauthenticated slot Ingress, and records a
// Warning Event on the Preview owning it when that Preview holds the slot (an
// Event belongs in the release namespace, never a slot).
func (s *Sweeper) reportUnauthenticated(ctx context.Context, ing *networkingv1.Ingress, owner *v1alpha1.Preview,
	slot int32, unauthFor time.Duration, deleting bool) {
	if s.Log != nil {
		s.Log.LogAttrs(ctx, slog.LevelWarn, "unauthenticated preview Ingress",
			slog.String("namespace", ing.Namespace), slog.String("name", ing.Name),
			slog.Duration("for", unauthFor), slog.Bool("deleting", deleting))
	}
	if owner == nil || owner.Status.Slot == nil || *owner.Status.Slot != slot || s.Events == nil {
		return
	}
	action, reason := "ReportIngress", "UnauthenticatedIngress"
	if deleting {
		action, reason = "DeleteIngress", "UnauthenticatedIngressDeleted"
	}
	s.Events.Eventf(owner, nil, corev1.EventTypeWarning, reason, action,
		"Ingress %s/%s lacks the pinned sign-in annotations (for %s; deleted after %d poll intervals)",
		ing.Namespace, ing.Name, unauthFor.Truncate(time.Second), unauthenticatedGrace)
}

// previewClass reports whether ing is of the preview load balancer's class,
// by its class name or the legacy class annotation.
func previewClass(ing *networkingv1.Ingress) bool {
	if ing.Spec.IngressClassName != nil {
		return *ing.Spec.IngressClassName == previewIngressClass
	}
	return ing.Annotations["kubernetes.io/ingress.class"] == previewIngressClass
}

func (s *Sweeper) sweepSlot(ctx context.Context, namespace string, slot int32, live map[string]int32) error {
	selector := client.MatchingLabels{labelManagedBy: managedBy}
	var ingresses networkingv1.IngressList
	if err := s.List(ctx, &ingresses, client.InNamespace(namespace), selector); err != nil {
		return err
	}
	ingressLive := live
	if s.Settings.NoLegacyIngress {
		// Without the legacy Ingress no Preview renders one, so every
		// managed slot Ingress is left over, a live Preview's too.
		ingressLive = nil
	}
	for i := range ingresses.Items {
		if err := s.deleteOrphan(ctx, &ingresses.Items[i], slot, ingressLive); err != nil {
			return err
		}
	}
	var services corev1.ServiceList
	if err := s.List(ctx, &services, client.InNamespace(namespace), selector); err != nil {
		return err
	}
	for i := range services.Items {
		if err := s.deleteOrphan(ctx, &services.Items[i], slot, live); err != nil {
			return err
		}
	}
	var deployments appsv1.DeploymentList
	if err := s.List(ctx, &deployments, client.InNamespace(namespace), selector); err != nil {
		return err
	}
	for i := range deployments.Items {
		if err := s.deleteOrphan(ctx, &deployments.Items[i], slot, live); err != nil {
			return err
		}
	}
	if !s.Settings.verifying() {
		return nil
	}
	return s.sweepVerification(ctx, namespace, slot, live)
}

// sweepVerification removes what verification left in a slot: the
// NetworkPolicies and Jobs of a Preview no longer holding the slot, a
// tester or tool Job whose PreviewVerification is gone, and such a Job's Pod
// whose Job is gone (one deleted without background propagation); a tool
// Job's key Secret and ConfigMap are the Job's, and go with it. Pods are listed before
// Jobs, so a Pod seen here has a Job the Job list sees too, whichever was
// created last; a component's Pod is never the sweep's.
func (s *Sweeper) sweepVerification(ctx context.Context, namespace string, slot int32, live map[string]int32) error {
	selector := client.MatchingLabels{labelManagedBy: managedBy}
	var policies networkingv1.NetworkPolicyList
	if err := s.List(ctx, &policies, client.InNamespace(namespace), selector); err != nil {
		return err
	}
	for i := range policies.Items {
		if err := s.deleteOrphan(ctx, &policies.Items[i], slot, live); err != nil {
			return err
		}
	}
	var pods corev1.PodList
	if err := s.List(ctx, &pods, client.InNamespace(namespace), selector, jobPodRoles()); err != nil {
		return err
	}
	var jobs batchv1.JobList
	if err := s.List(ctx, &jobs, client.InNamespace(namespace), selector); err != nil {
		return err
	}
	verifications, err := s.liveVerifications(ctx, jobs.Items)
	if err != nil {
		return err
	}
	kept := make(map[types.UID]bool, len(jobs.Items))
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !renderedName(job) {
			continue
		}
		held, ok := live[job.Labels[labelPreviewUID]]
		if ok && held == slot && verifications[job.Labels[labelVerificationUID]] {
			kept[job.UID] = true
			continue
		}
		s.logOrphan(ctx, objectKind(job), job)
		if err := deleteJob(ctx, s.Client, job); err != nil {
			return err
		}
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		if owner != nil && owner.Kind == "Job" && kept[owner.UID] {
			continue
		}
		if !testerPodName(pod) {
			continue
		}
		s.logOrphan(ctx, "Pod", pod)
		if err := s.Delete(ctx, pod); err != nil && !kerrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// jobPodRoles selects the Pods of the Jobs a verification renders: the
// tester's and the tool pod's roles.
func jobPodRoles() client.ListOption {
	roles, err := labels.NewRequirement(labelRole, selection.In,
		[]string{string(v1alpha1.PreviewRoleTester), v1alpha1.PreviewRoleTools})
	if err != nil {
		// Constant, valid values: never. Fail closed: no Pod is swept.
		return client.MatchingLabelsSelector{Selector: labels.Nothing()}
	}
	return client.MatchingLabelsSelector{Selector: labels.NewSelector().Add(*roles)}
}

// liveVerifications are the UIDs of the PreviewVerifications in the release
// namespace, read only when there is a Job to judge.
func (s *Sweeper) liveVerifications(ctx context.Context, jobs []batchv1.Job) (map[string]bool, error) {
	if len(jobs) == 0 {
		return nil, nil
	}
	var list v1alpha1.PreviewVerificationList
	if err := s.List(ctx, &list, client.InNamespace(s.Settings.Namespace)); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list.Items))
	for _, pv := range list.Items {
		if pv.DeletionTimestamp.IsZero() {
			out[string(pv.UID)] = true
		}
	}
	return out, nil
}

func (s *Sweeper) deleteOrphan(ctx context.Context, obj client.Object, slot int32, live map[string]int32) error {
	if !renderedName(obj) {
		return nil // do not adopt or delete an arbitrary object with a copied managed-by label
	}
	l := obj.GetLabels()
	if held, ok := live[l[labelPreviewUID]]; ok && held == slot {
		return nil
	}
	s.logOrphan(ctx, objectKind(obj), obj)
	err := s.Delete(ctx, obj)
	if kerrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (s *Sweeper) logOrphan(ctx context.Context, kind string, obj client.Object) {
	if s.Log != nil {
		s.Log.LogAttrs(ctx, slog.LevelInfo, "deleting orphan preview object",
			slog.String("kind", kind), slog.String("namespace", obj.GetNamespace()),
			slog.String("name", obj.GetName()))
	}
}

// renderedName reports whether obj's name is one the controller renders for
// the Preview its labels name: preview-<p> (the Ingress, and the first or
// only component's Deployment and Service), or preview-<p>-<c> for a further
// component whose component label is c; for a NetworkPolicy
// preview-<p>-np-<part>, and for a Job preview-<p>-vt-<10 hex>.
func renderedName(obj client.Object) bool {
	l := obj.GetLabels()
	p, uid, c := l[labelPreview], l[labelPreviewUID], l[labelComponent]
	name := obj.GetName()
	if p == "" || uid == "" || !strings.HasPrefix(name, "preview-") {
		return false
	}
	switch obj.(type) {
	case *networkingv1.NetworkPolicy:
		part, ok := strings.CutPrefix(name, "preview-"+p+policyInfix)
		return ok && namePattern.MatchString(part)
	case *batchv1.Job:
		rest, ok := strings.CutPrefix(name, "preview-"+p)
		return ok && testerJobPattern.MatchString(rest)
	}
	return name == "preview-"+p || (c != "" && name == "preview-"+p+"-"+c)
}

// testerJobPattern is a tester Job's name past preview-<p>.
var testerJobPattern = regexp.MustCompile(`^` + testerInfix + `[0-9a-f]{10}$`)

// testerPodName reports whether pod is named as the Job controller names a
// Pod of a tester Job its labels' Preview renders: preview-<p>-vt-<10 hex>-
// and a suffix.
func testerPodName(pod *corev1.Pod) bool {
	p, uid := pod.Labels[labelPreview], pod.Labels[labelPreviewUID]
	rest, ok := strings.CutPrefix(pod.Name, "preview-"+p+testerInfix)
	return p != "" && uid != "" && ok && len(rest) > 11 && testerJobPattern.MatchString(testerInfix+rest[:10]) &&
		rest[10] == '-'
}

func objectKind(obj client.Object) string {
	switch obj.(type) {
	case *appsv1.Deployment:
		return "Deployment"
	case *corev1.Service:
		return "Service"
	case *networkingv1.Ingress:
		return "Ingress"
	case *networkingv1.NetworkPolicy:
		return "NetworkPolicy"
	case *batchv1.Job:
		return "Job"
	default:
		return "unknown"
	}
}
