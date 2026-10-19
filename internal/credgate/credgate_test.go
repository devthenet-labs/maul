// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package credgate

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/quick"

	"github.com/bitwise-media-group/patchy/api/v1alpha1"
)

// seed keeps every property deterministic in the gate.
const seed = 20261018

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

func person(login string, id int64) *v1alpha1.Attribution {
	return &v1alpha1.Attribution{User: &v1alpha1.UserRef{Login: login, ID: id}, Reason: v1alpha1.AttributionApprover}
}

const (
	anthropic = v1alpha1.ModelCredentialProviderAnthropic
	openai    = v1alpha1.ModelCredentialProviderOpenAI
)

func record(id int64, health v1alpha1.ModelCredentialHealth) *v1alpha1.ModelCredential {
	return recordFor(anthropic, id, health)
}

func recordFor(p v1alpha1.ModelCredentialProvider, id int64,
	health v1alpha1.ModelCredentialHealth) *v1alpha1.ModelCredential {
	owner := v1alpha1.ModelCredentialOwner(id)
	c := &v1alpha1.ModelCredential{
		Spec: v1alpha1.ModelCredentialSpec{Owner: owner, Provider: p, Kind: v1alpha1.ModelCredentialAPIKey},
	}
	c.Name = v1alpha1.ModelCredentialName(owner, p)
	c.Status.Health = health
	return c
}

// TestProviderFor: the provider a run's credential is judged on comes from
// the harness through the model registry: claude anthropic, codex openai,
// the fake harness as claude; copilot and anything unknown none.
func TestProviderFor(t *testing.T) {
	for _, tc := range []struct {
		harness string
		want    v1alpha1.ModelCredentialProvider
		ok      bool
	}{
		{"claude", anthropic, true}, {"codex", openai, true}, {"fake", anthropic, true},
		{"copilot", "", false}, {"", "", false}, {"gemini", "", false},
	} {
		got, ok := ProviderFor(tc.harness)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ProviderFor(%q) = %q, %v; want %q, %v", tc.harness, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
		ok   bool
	}{
		{"", ModeOff, true}, {"off", ModeOff, true}, {" attribute", ModeAttribute, true},
		{"enforce", ModeEnforce, true}, {"Enforce", "", false}, {"on", "", false},
	} {
		got, err := ParseMode(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestJudge(t *testing.T) {
	alice := person("alice", 42)
	other := record(7, v1alpha1.ModelCredentialValid)
	misnamed := record(42, v1alpha1.ModelCredentialValid)
	misnamed.Name = "0000000000000000-anthropic"
	for _, tc := range []struct {
		name   string
		mode   Mode
		a      *v1alpha1.Attribution
		cred   *v1alpha1.ModelCredential
		reason string
	}{
		{"off never holds", ModeOff, alice, nil, ""},
		{"attribute never holds", ModeAttribute, alice, nil, ""},
		{"enforce: an org run", ModeEnforce, &v1alpha1.Attribution{Reason: v1alpha1.AttributionOrg}, nil, ""},
		{"enforce: no attribution", ModeEnforce, nil, nil, ""},
		{"enforce: unregistered", ModeEnforce,
			&v1alpha1.Attribution{Reason: v1alpha1.AttributionApproverUnregistered}, nil, ""},
		{"enforce: valid", ModeEnforce, alice, record(42, v1alpha1.ModelCredentialValid), ""},
		{"enforce: none stored", ModeEnforce, alice, nil, ReasonMissing},
		{"enforce: someone else's", ModeEnforce, alice, other, ReasonMissing},
		{"enforce: misnamed record", ModeEnforce, alice, misnamed, ReasonMissing},
		{"enforce: rejected", ModeEnforce, alice, record(42, v1alpha1.ModelCredentialRejected), ReasonRejected},
		{"enforce: unknown", ModeEnforce, alice, record(42, v1alpha1.ModelCredentialUnknown), ReasonUnverified},
		{"enforce: never probed", ModeEnforce, alice, record(42, ""), ReasonUnverified},
		{"enforce: another provider's valid record", ModeEnforce, alice,
			recordFor(openai, 42, v1alpha1.ModelCredentialValid), ReasonMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Judge(tc.mode, anthropic, tc.a, tc.cred)
			if v.Hold != (tc.reason != "") || v.Reason != tc.reason {
				t.Fatalf("Judge = %+v, want reason %q", v, tc.reason)
			}
			if v.Hold && (!reflect.DeepEqual(v.User, tc.a.User) || v.Provider != anthropic) {
				t.Errorf("held user, provider = %+v, %s, want %+v, anthropic", v.User, v.Provider, tc.a.User)
			}
		})
	}
}

// TestJudgeOpenAI: a codex run is judged on the person's OpenAI record
// alone: their Valid Anthropic record does not let it through, a Valid
// OpenAI key does, and an OpenAI record of a kind OpenAI does not take
// (a setup-token) counts as none.
func TestJudgeOpenAI(t *testing.T) {
	alice := person("alice", 42)
	setupToken := recordFor(openai, 42, v1alpha1.ModelCredentialValid)
	setupToken.Spec.Kind = v1alpha1.ModelCredentialOAuthToken
	for _, tc := range []struct {
		name   string
		cred   *v1alpha1.ModelCredential
		reason string
	}{
		{"valid openai key", recordFor(openai, 42, v1alpha1.ModelCredentialValid), ""},
		{"valid anthropic record", record(42, v1alpha1.ModelCredentialValid), ReasonMissing},
		{"none", nil, ReasonMissing},
		{"an openai setup-token", setupToken, ReasonMissing},
		{"rejected", recordFor(openai, 42, v1alpha1.ModelCredentialRejected), ReasonRejected},
		{"unknown", recordFor(openai, 42, v1alpha1.ModelCredentialUnknown), ReasonUnverified},
	} {
		v := Judge(ModeEnforce, openai, alice, tc.cred)
		if v.Hold != (tc.reason != "") || v.Reason != tc.reason || (v.Hold && v.Provider != openai) {
			t.Errorf("%s: Judge = %+v, want reason %q on openai", tc.name, v, tc.reason)
		}
	}
}

// TestJudgeProperty: whatever the mode, attribution and record, a run is
// held exactly when the mode is enforce, a person with an id is behind it,
// and the record is not that person's record for the run's provider found
// Valid. So off and attribute never hold, an org run never holds, and no
// one else's credential, nor the person's own for another provider, ever
// lets a person's run through.
func TestJudgeProperty(t *testing.T) {
	modes := []Mode{ModeOff, ModeAttribute, ModeEnforce}
	healths := []v1alpha1.ModelCredentialHealth{"", v1alpha1.ModelCredentialUnknown, v1alpha1.ModelCredentialValid,
		v1alpha1.ModelCredentialRejected}
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		mode := modes[r.Intn(len(modes))]
		var a *v1alpha1.Attribution
		switch r.Intn(4) {
		case 0:
		case 1:
			a = &v1alpha1.Attribution{Reason: v1alpha1.AttributionOrg}
		default:
			a = person("p"+strconv.Itoa(r.Intn(5)), int64(r.Intn(4))) // 0 is no id
		}
		providers := v1alpha1.ModelCredentialProviders
		p := providers[r.Intn(len(providers))]
		var cred *v1alpha1.ModelCredential
		if r.Intn(4) > 0 {
			cred = recordFor(providers[r.Intn(len(providers))], int64(r.Intn(4)+1), healths[r.Intn(len(healths))])
			if r.Intn(8) == 0 {
				cred.Spec.Provider = "other"
			}
			if r.Intn(8) == 0 {
				cred.Spec.Kind = v1alpha1.ModelCredentialOAuthToken
			}
		}
		args[0], args[1], args[2], args[3] = reflect.ValueOf(mode), reflect.ValueOf(p), reflect.ValueOf(a),
			reflect.ValueOf(cred)
	}
	prop := func(mode Mode, p v1alpha1.ModelCredentialProvider, a *v1alpha1.Attribution,
		cred *v1alpha1.ModelCredential) bool {
		v := Judge(mode, p, a, cred)
		u := Person(a)
		ownValid := u != nil && cred != nil && cred.Spec.Owner == Owner(u) && cred.Spec.Provider == p &&
			cred.Name == v1alpha1.ModelCredentialName(Owner(u), p) &&
			v1alpha1.ModelCredentialKindAllowed(p, cred.Spec.Kind) &&
			cred.Status.Health == v1alpha1.ModelCredentialValid
		want := mode == ModeEnforce && u != nil && !ownValid
		if v.Hold != want {
			return false
		}
		return !v.Hold || (v.User == u && v.Reason != "" && v.Provider == p)
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

type fakeLookup struct {
	creds map[string]*v1alpha1.ModelCredential
	err   error
	asked int
}

func (f *fakeLookup) ModelCredential(_ context.Context, owner string, p v1alpha1.ModelCredentialProvider) (
	*v1alpha1.ModelCredential, error) {
	f.asked++
	if c := f.creds[owner+"/"+string(p)]; c != nil {
		return c, f.err
	}
	return f.creds[owner], f.err
}

func TestCheckReadsOnlyUnderEnforceForAPerson(t *testing.T) {
	ctx := context.Background()
	l := &fakeLookup{creds: map[string]*v1alpha1.ModelCredential{
		"github-id:42": record(42, v1alpha1.ModelCredentialValid),
	}}
	for _, mode := range []Mode{ModeOff, ModeAttribute} {
		if v, err := Check(ctx, mode, anthropic, person("alice", 1), l); err != nil || v.Hold {
			t.Errorf("Check(%s) = %+v, %v", mode, v, err)
		}
	}
	org := &v1alpha1.Attribution{Reason: v1alpha1.AttributionOrg}
	if v, err := Check(ctx, ModeEnforce, anthropic, org, l); err != nil ||
		v.Hold {
		t.Errorf("Check(org) = %+v, %v", v, err)
	}
	if l.asked != 0 {
		t.Fatalf("read %d records for runs that read none", l.asked)
	}
	if v, err := Check(ctx, ModeEnforce, anthropic, person("alice", 42), l); err != nil || v.Hold {
		t.Errorf("Check(valid) = %+v, %v", v, err)
	}
	if v, err := Check(ctx, ModeEnforce, anthropic, person("bob", 9), l); err != nil || !v.Hold ||
		v.Reason != ReasonMissing {
		t.Errorf("Check(missing) = %+v, %v", v, err)
	}
	// alice's Valid record is Anthropic's: a codex run of hers is held.
	if v, err := Check(ctx, ModeEnforce, openai, person("alice", 42), l); err != nil || !v.Hold ||
		v.Reason != ReasonMissing || v.Provider != openai {
		t.Errorf("Check(openai, anthropic record only) = %+v, %v", v, err)
	}
	l.creds["github-id:42/openai"] = recordFor(openai, 42, v1alpha1.ModelCredentialValid)
	if v, err := Check(ctx, ModeEnforce, openai, person("alice", 42), l); err != nil || v.Hold {
		t.Errorf("Check(openai, valid) = %+v, %v", v, err)
	}
	l.err = errors.New("boom")
	if _, err := Check(ctx, ModeEnforce, anthropic, person("alice", 42), l); err == nil {
		t.Error("a failed read was not an error")
	}
	if _, err := Check(ctx, ModeEnforce, anthropic, person("alice", 42), nil); err == nil {
		t.Error("enforce without a lookup was not an error")
	}
}

func TestHolds(t *testing.T) {
	ctx := context.Background()
	l := &fakeLookup{creds: map[string]*v1alpha1.ModelCredential{
		"github-id:42": record(42, v1alpha1.ModelCredentialValid),
	}}
	for _, tc := range []struct {
		mode Mode
		id   int64
		want bool
	}{
		{ModeEnforce, 42, false}, {ModeEnforce, 9, true}, {ModeEnforce, 0, true},
		{ModeAttribute, 9, false}, {ModeOff, 0, false},
	} {
		got, err := Holds(ctx, tc.mode, anthropic, "x", tc.id, l)
		if err != nil || got != tc.want {
			t.Errorf("Holds(%s, %d) = %v, %v; want %v", tc.mode, tc.id, got, err, tc.want)
		}
	}
}

func TestMessage(t *testing.T) {
	v := Verdict{Hold: true, User: &v1alpha1.UserRef{Login: "alice", ID: 42}, Reason: ReasonMissing}
	got := Message(v, "https://status.example.com/#/account", true)
	for _, want := range []string{"@alice has no Anthropic credential", "https://status.example.com/#/account",
		"approver who has a valid credential"} {
		if !strings.Contains(got, want) {
			t.Errorf("Message = %q, missing %q", got, want)
		}
	}
	if got := Message(v, "", false); strings.Contains(got, "approver") || !strings.Contains(got, "Account page") {
		t.Errorf("Message(no url, no reapprove) = %q", got)
	}
	v.Reason = ReasonRejected
	if got := Message(v, "", false); !strings.Contains(got, "was rejected") {
		t.Errorf("rejected Message = %q", got)
	}
	v.Reason = ReasonUnverified
	if got := Message(v, "", false); !strings.Contains(got, "not been verified") {
		t.Errorf("unverified Message = %q", got)
	}
	if got := Message(Verdict{}, "x", true); got != "" {
		t.Errorf("Message(no hold) = %q", got)
	}
	// An OpenAI hold names OpenAI and asks for an API key only.
	v = Verdict{Hold: true, User: &v1alpha1.UserRef{Login: "alice", ID: 42}, Provider: openai, Reason: ReasonMissing}
	got = Message(v, "", false)
	if !strings.Contains(got, "@alice has no OpenAI credential") || !strings.Contains(got, "an OpenAI API key") ||
		strings.Contains(got, "Anthropic") || strings.Contains(got, "setup-token") {
		t.Errorf("openai Message = %q", got)
	}
	v.Reason = ReasonRejected
	if got := Message(v, "", false); !strings.Contains(got, "OpenAI credential was rejected by OpenAI") {
		t.Errorf("openai rejected Message = %q", got)
	}
}

// TestImportsOnlyStdlibAndAPI keeps the core pure: the stdlib, the API
// types and the model registry, itself stdlib only.
func TestImportsOnlyStdlibAndAPI(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			first, _, _ := strings.Cut(path, "/")
			if strings.Contains(first, ".") && path != "github.com/bitwise-media-group/patchy/api/v1alpha1" &&
				path != "github.com/bitwise-media-group/patchy/internal/model" {
				t.Errorf("%s imports %s: package credgate imports only the stdlib, api/v1alpha1 and "+
					"internal/model", name, path)
			}
		}
	}
}
