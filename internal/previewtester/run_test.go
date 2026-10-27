// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtester

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

func specFor(t *testing.T, api, web *component) (Spec, *dialer) {
	t.Helper()
	s, err := ParseSpec([]byte(validSpec))
	if err != nil {
		t.Fatal(err)
	}
	return s, &dialer{routes: map[string]string{
		"preview-bb-api:80": listenerAddr(api),
		"preview-bb:80":     listenerAddr(web),
	}}
}

// TestRunWritesWire: one observation line per criterion, in plan order,
// each against its own component, closed by a summary the controller's
// parser accepts.
func TestRunWritesWire(t *testing.T) {
	api := newComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = io.WriteString(w, "ok") })
	web := newComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		_, _ = io.WriteString(w, "<h1>Beat Battle</h1>")
	})
	s, d := specFor(t, api, web)
	var out bytes.Buffer
	sum, err := Run(context.Background(), s, NewClient(Options{Dial: d.dial}), &out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	obs, parsed, err := acceptance.ParseWire(out.Bytes())
	if err != nil {
		t.Fatalf("ParseWire: %v\n%s", err, out.String())
	}
	if parsed != sum || sum.Count != 2 {
		t.Errorf("summary = %+v, parsed %+v", sum, parsed)
	}
	if len(obs) != 2 || obs[0].ID != "health" || obs[1].ID != "home" {
		t.Fatalf("observations = %+v", obs)
	}
	if got := api.requests(); len(got) != 1 || got[0].RequestURI != "/api/healthz" {
		t.Errorf("api saw %+v", got)
	}
	if got := web.requests(); len(got) != 1 || got[0].RequestURI != "/" {
		t.Errorf("web saw %+v", got)
	}
	for i, c := range s.Criteria {
		if v := acceptance.Judge(c, obs[i], s.RunTokens()); v.Outcome != acceptance.OutcomePass {
			t.Errorf("%s: Judge = %+v", c.ID, v)
		}
	}
	for _, line := range strings.SplitAfter(out.String(), "\n") {
		if line != "" && !strings.HasPrefix(line, acceptance.WirePrefix) {
			t.Errorf("stdout carries a non-wire line %q", line)
		}
	}
}

// TestWriteTermination: the termination message is the summary, as the
// controller parses it.
func TestWriteTermination(t *testing.T) {
	sum := acceptance.Summary{Count: 2, SHA256: strings.Repeat("ab", 32)}
	path := filepath.Join(t.TempDir(), "termination-log")
	if err := WriteTermination(path, sum); err != nil {
		t.Fatalf("WriteTermination: %v", err)
	}
	msg, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if term, err := acceptance.ParseSummary(string(msg)); err != nil || term != sum {
		t.Errorf("termination message %q = %+v, %v; want %+v", msg, term, err, sum)
	}
}

// TestRunStopsWhenCancelled: a tester told to stop writes no summary, so
// the controller never mistakes a cut-short run for a complete one.
func TestRunStopsWhenCancelled(t *testing.T) {
	api := newComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = io.WriteString(w, "ok") })
	web := newComponent(t, func(w http.ResponseWriter, _ *http.Request, _ int) { _, _ = io.WriteString(w, "ok") })
	s, d := specFor(t, api, web)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if _, err := Run(ctx, s, NewClient(Options{Dial: d.dial}), &out); err == nil {
		t.Fatal("Run succeeded after cancellation")
	}
	if _, _, err := acceptance.ParseWire(out.Bytes()); err != acceptance.ErrNoSummary {
		t.Errorf("ParseWire = %v, want ErrNoSummary\n%s", err, out.String())
	}
}

// TestRunRefusesInvalidSpec: Run holds a spec to ParseSpec's rules even
// when it was built in code.
func TestRunRefusesInvalidSpec(t *testing.T) {
	s, err := ParseSpec([]byte(validSpec))
	if err != nil {
		t.Fatal(err)
	}
	s.Targets = map[string]string{"api": "preview-bb-api:80"}
	var out bytes.Buffer
	if _, err := Run(context.Background(), s, NewClient(Options{}), &out); err == nil {
		t.Error("Run accepted a criterion with no target")
	}
	if out.Len() != 0 {
		t.Errorf("Run wrote %q", out.String())
	}
}
