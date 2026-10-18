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

func record(id int64, health v1alpha1.ModelCredentialHealth) *v1alpha1.ModelCredential {
	owner := v1alpha1.ModelCredentialOwner(id)
	c := &v1alpha1.ModelCredential{
		Spec: v1alpha1.ModelCredentialSpec{Owner: owner, Provider: Provider, Kind: v1alpha1.ModelCredentialAPIKey},
	}
	c.Name = v1alpha1.ModelCredentialName(owner, Provider)
	c.Status.Health = health
	return c
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := Judge(tc.mode, tc.a, tc.cred)
			if v.Hold != (tc.reason != "") || v.Reason != tc.reason {
				t.Fatalf("Judge = %+v, want reason %q", v, tc.reason)
			}
			if v.Hold && !reflect.DeepEqual(v.User, tc.a.User) {
				t.Errorf("held user = %+v, want %+v", v.User, tc.a.User)
			}
		})
	}
}

// TestJudgeProperty: whatever the mode, attribution and record, a run is
// held exactly when the mode is enforce, a person with an id is behind it,
// and the record is not that person's anthropic record found Valid. So off
// and attribute never hold, an org run never holds, and no one else's
// credential ever lets a person's run through.
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
		var cred *v1alpha1.ModelCredential
		if r.Intn(4) > 0 {
			cred = record(int64(r.Intn(4)+1), healths[r.Intn(len(healths))])
			if r.Intn(8) == 0 {
				cred.Spec.Provider = "other"
			}
		}
		args[0], args[1], args[2] = reflect.ValueOf(mode), reflect.ValueOf(a), reflect.ValueOf(cred)
	}
	prop := func(mode Mode, a *v1alpha1.Attribution, cred *v1alpha1.ModelCredential) bool {
		v := Judge(mode, a, cred)
		u := Person(a)
		ownValid := u != nil && cred != nil && cred.Spec.Owner == Owner(u) && cred.Spec.Provider == Provider &&
			cred.Status.Health == v1alpha1.ModelCredentialValid
		want := mode == ModeEnforce && u != nil && !ownValid
		if v.Hold != want {
			return false
		}
		return !v.Hold || (v.User == u && v.Reason != "")
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

func (f *fakeLookup) ModelCredential(_ context.Context, owner string, _ v1alpha1.ModelCredentialProvider) (
	*v1alpha1.ModelCredential, error) {
	f.asked++
	return f.creds[owner], f.err
}

func TestCheckReadsOnlyUnderEnforceForAPerson(t *testing.T) {
	ctx := context.Background()
	l := &fakeLookup{creds: map[string]*v1alpha1.ModelCredential{
		"github-id:42": record(42, v1alpha1.ModelCredentialValid),
	}}
	for _, mode := range []Mode{ModeOff, ModeAttribute} {
		if v, err := Check(ctx, mode, person("alice", 1), l); err != nil || v.Hold {
			t.Errorf("Check(%s) = %+v, %v", mode, v, err)
		}
	}
	if v, err := Check(ctx, ModeEnforce, &v1alpha1.Attribution{Reason: v1alpha1.AttributionOrg}, l); err != nil ||
		v.Hold {
		t.Errorf("Check(org) = %+v, %v", v, err)
	}
	if l.asked != 0 {
		t.Fatalf("read %d records for runs that read none", l.asked)
	}
	if v, err := Check(ctx, ModeEnforce, person("alice", 42), l); err != nil || v.Hold {
		t.Errorf("Check(valid) = %+v, %v", v, err)
	}
	if v, err := Check(ctx, ModeEnforce, person("bob", 9), l); err != nil || !v.Hold || v.Reason != ReasonMissing {
		t.Errorf("Check(missing) = %+v, %v", v, err)
	}
	l.err = errors.New("boom")
	if _, err := Check(ctx, ModeEnforce, person("alice", 42), l); err == nil {
		t.Error("a failed read was not an error")
	}
	if _, err := Check(ctx, ModeEnforce, person("alice", 42), nil); err == nil {
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
		got, err := Holds(ctx, tc.mode, "x", tc.id, l)
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
}

// TestImportsOnlyStdlibAndAPI keeps the core pure.
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
			if strings.Contains(first, ".") && path != "github.com/bitwise-media-group/patchy/api/v1alpha1" {
				t.Errorf("%s imports %s: package credgate imports only the stdlib and api/v1alpha1", name, path)
			}
		}
	}
}
