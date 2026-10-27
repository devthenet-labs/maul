// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtester

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// TerminationPath is the container's termination message file (the
// Kubernetes default terminationMessagePath).
const TerminationPath = "/dev/termination-log"

// maxTerminationBytes is the kubelet's bound on a termination message.
const maxTerminationBytes = 4 << 10

// Run first waits, within ReadinessWindow of the budget, for every target
// of s to accept a TCP connection (readiness.go). It then observes every
// criterion of s in plan order, each against its target, and writes the
// MAUL-VERIFY v1 wire format to out: one line per criterion, then the
// summary, with the readiness waits, which it returns for the termination
// message. A target still unreachable at the end of the window ends the run
// there: no criterion is observed, and the summary names the target, which
// preview-controller settles as infrastructure, never as verdicts. Once
// the spec's budget has run out, every request still to be sent or in
// flight fails as timed out, so the criteria it leaves are observed failing
// and the summary is still written: an application that hangs is judged,
// never mistaken for a broken tester. A cancelled context
// (the tester itself told to stop) stops it before the next line, and it
// then writes no summary, so a cut-short run is never read as complete.
func Run(ctx context.Context, s Spec, c *Client, out io.Writer) (acceptance.Summary, error) {
	if err := s.Validate(); err != nil {
		return acceptance.Summary{}, err
	}
	total := time.Duration(s.BudgetSeconds) * time.Second
	budget, cancel := context.WithTimeout(ctx, total)
	defer cancel()
	waits := c.awaitTargets(budget, s.Targets, ReadinessWindow(total))
	if err := ctx.Err(); err != nil {
		return acceptance.Summary{}, fmt.Errorf("stopped waiting for the targets: %w", err)
	}
	w, err := acceptance.NewWireWriter(out, waits)
	if err != nil {
		return acceptance.Summary{}, err
	}
	if slices.ContainsFunc(waits, func(w acceptance.TargetWait) bool { return !w.Reached() }) {
		return w.Close()
	}
	rt := s.RunTokens()
	for _, cr := range s.Criteria {
		if err := ctx.Err(); err != nil {
			return acceptance.Summary{}, fmt.Errorf("stopped before %s: %w", cr.ID, err)
		}
		obs := acceptance.Observe(cr, rt, c.Doer(budget, s.Targets[cr.Target]))
		if err := ctx.Err(); err != nil {
			return acceptance.Summary{}, fmt.Errorf("stopped during %s: %w", cr.ID, err)
		}
		if err := w.Write(obs); err != nil {
			return acceptance.Summary{}, fmt.Errorf("write %s: %w", cr.ID, err)
		}
	}
	return w.Close()
}

// WriteTermination writes the summary as the termination message at path.
func WriteTermination(path string, sum acceptance.Summary) error {
	msg := sum.TerminationMessage()
	if len(msg) > maxTerminationBytes {
		return fmt.Errorf("termination message is over %d bytes", maxTerminationBytes)
	}
	if err := os.WriteFile(path, []byte(msg), 0o600); err != nil {
		return fmt.Errorf("write termination message: %w", err)
	}
	return nil
}
