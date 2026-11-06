// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/toolexec"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestRunWorkspaceHeadNotBuiltNeverServes (S2-B5): a workspace relay
// whose head did not build never answers its readiness path, so its
// container is never Ready, status.tools is never written and no evaluator
// is launched (nor paid for); it finishes at once on its own account
// (FinishBuildFailed) after the two build entries, so the round settles
// Infra tools-build-failed without waiting for the deadline. A head that
// built serves as before, whatever the base did.
func TestRunWorkspaceHeadNotBuiltNeverServes(t *testing.T) {
	timesOut := func(req *toolexec.Request, _ int) (toolexec.Result, error) {
		if req.Kind == toolexec.KindBuild {
			return toolexec.Result{TimedOut: true, Exit: -1}, nil
		}
		return toolexec.Result{}, nil
	}
	refused := func(req *toolexec.Request, _ int) (toolexec.Result, error) {
		return toolexec.Result{Refused: "busy"}, nil
	}
	for _, tc := range []struct {
		name       string
		head, base func(*toolexec.Request, int) (toolexec.Result, error)
		headResult maulv1.BuildResult
		serves     bool
	}{
		{"head fails", buildsFail, nil, maulv1.BuildFailed, false},
		{"head times out", timesOut, nil, maulv1.BuildTimeout, false},
		{"head not run", refused, nil, maulv1.BuildError, false},
		{"both fail", buildsFail, buildsFail, maulv1.BuildFailed, false},
		{"base fails", nil, buildsFail, maulv1.BuildBuilt, true},
		{"both build", nil, nil, maulv1.BuildBuilt, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trees := t.TempDir()
			for _, side := range []string{"head", "base"} {
				writeTree(t, filepath.Join(trees, side), map[string]string{"go.mod": treeGoMod})
			}
			l := newLifecycle(t, wsConfig(), routes{})
			l.o.Head, l.o.Base = &fakeSandbox{do: tc.head}, &fakeSandbox{do: tc.base}
			l.o.Trees, l.o.Key = trees, wsKey
			l.o.Timeout = time.Minute
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			l.o.Listener = ln
			done := make(chan error, 1)
			go func() { _, err := Run(context.Background(), l.o); done <- err }()

			if tc.serves {
				waitReady(t, "http://"+ln.Addr().String())
				req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+
					toolwire.Path(toolwire.VerbFinish), strings.NewReader(`{}`))
				req.Header.Set(toolwire.KeyHeader, wsKey)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Run did not return: a head that did not build waits for the deadline")
			}
			if !tc.serves {
				c := http.Client{Timeout: 200 * time.Millisecond}
				if resp, err := c.Get("http://" + ln.Addr().String() + ReadyPath); err == nil {
					_ = resp.Body.Close()
					t.Errorf("the readiness path answered %d on a head that did not build", resp.StatusCode)
				}
			}
			led := l.verify(t)
			b := buildEntries(t, led)
			if b[""].Result != tc.headResult {
				t.Errorf("head build %+v, want %s", b[""], tc.headResult)
			}
			last := led.Entries[len(led.Entries)-1]
			if tc.serves {
				if last.Kind != toolwire.KindRunner || last.Verb != toolwire.VerbFinish {
					t.Errorf("last entry %+v, want the runner's finish", last)
				}
				return
			}
			if len(led.Entries) != 3 {
				t.Errorf("%d entries, want the two builds and finish: %+v", len(led.Entries), led.Entries)
			}
			if r := finishReason(t, last); r != FinishBuildFailed {
				t.Errorf("finish reason %q, want %q", r, FinishBuildFailed)
			}
		})
	}
}
