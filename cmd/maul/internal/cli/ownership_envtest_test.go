// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/cmd/maul/internal/kubecfg"
	"github.com/devthenet-labs/maul/internal/action"
	"github.com/devthenet-labs/maul/internal/kube"
)

// The CLI's spec writes share their objects with an operator's server-side
// apply: Helm owns an Integration's configuration, the controllers a
// Finding's scan data. A CLI verb must take ownership of the one field it
// writes and nothing else, or the operator's next apply conflicts. A full
// Update did exactly that for every metav1.Duration the operator wrote in
// short form ("10m" reads back as "10m0s"), so `maul backfill` broke the
// next `helm upgrade` until it ran with --force-conflicts. A Finding's
// timestamps drift the same way (fractional seconds are dropped).

// cliManager is the field manager the real CLI writes as (its user agent).
const cliManager = "maul"

// helmIntegration is the Integration as the maul-config chart renders it,
// durations in the short form an operator writes.
func helmIntegration() *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "Integration",
		"metadata":   map[string]any{"name": "github", "namespace": testNamespace},
		"spec": map[string]any{
			"provider":  "github",
			"interval":  "10m",
			"secretRef": map[string]any{"name": "github-app"},
			"github": map[string]any{
				"codeScanningAlerts": map[string]any{"enabled": true},
				"redelivery":         map[string]any{"enabled": true, "lookback": "24h"},
			},
		},
	}}
	return u
}

// applyAs server-side-applies obj as manager without forcing, as Helm does
// unless told --force-conflicts.
func applyAs(t *testing.T, c client.Client, manager string, obj *unstructured.Unstructured) error {
	t.Helper()
	return c.Apply(t.Context(), client.ApplyConfigurationFromUnstructured(obj.DeepCopy()),
		client.FieldOwner(manager))
}

// cliClient is a client to the same API server that identifies itself the
// way the real CLI does, so its writes land under the CLI's field manager.
func cliClient(t *testing.T, cfg *rest.Config) client.Client {
	t.Helper()
	cfg = rest.CopyConfig(cfg)
	cfg.UserAgent = cliManager
	c, err := client.New(cfg, client.Options{Scheme: kube.Scheme()})
	if err != nil {
		t.Fatalf("build cli client: %v", err)
	}
	return c
}

// specFieldsOf reports the top-level spec fields manager owns on obj.
func specFieldsOf(t *testing.T, obj metav1.Object, manager string) []string {
	t.Helper()
	var out []string
	for _, mf := range obj.GetManagedFields() {
		if mf.Manager != manager || mf.FieldsV1 == nil {
			continue
		}
		var fields map[string]map[string]any
		if err := json.Unmarshal(mf.FieldsV1.GetRawBytes(), &fields); err != nil {
			t.Fatalf("decode managed fields of %s: %v", manager, err)
		}
		for k := range fields["f:spec"] {
			if name, ok := strings.CutPrefix(k, "f:"); ok {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return out
}

func TestBackfillLeavesHelmFieldsToHelm(t *testing.T) {
	opts, _, _, admin := startGetEnv(t)
	env, err := opts.Connect()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	cli := cliClient(t, env.Config)
	opts.WithEnv(&kubecfg.Env{Client: cli, Config: env.Config, Namespace: testNamespace})
	opts.WithAccess(func(context.Context, *kubecfg.Env, string, string) (bool, error) { return true, nil })
	opts.WithIdentity(func() string { return "op@acme.test" })

	if err := applyAs(t, admin, "helm", helmIntegration()); err != nil {
		t.Fatalf("first helm apply: %v", err)
	}
	if err := runBackfill(t.Context(), opts, "github", []string{"acme/"}); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	var integ v1alpha1.Integration
	if err := admin.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: "github"}, &integ); err != nil {
		t.Fatalf("get integration: %v", err)
	}
	if integ.Spec.Backfill == nil || integ.Spec.Backfill.By != "op@acme.test" {
		t.Fatalf("spec.backfill = %+v, want stamped by op@acme.test", integ.Spec.Backfill)
	}
	if got, want := specFieldsOf(t, &integ, cliManager), []string{"backfill"}; !slices.Equal(got, want) {
		t.Errorf("CLI owns spec fields %v, want only %v", got, want)
	}
	helmFields := specFieldsOf(t, &integ, "helm")
	for _, f := range []string{"github", "interval", "provider", "secretRef"} {
		if !slices.Contains(helmFields, f) {
			t.Errorf("helm no longer owns spec.%s (owns %v)", f, helmFields)
		}
	}

	// The operator's next upgrade, unforced, must go through.
	if err := applyAs(t, admin, "helm", helmIntegration()); err != nil {
		t.Fatalf("helm re-apply after backfill conflicted: %v", err)
	}
}

// controllerFinding is a Finding as a controller applies it. Its approval
// timestamp carries fractional seconds, which the API server stores as
// written and metav1.Time drops on a round trip ("...:00.250Z" reads back as
// "...:00Z"): a full Update of the typed object therefore changes that value
// and takes it from the controller, as it took Helm's short-form durations.
func controllerFinding() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "Finding",
		"metadata":   map[string]any{"name": "fnd-own", "namespace": testNamespace},
		"spec": map[string]any{
			"integrationRef": map[string]any{"name": "gh"},
			"source":         "github-code-scanning",
			"advisories":     []any{"CVE-2026-0001"},
			"title":          "Command injection in the build script",
			"severity":       "high",
			"approval":       map[string]any{"by": "octocat", "at": "2026-07-24T09:00:00.250Z"},
		},
	}}
}

func TestActionLeavesControllerFieldsAlone(t *testing.T) {
	opts, _, _, admin := startGetEnv(t)
	env, err := opts.Connect()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	cli := cliClient(t, env.Config)
	opts.WithEnv(&kubecfg.Env{Client: cli, Config: env.Config, Namespace: testNamespace})
	opts.WithAccess(func(context.Context, *kubecfg.Env, string, string) (bool, error) { return true, nil })
	opts.WithIdentity(func() string { return "op@acme.test" })

	const controller = "integration-controller"
	if err := applyAs(t, admin, controller, controllerFinding()); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	var fnd v1alpha1.Finding
	key := client.ObjectKey{Namespace: testNamespace, Name: "fnd-own"}
	if err := admin.Get(t.Context(), key, &fnd); err != nil {
		t.Fatalf("get finding: %v", err)
	}
	fnd.Status.Phase = v1alpha1.PhaseQueued
	if err := admin.Status().Update(t.Context(), &fnd); err != nil {
		t.Fatalf("seed finding status: %v", err)
	}

	for _, verb := range []string{action.VerbSuspend, action.VerbResume, action.VerbExpedite} {
		if err := runAction(t.Context(), opts, &actionFlags{yes: true}, verb, "finding", []string{"fnd-own"}); err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
	}

	var got v1alpha1.Finding
	if err := admin.Get(t.Context(), key, &got); err != nil {
		t.Fatalf("get finding: %v", err)
	}
	if got.Spec.Expedite == nil || got.Spec.Suspend {
		t.Fatalf("spec = expedite %+v suspend %v, want expedited and resumed", got.Spec.Expedite, got.Spec.Suspend)
	}
	if fields, want := specFieldsOf(t, &got, cliManager), []string{"expedite"}; !slices.Equal(fields, want) {
		t.Errorf("CLI owns spec fields %v, want only %v", fields, want)
	}
	if fields := specFieldsOf(t, &got, controller); !slices.Contains(fields, "approval") {
		t.Errorf("%s no longer owns spec.approval (owns %v)", controller, fields)
	}

	// The controller's next apply, unforced, must go through.
	if err := applyAs(t, admin, controller, controllerFinding()); err != nil {
		t.Fatalf("controller re-apply after the CLI's actions conflicted: %v", err)
	}
}
