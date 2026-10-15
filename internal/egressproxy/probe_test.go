// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// probeRig is an engine logging its audit lines to the returned buffer,
// art-npm granted to profile web and answering h, holding cred.
func probeRig(t *testing.T, h http.HandlerFunc, cred Credential) (*Server, *bytes.Buffer, *recordingUpstream) {
	t.Helper()
	w := newWorld(t)
	up := w.host(npmHost, h)
	creds := newFakeCreds()
	creds.set("art-npm", cred)
	cfg := Config{Profiles: mustProfiles(t, npmProfile(nil)), Registries: mustRegistries(t, npmRegistry()),
		Credentials: creds, rootCAs: w.pki.pool, dialRewrite: w.rewrite}
	cfg.Policy.testLoopback = true
	var audit bytes.Buffer
	srv, err := New(cfg, slog.New(slog.NewJSONHandler(&audit, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return srv, &audit, up
}

// TestProbe: a probe's request reaches the registry with its credential and
// reads its own audit line: the outcome, the statuses and the credential's
// kind, OK only for a forwarded 200. Lines already in the buffer (an
// earlier request's, a credential_rejected warning without an outcome) are
// never read as the probe's.
func TestProbe(t *testing.T) {
	static, err := BearerCredential(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	minted, err := CodeArtifactCredential(netprofile.EcosystemNPM, sentinel, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		h      http.HandlerFunc
		cred   Credential
		want   ProbeResult
		wantOK bool
	}{
		{"static 200", okBody(`{"name":"left-pad"}`), static,
			ProbeResult{Status: 200, UpstreamStatus: 200, Outcome: OutcomeProxied, Credential: "static"}, true},
		{"codeartifact 200", okBody(`{"name":"left-pad"}`), minted,
			ProbeResult{Status: 200, UpstreamStatus: 200, Outcome: OutcomeProxied, Credential: "codeartifact"}, true},
		{"upstream 404", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
			static, ProbeResult{Status: 404, UpstreamStatus: 404, Outcome: OutcomeProxied, Reason: ReasonNotFound,
				Credential: "static"},
			false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, audit, up := probeRig(t, tt.h, tt.cred)
			audit.WriteString(`{"msg":"refused","status":403,"outcome":"not_granted","credential":"none"}` + "\n")
			audit.WriteString(`{"msg":"credential_rejected","credential":"static","upstream_status":401}` + "\n")
			res, err := Probe(t.Context(), srv, "web", "/r/art-npm/left-pad", audit)
			if err != nil {
				t.Fatal(err)
			}
			if res != tt.want || res.OK() != tt.wantOK {
				t.Errorf("Probe = %+v ok=%v, want %+v ok=%v", res, res.OK(), tt.want, tt.wantOK)
			}
			hits := up.Hits()
			if len(hits) != 1 || hits[0].requestURI != npmBase+"/left-pad" ||
				!strings.HasSuffix(hits[0].header.Get("Authorization"), sentinel) {
				t.Errorf("upstream hits = %+v, want one credentialed packument GET", hits)
			}
		})
	}
}

// TestProbeFailures: an unknown profile and a request that leaves no audit
// line in the buffer are errors, not outcomes.
func TestProbeFailures(t *testing.T) {
	static, err := BearerCredential(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	srv, _, _ := probeRig(t, okBody("{}"), static)
	if _, err := Probe(t.Context(), srv, "nope", "/r/art-npm/left-pad", &bytes.Buffer{}); err == nil {
		t.Error("an unknown profile was probed")
	}
	_, err = Probe(t.Context(), srv, "web", "/r/art-npm/left-pad", &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no audit line") {
		t.Errorf("Probe without its audit line = %v, want the no-audit-line error", err)
	}
}

// TestProbeResultOK: only a forwarded 200 is OK.
func TestProbeResultOK(t *testing.T) {
	for _, tt := range []struct {
		res  ProbeResult
		want bool
	}{
		{ProbeResult{Status: 200, Outcome: OutcomeProxied}, true},
		{ProbeResult{Status: 404, Outcome: OutcomeProxied}, false},
		{ProbeResult{Status: 200, Outcome: OutcomeCredentialUnavailable}, false},
		{ProbeResult{Status: 503, Outcome: OutcomeCredentialUnavailable}, false},
	} {
		if got := tt.res.OK(); got != tt.want {
			t.Errorf("%+v.OK() = %v, want %v", tt.res, got, tt.want)
		}
	}
}
