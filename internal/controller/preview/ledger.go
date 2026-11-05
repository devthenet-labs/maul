// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolledger"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// Collecting the tool pod (DESIGN 2.2 step 10; SPEC Q7, Q8): preview-
// controller reads the tools container's log (that container only) and
// termination message, and toolledger holds the entry lines to the summary
// both carry and derives what the ledger shows for the verification's
// status: an index of every entry, the replay and confirmation runs, and
// the confirmed checks' bodies read from the ledger's own dry-run records,
// never from the evaluator's report. The full ledger goes to <pv>-ledger, a
// ConfigMap the verification owns in the release namespace, its oldest
// outputs dropped past maxLedgerConfigMap; the index on status is never
// cut. A log that does not hold is maul's trouble (Infra ledger-mismatch),
// never a verdict.

const (
	// ledgerSuffix and evidenceSuffix name the verification's ConfigMaps:
	// <pv>-ledger and <pv>-evidence.
	ledgerSuffix   = "-ledger"
	evidenceSuffix = "-evidence"
	// maxLedgerConfigMap bounds what a ledger or evidence ConfigMap holds.
	maxLedgerConfigMap = toolledger.MaxConfigMapBytes
)

// ledgerMeta names a ConfigMap of the verification: a controller reference
// to it, and the Intent's label, which intent-controller's and the status
// page's label-confined, owner-checked reads of a ConfigMap require.
func ledgerMeta(pv *v1alpha1.PreviewVerification, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: pv.Namespace,
		Labels: map[string]string{v1alpha1.LabelIntent: pv.Spec.IntentRef.Name,
			v1alpha1.LabelPreviewVerification: string(pv.UID), labelManagedBy: managedBy},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(),
			Kind: "PreviewVerification", Name: pv.Name, UID: pv.UID, Controller: ptr(true)}}}
}

// ledgerConfigMap is the full ledger as <pv>-ledger: toolledger's
// ConfigMapData, immutable, owned by the verification.
func ledgerConfigMap(pv *v1alpha1.PreviewVerification, l toolwire.Ledger) (*corev1.ConfigMap, error) {
	data, err := toolledger.ConfigMapData(l)
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{ObjectMeta: ledgerMeta(pv, pv.Name+ledgerSuffix), Immutable: ptr(true), Data: data}, nil
}
