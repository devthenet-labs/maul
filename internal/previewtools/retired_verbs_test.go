// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// retiredPaths are the check runner's endpoints, which no relay serves any
// more, each with a body its old endpoint took.
var retiredPaths = map[string]string{
	"/v1/check-run":     `{"check":{"id":"x.y","outcome":"x","form":"exec","exec":{"argv":["true"],"expect":{"exit":0}}}}`,
	"/v1/check-results": `{}`,
	"/v1/confirm":       `{"digests":["` + strings.Repeat("a", 64) + `"]}`,
}

// postRetired posts body to path, keyed or not, from a remote caller.
func postRetired(h *harness, path, body string, keyed bool) (int, toolwire.Response) {
	h.t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "10.9.9.9:41000"
	if keyed {
		req.Header.Set(toolwire.KeyHeader, wsKey)
	}
	rec := httptest.NewRecorder()
	h.relay.Handler().ServeHTTP(rec, req)
	var resp toolwire.Response
	if err := toolwire.DecodeResponse(rec.Body, &resp); err != nil {
		h.t.Fatalf("%s: decode %q: %v", path, rec.Body.String(), err)
	}
	return rec.Code, resp
}

// TestRetiredCheckVerbsAreNoTool: check-run, check-results and confirm
// answer 404 "no such tool", in the slot and in a workspace, with the key
// or without: no entry records them and none spends the agent's calls.
func TestRetiredCheckVerbsAreNoTool(t *testing.T) {
	slotCfg := baseConfig()
	slotCfg.MaxToolCalls = 50
	wsCfg := wsConfig()
	wsCfg.MaxToolCalls = 50
	for _, tc := range []struct {
		name  string
		h     func(t *testing.T) *harness
		keyed bool
	}{
		{"slot", func(t *testing.T) *harness { return newHarness(t, slotCfg, routes{}, nil) }, false},
		{"workspace with the key", func(t *testing.T) *harness {
			return newWorkspaceHarness(t, wsCfg, &fakeSandbox{}, &fakeSandbox{})
		}, true},
		{"workspace without the key", func(t *testing.T) *harness {
			return newWorkspaceHarness(t, wsCfg, &fakeSandbox{}, &fakeSandbox{})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.h(t)
			before := h.relay.entries
			for path, body := range retiredPaths {
				for range 3 {
					code, resp := postRetired(h, path, body, tc.keyed)
					if code != http.StatusNotFound || resp.Refused != toolwire.RefusedVerb || resp.Reason != "no such tool" ||
						resp.Seq != 0 {
						t.Errorf("%s: %d %+v, want 404 %s \"no such tool\", unrecorded", path, code, resp,
							toolwire.RefusedVerb)
					}
				}
			}
			if h.relay.entries != before {
				t.Errorf("the retired calls wrote %d entries", h.relay.entries-before)
			}
			// Every one of the agent's calls is still its own.
			for i := range 50 {
				if _, resp := h.callKeyed(toolwire.VerbExec, `{"argv":["true"]}`); resp.Refused == toolwire.RefusedToolCalls {
					t.Fatalf("call %d: %+v, the retired calls spent the agent's calls", i+1, resp)
				}
			}
		})
	}
}

// TestTheAgentKeepsAllButTheFinishsRoom: past the agent's calls the ledger
// keeps room only for agent-runner's finish (and the relay's own, should
// the runner never come), so an agent that spends everything on refusals
// leaves every other entry to the round, and the finish still lands with
// the cited evidence and the summary.
func TestTheAgentKeepsAllButTheFinishsRoom(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake")
	sha := sha256Hex(png)
	cfg := baseConfig()
	cfg.MaxToolCalls = 300
	h := newHarness(t, cfg, routes{}, func(o *Options) { o.Artifacts = fakeArtifacts{sha: png} })
	writeFile(t, h.dir, EventsFile, "")
	shot, err := h.relay.record(entryOf(toolwire.KindAgent, toolwire.VerbBrowser, `{"verb":"screenshot"}`,
		withArtifacts(toolwire.Artifact{Name: "shot.png", SHA256: sha, Bytes: int64(len(png))})))
	if err != nil {
		t.Fatal(err)
	}
	for range toolwire.MaxEntries {
		h.call(toolwire.VerbEvents, `{}`)
	}
	_, resp := h.call(toolwire.VerbFinish, mustJSON(t, toolwire.FinishRequest{
		Evidence: []toolwire.EvidenceRef{{Seq: shot, SHA256: sha}}}))
	if resp.Refused != "" || resp.Seq != toolwire.MaxEntries-1 {
		t.Fatalf("finish: %+v, want it recorded as entry %d", resp, toolwire.MaxEntries-1)
	}
	l := h.verify()
	agent := 0
	for _, e := range l.Entries {
		if e.Kind == toolwire.KindAgent {
			agent++
		}
	}
	if agent != toolwire.MaxEntries-2 {
		t.Errorf("%d agent entries, want %d: the ledger keeps room past the finish's", agent, toolwire.MaxEntries-2)
	}
	if len(l.Evidence) != 1 || l.Evidence[0].SHA256 != sha || l.Summary.Count != toolwire.MaxEntries-1 {
		t.Errorf("evidence %+v, summary %+v: the finish did not land whole", l.Evidence, l.Summary)
	}
}
