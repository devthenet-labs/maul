// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"net/http"
	"testing"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

// TestConfirmRefusesARedCheckListingAnErrorStatus (third review, F2): a
// check proposed to show a defect that lists an error status is refused at
// confirm, whatever its dry run showed.
func TestConfirmRefusesARedCheckListingAnErrorStatus(t *testing.T) {
	// The request names a field the API does not take: its correct 400 says
	// "name is required".
	const check = `{"id":"p.err","outcome":"p","form":"http","http":{"target":"api","steps":[
	 {"request":{"method":"POST","path":"/api/players/{{run}}","json":{"title":"{{run}}"}},
	  "expect":{"status":[201,400],"json":{"/name":"{{run}}"}}}]}}`
	api := newFakeComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"name":"is required"}`))
	})
	h := newHarness(t, baseConfig(), routes{"10.0.0.1:80": api.srv.Listener.Addr().String()}, nil)
	h.call(toolwire.VerbCheckRun, `{"check":`+check+`}`)
	d := bareDigest(t, check)
	if _, resp := h.call(toolwire.VerbConfirm, redConfirm(t, d, true)); resp.Refused != toolwire.RefusedNoRedDryRun {
		t.Errorf("a red check listing status 400 was confirmed: %+v", resp)
	}
	h.finish()
}
