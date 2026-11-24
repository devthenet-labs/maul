// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolexec

import (
	"context"
	"testing"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestNotStartedIsMaulsMarker (FD 2): what the shim answers for a program
// it could not start is exactly what the ledger index reads as one that
// never ran (toolwire.NotStarted), whether it was not found (127) or could
// not be executed (126); a program that ran and exited 127 is not.
func TestNotStartedIsMaulsMarker(t *testing.T) {
	s := newServer(t)
	if err := s.writeFiles([]toolwire.File{{Name: "data.txt", Content: []byte("not a program")}}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		argv []string
		exit int
		want bool
	}{
		{"not found", []string{"maul-no-such-program"}, 127, true},
		{"not executable", []string{"./data.txt"}, 126, true},
		{"ran and exited 127", []string{"sh", "-c", "exit 127"}, 127, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := s.Run(context.Background(), &toolwire.ExecRequest{Argv: tt.argv})
			if got.Exit != tt.exit || toolwire.NotStarted(got.Exit, got.Output) != tt.want {
				t.Errorf("exit %d output %q: NotStarted %v, want exit %d and %v", got.Exit, got.Output,
					toolwire.NotStarted(got.Exit, got.Output), tt.exit, tt.want)
			}
		})
	}
}
