// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package usagerollup

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/stats"
)

// Finalizers is the finalizer list a controller creates a run with: base,
// and v1alpha1.FinalizerUsageRollup when the run is attributed (its
// spec.attributedTo is set, which only --attribution-mode attribute does).
func Finalizers(attributedTo *v1alpha1.Attribution, base ...string) []string {
	if attributedTo == nil {
		return base
	}
	return append(base, v1alpha1.FinalizerUsageRollup)
}

// Owed reports whether a run still owes its usage: it carries the usage
// finalizer and is not yet marked rolled up. A run without the finalizer
// (one from before the feature) is not applicable and never owes.
func Owed(obj client.Object, conditions []metav1.Condition) bool {
	return slices.Contains(obj.GetFinalizers(), v1alpha1.FinalizerUsageRollup) &&
		!meta.IsStatusConditionTrue(conditions, v1alpha1.ConditionUsageRolledUp)
}

// Run is what aggregation needs of one run.
type Run struct {
	// Kind is one of the stats.UsageKind constants.
	Kind string
	// UID keys the ledger (u:<uid>).
	UID types.UID
	// Attribution is the run's spec.attributedTo.
	Attribution *v1alpha1.Attribution
	// Settled is a terminal phase (Complete or Failed).
	Settled bool
	// Usage is the run's usage; nil when the run never reported a stage.
	Usage *v1alpha1.UsageSummary
	// CompletedAt buckets the run: its finish time, else its creation.
	CompletedAt time.Time
	// Intent and Project name an intent run's Intent and Project; empty
	// otherwise (or Project when the Intent is gone).
	Intent, Project string
}

// Due reports whether run can be aggregated now: when it is being deleted,
// at once (nothing will settle it any more); otherwise once it is settled
// and its usage final. With measuring off, or without a usage at all,
// nothing will ever mark it final, so settled is enough.
func Due(run Run, deleting, measuring bool) bool {
	if deleting {
		return true
	}
	if !run.Settled {
		return false
	}
	return !measuring || run.Usage == nil || run.Usage.Final
}

// Kind adapts one run kind to Settle.
type Kind[T client.Object] struct {
	// New allocates an empty run object.
	New func() T
	// Run describes the run; it may read (the Intent, for its Project).
	Run func(ctx context.Context, obj T) (Run, error)
	// Conditions are the run's status conditions.
	Conditions func(obj T) *[]metav1.Condition
}

// Aggregator folds runs into their UsageRollups.
type Aggregator struct {
	// Client writes UsageRollups and the runs' condition and finalizer.
	Client client.Client
	// Reader reads runs and UsageRollups uncached (the manager's API
	// reader), so no informer on UsageRollups is ever needed.
	Reader client.Reader
	// Namespace the UsageRollups live in: the release namespace.
	Namespace string
	// Measuring is whether the run controller's usage meter is on
	// (--usage-measurement): a settled run then waits for its usage to be
	// final.
	Measuring bool
	// Now is the clock seam; nil means time.Now.
	Now func() time.Time
	// Log receives diagnostics; nil discards.
	Log *slog.Logger
}

// Settle aggregates the run at key when it is owed and due, marks it, and
// releases its usage finalizer. It is safe to call on every event: a run
// that owes nothing costs one read, and a crash anywhere replays to the same
// result.
func Settle[T client.Object](ctx context.Context, a *Aggregator, key client.ObjectKey, k Kind[T]) error {
	if a == nil {
		return nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj := k.New()
		if err := a.Reader.Get(ctx, key, obj); err != nil {
			return client.IgnoreNotFound(err)
		}
		conds := k.Conditions(obj)
		if !Owed(obj, *conds) {
			return nil
		}
		run, err := k.Run(ctx, obj)
		if err != nil {
			return err
		}
		if !Due(run, !obj.GetDeletionTimestamp().IsZero(), a.Measuring) {
			return nil
		}
		scopes, err := a.Aggregate(ctx, run)
		if err != nil {
			return err
		}
		meta.SetStatusCondition(conds, metav1.Condition{
			Type:               v1alpha1.ConditionUsageRolledUp,
			Status:             metav1.ConditionTrue,
			Reason:             "Aggregated",
			Message:            fmt.Sprintf("usage folded into %d usage rollups", scopes),
			ObservedGeneration: obj.GetGeneration(),
		})
		return a.Client.Status().Update(ctx, obj)
	})
	if err != nil {
		return err
	}
	return release(ctx, a, key, k)
}

// release removes the usage finalizer from a run marked rolled up: once its
// usage is in every scope, nothing about the run is owed any more, so its
// deletion (by TTL or by hand) need not wait for this controller.
func release[T client.Object](ctx context.Context, a *Aggregator, key client.ObjectKey, k Kind[T]) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj := k.New()
		if err := a.Reader.Get(ctx, key, obj); err != nil {
			return client.IgnoreNotFound(err)
		}
		fins := obj.GetFinalizers()
		if !slices.Contains(fins, v1alpha1.FinalizerUsageRollup) ||
			!meta.IsStatusConditionTrue(*k.Conditions(obj), v1alpha1.ConditionUsageRolledUp) {
			return nil
		}
		obj.SetFinalizers(slices.DeleteFunc(slices.Clone(fins), func(f string) bool {
			return f == v1alpha1.FinalizerUsageRollup
		}))
		return client.IgnoreNotFound(a.Client.Update(ctx, obj))
	})
}

// Aggregate folds run into every scope it contributes to, returning how
// many. Each scope's ledger makes it exactly-once; the metrics fire when
// the user scope first takes the run.
func (a *Aggregator) Aggregate(ctx context.Context, run Run) (int, error) {
	d, err := stats.UsageDeltaFrom(run.Kind, run.Usage, run.CompletedAt)
	if err != nil {
		a.log().LogAttrs(ctx, slog.LevelWarn, "unparseable run cost; counted as zero",
			slog.String("run", string(run.UID)), slog.Any("error", err))
	}
	d.Intent, d.Project = run.Intent, run.Project
	user := stats.UsageUser(run.Attribution)
	login := ""
	if run.Attribution != nil && run.Attribution.User != nil {
		login = run.Attribution.User.Login
	}
	ledger := "u:" + string(run.UID)
	scopes := stats.UsageScopes(user, run.Project)
	for _, scope := range scopes {
		applied, err := a.apply(ctx, scope, ledger, d, login)
		if err != nil {
			return 0, err
		}
		if applied && scope.Type == v1alpha1.UsageScopeUser {
			stats.RecordUsage(ctx, user, d)
		}
	}
	return len(scopes), nil
}

// apply folds d into one scope's rollup under conflict retry, creating the
// object on demand; it reports whether this call applied it.
func (a *Aggregator) apply(ctx context.Context, scope v1alpha1.UsageScope, ledger string, d stats.UsageDelta,
	login string) (bool, error) {
	if scope.Type == v1alpha1.UsageScopeProject {
		login = "" // a project scope has no person
	}
	name := stats.UsageObjectName(scope)
	key := types.NamespacedName{Namespace: a.Namespace, Name: name}
	applied := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		applied = false
		var cur v1alpha1.UsageRollup
		err := a.Reader.Get(ctx, key, &cur)
		if kerrors.IsNotFound(err) {
			cur = v1alpha1.UsageRollup{
				ObjectMeta: metav1.ObjectMeta{
					Name: name, Namespace: a.Namespace,
					Labels: map[string]string{v1alpha1.LabelUsageScope: string(scope.Type)},
				},
				Spec: v1alpha1.UsageRollupSpec{Scope: scope},
			}
			if err := a.Client.Create(ctx, &cur); err != nil && !kerrors.IsAlreadyExists(err) {
				return fmt.Errorf("create usage rollup %s: %w", name, err)
			}
			if err := a.Reader.Get(ctx, key, &cur); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if cur.Spec.Scope != scope {
			return fmt.Errorf("usage rollup %s holds scope %+v, not %+v", name, cur.Spec.Scope, scope)
		}
		if !stats.ApplyUsage(&cur.Status, ledger, d, login, scope.Type == v1alpha1.UsageScopeUser, a.now()) {
			return nil // already applied: a crash between this write and the run's marker
		}
		if err := a.Client.Status().Update(ctx, &cur); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

func (a *Aggregator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Aggregator) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.New(slog.DiscardHandler)
}

// New is the Aggregator a run controller wires in: the manager's client for
// writes, its API reader for every read, the release namespace, and
// measuring when its usage meter is on. It is always built: it acts only on
// runs that carry the usage finalizer, so a controller whose attribution
// mode was turned off still releases the runs created while it was on.
func New(c client.Client, reader client.Reader, namespace string, measuring bool, log *slog.Logger) *Aggregator {
	return &Aggregator{Client: c, Reader: reader, Namespace: namespace, Measuring: measuring, Log: log}
}
