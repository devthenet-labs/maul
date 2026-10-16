// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package remediation

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/bitwise-media-group/patchy/api/v1alpha1"
	"github.com/bitwise-media-group/patchy/internal/jobs"
	"github.com/bitwise-media-group/patchy/internal/usagemeter/usagemetertest"
)

// TestMeasureSettledRemediation: a finished remediation's usage is settled
// from the broker on the harness the spawner resolved, whatever the pod
// claims it ran: a pod reporting "codex" on a claude run is still measured,
// and a codex run is reported without asking the broker.
func TestMeasureSettledRemediation(t *testing.T) {
	jobCreated := crdClock.Add(-time.Hour)
	for _, tc := range []struct {
		name, specHarness, podHarness string
		want                          v1alpha1.UsageSource
		asked                         int
	}{
		{"claude run, pod claims codex", "claude", "codex", v1alpha1.UsageSourceMeasured, 1},
		{"codex run", "codex", "codex", v1alpha1.UsageSourceReported, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objs := runningRemediation()
			rem := objs[1].(*v1alpha1.Remediation)
			rem.Spec.Parameters.Harness = tc.specHarness
			rem.Status.Phase = v1alpha1.RunComplete
			rem.Status.Stage = &v1alpha1.StageResult{Outcome: "ok", Harness: tc.podHarness,
				Usage: v1alpha1.UsageSummary{OutputTokens: 9}}
			runner := &fakeCRRunner{status: &jobs.Status{Done: true, Created: jobCreated}}
			r, c := newRemediation(t, runner, &fakeForge{}, objs...)
			broker, meter := usagemetertest.New(t, func() time.Time { return crdClock })
			broker.Set("job-rem-1", usagemetertest.Answer("job-rem-1", jobCreated.Add(-time.Hour),
				jobCreated.Add(time.Minute), 0, usagemetertest.Sonnet(0)))
			r.Meter = meter
			remOnce(t, r, rem.Name)

			if got := len(broker.Asked()); got != tc.asked {
				t.Errorf("broker asked %d times, want %d", got, tc.asked)
			}
			var got v1alpha1.Remediation
			if err := c.Get(t.Context(), types.NamespacedName{Namespace: "patchy", Name: rem.Name}, &got); err != nil {
				t.Fatal(err)
			}
			u := got.Status.Stage.Usage
			if !u.Final || u.Source != tc.want || u.OutputTokens != 9 {
				t.Fatalf("usage = %+v", u)
			}
			if (u.Measured != nil) != (tc.want == v1alpha1.UsageSourceMeasured) {
				t.Errorf("measured = %+v", u.Measured)
			}
		})
	}
}
