// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"

	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// Execer runs a program in the sandbox container: the exec shim's client
// (toolexec.Client) in a pod with a sandbox, a fake in tests.
type Execer interface {
	Exec(ctx context.Context, req *toolwire.ExecRequest) (toolexec.Result, error)
}

// reasonNoSandbox is maul's words for an exec in a pod without a sandbox.
const reasonNoSandbox = "this tool pod has no sandbox container"

// exec answers `maul-tool exec`: argv run in the sandbox. The output is the
// program's (untrusted); the exit code is maul's measurement.
func (r *Relay) exec(ctx context.Context, req *toolwire.ExecRequest) result {
	if r.execer == nil {
		return refusal(toolwire.RefusedUnavailable, reasonNoSandbox)
	}
	res, err := r.execer.Exec(ctx, req)
	if err != nil {
		return refusal(toolwire.RefusedUnavailable, "the sandbox did not answer")
	}
	if res.Refused != "" {
		return refusal(res.Refused, "the sandbox refused the exec")
	}
	exit := res.Exit
	return result{output: res.Output, inline: res.Output, exit: exit, observed: &toolwire.Observed{Exit: &exit}}
}
