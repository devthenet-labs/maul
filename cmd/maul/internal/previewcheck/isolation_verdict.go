// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"fmt"
	"strings"

	"github.com/devthenet-labs/maul/cmd/maul/internal/checkreport"
)

// The netprobe client summaries (hack/preview-isolation-probe/cmd/netprobe).
const (
	outcomeReached      = "REACHED"
	outcomeBlocked      = "BLOCKED"
	outcomeInconclusive = "INCONCLUSIVE"
)

// clientResult is what one client Pod did.
type clientResult struct {
	// ran: the Pod was created.
	ran bool
	// refusedBlocks: it counted a refused connection as blocked.
	refusedBlocks bool
	// outcome is its summary (REACHED, BLOCKED or INCONCLUSIVE), "" when
	// it gave none.
	outcome string
	// detail says why there is no outcome.
	detail string
}

// isolationRun is everything a probe run observed, in the order it ran:
// the listener started, the gateway's client ran alone, the two clients
// that must be refused ran once it had reached the listener, and the
// listener was read again after them.
type isolationRun struct {
	// setup is why the run stopped before any client could run (the
	// listener never became Ready, or has no IPv4 address).
	setup string
	// listener is the listener's address once Ready.
	listener                 string
	gateway, agent, release  clientResult
	agentWhere, releaseWhere string
	gatewayWhere             string
	// listenerChecked: the listener was read after the refused clients
	// finished; listenerSurvived: it was then still Running, the same Pod,
	// its container never restarted. listenerState says what it was.
	listenerChecked, listenerSurvived bool
	listenerState                     string
	// ipBlocks are the ranges the slot policy also admits.
	ipBlocks []string
	// cleanup lists the probe Pods that could not be deleted.
	cleanup []string
}

// isolationVerdict is preview-netpol's line.
type isolationVerdict struct {
	status checkreport.Status
	reason string
}

// judgeIsolation is preview-netpol's verdict, pure. It passes only when
// the gateway's client REACHED the listener, both other clients were
// BLOCKED, and the listener was still running after they finished. A
// refusal counts as blocked only under that order: the gateway's reach
// proves the listener was answering before them, and its survival that it
// still was after, so a connection refused in between can only be the
// policy's. A dead listener can never pass as blocked, and neither can a
// client that never ran.
func judgeIsolation(r isolationRun) isolationVerdict {
	var fails []string
	cleanupNote := ""
	if len(r.cleanup) > 0 {
		cleanupNote = "; and " + strings.Join(r.cleanup, ", ") + " could not be deleted: delete " +
			plural(len(r.cleanup), "it", "them") + " by hand"
	}
	if r.setup != "" {
		return isolationVerdict{checkreport.Fail, "the probe could not run: " + r.setup + cleanupNote}
	}
	gatewayReached := r.gateway.ran && r.gateway.outcome == outcomeReached
	if !gatewayReached {
		return isolationVerdict{checkreport.Fail, fmt.Sprintf("the gateway-labelled client %s did not reach the "+
			"listener at %s (%s), so no refusal can be judged and the other clients were not run: keep previews "+
			"unused and investigate (is the image pullable, the listener on a preview node?)%s", r.gatewayWhere,
			r.listener, describe(r.gateway), cleanupNote)}
	}
	for _, c := range []struct {
		where string
		res   clientResult
	}{{r.agentWhere, r.agent}, {r.releaseWhere, r.release}} {
		switch {
		case !c.res.ran:
			fails = append(fails, fmt.Sprintf("%s did not run (%s)", c.where, describe(c.res)))
		case c.res.outcome == outcomeReached:
			fails = append(fails, fmt.Sprintf("%s REACHED the listener: the slot policy admits more than the "+
				"preview gateway", c.where))
		case c.res.outcome != outcomeBlocked:
			fails = append(fails, fmt.Sprintf("%s was not blocked (%s)", c.where, describe(c.res)))
		}
	}
	switch {
	case !r.listenerChecked:
		fails = append(fails, "the listener could not be read after the refused clients ran ("+r.listenerState+
			"), so their refusals may be its own")
	case !r.listenerSurvived:
		fails = append(fails, "the listener was "+r.listenerState+" after the refused clients ran, so their "+
			"refusals may be its own")
	}
	if len(fails) > 0 {
		return isolationVerdict{checkreport.Fail, strings.Join(fails, "; ") + ": keep previews unused and " +
			"investigate" + cleanupNote}
	}
	reason := fmt.Sprintf("the slot's NetworkPolicies admit no peer but the preview gateway's (checked as "+
		"written), and end to end %s REACHED the listener at %s; then %s was BLOCKED, so agents cannot reach a "+
		"slot by any path (their own namespace's default-deny may be what stops them), and %s was BLOCKED by the "+
		"slot policy (a refusal counted only after that reach); and the listener was still Running after them",
		r.gatewayWhere, r.listener, r.agentWhere, r.releaseWhere)
	switch {
	case len(r.cleanup) > 0:
		return isolationVerdict{checkreport.Warn, reason + cleanupNote}
	case len(r.ipBlocks) > 0:
		return isolationVerdict{checkreport.Warn, reason + "; but the slot policy also admits " +
			strings.Join(r.ipBlocks, ", ") + " on port http, bypassing the gateway, which this run does not test " +
			"(they go once preview.legacyIngress is false)"}
	}
	return isolationVerdict{checkreport.Pass, reason + "; every probe Pod was deleted"}
}

// describe is a client's outcome or why it has none.
func describe(c clientResult) string {
	switch {
	case c.outcome != "" && c.detail != "":
		return c.outcome + ", " + c.detail
	case c.outcome != "":
		return c.outcome
	case c.detail != "":
		return c.detail
	}
	return "no summary"
}
