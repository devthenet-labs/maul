// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package attribution

import (
	"errors"
	"go/parser"
	"go/token"
	"math"
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
const seed = 20261016

func quickConfig(n int) *quick.Config {
	return &quick.Config{MaxCount: n, Rand: rand.New(rand.NewSource(seed))}
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
		ok   bool
	}{
		{"", ModeOff, true},
		{"off", ModeOff, true},
		{" attribute ", ModeAttribute, true},
		{"enforce", "", false},
		{"Attribute", "", false},
	} {
		got, err := ParseMode(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseMode(%q) = %q, %v; want %q, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

// TestOffStampsNothing: mode off leaves the spec and the Job exactly as they
// were before attribution existed, whatever the run's attribution would be.
func TestOffStampsNothing(t *testing.T) {
	person := To("alice", 7, v1alpha1.AttributionApprover)
	for _, a := range []*v1alpha1.Attribution{nil, Org(), person} {
		if got := ModeOff.Stamp(a); got != nil {
			t.Errorf("off Stamp(%+v) = %+v, want nil", a, got)
		}
		if got := ModeOff.Annotation(a); got != "" {
			t.Errorf("off Annotation(%+v) = %q, want none", a, got)
		}
	}
	if got := ModeAttribute.Stamp(person); got != person {
		t.Errorf("attribute Stamp = %+v, want the attribution itself", got)
	}
	// Mandatory under attribute: a run with no attribution is the org's.
	if got := ModeAttribute.Annotation(nil); got != ValueOrg {
		t.Errorf("attribute Annotation(nil) = %q, want org", got)
	}
	if got := ModeAttribute.Annotation(person); got != "github-id:7" {
		t.Errorf("attribute Annotation(person) = %q, want github-id:7", got)
	}
}

// TestTo is the attribution table's re-attribution rule.
func TestTo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		login  string
		id     int64
		reason v1alpha1.AttributionReason
		want   v1alpha1.Attribution
	}{
		{"an approver with an id", "alice", 42, v1alpha1.AttributionApprover,
			v1alpha1.Attribution{User: &v1alpha1.UserRef{Login: "alice", ID: 42}, Reason: v1alpha1.AttributionApprover}},
		{"a reviewer", "bob-2", 9, v1alpha1.AttributionReviewer,
			v1alpha1.Attribution{User: &v1alpha1.UserRef{Login: "bob-2", ID: 9}, Reason: v1alpha1.AttributionReviewer}},
		{"an approver with no id", "alice", 0, v1alpha1.AttributionApprover,
			v1alpha1.Attribution{Reason: v1alpha1.AttributionApproverUnregistered}},
		{"a status-page approval", "github:alice", 0, v1alpha1.AttributionApprover,
			v1alpha1.Attribution{Reason: v1alpha1.AttributionApproverUnregistered}},
		{"a status-page username with an id", "github:alice", 5, v1alpha1.AttributionApprover,
			v1alpha1.Attribution{Reason: v1alpha1.AttributionApproverUnregistered}},
		{"an EKS ARN", "arn:aws:iam::111122223333:role/x", 0, v1alpha1.AttributionApprover,
			v1alpha1.Attribution{Reason: v1alpha1.AttributionApproverUnregistered}},
		{"a bot", "patchy[bot]", 3, v1alpha1.AttributionCommenter,
			v1alpha1.Attribution{Reason: v1alpha1.AttributionApproverUnregistered}},
		{"a login too long", strings.Repeat("a", 65), 3, v1alpha1.AttributionRequester,
			v1alpha1.Attribution{Reason: v1alpha1.AttributionApproverUnregistered}},
		{"a submitter with no id", "github:alice", 0, v1alpha1.AttributionSubmitter,
			v1alpha1.Attribution{Reason: v1alpha1.AttributionSubmitterUnregistered}},
		{"the org reason", "alice", 4, v1alpha1.AttributionOrg, v1alpha1.Attribution{Reason: v1alpha1.AttributionOrg}},
		{"an unregistered reason given as the reason", "alice", 4, v1alpha1.AttributionApproverUnregistered,
			v1alpha1.Attribution{Reason: v1alpha1.AttributionOrg}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := To(tc.login, tc.id, tc.reason); !reflect.DeepEqual(*got, tc.want) {
				t.Errorf("To(%q, %d, %s) = %+v, want %+v", tc.login, tc.id, tc.reason, *got, tc.want)
			}
		})
	}
}

func TestParseValue(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"org", 0, true},
		{"github-id:1", 1, true},
		{"github-id:9223372036854775807", math.MaxInt64, true},
		{"github-id:9223372036854775808", 0, false}, // in the grammar, past int64
		{"github-id:0", 0, false},
		{"github-id:01", 0, false},
		{"github-id:-1", 0, false},
		{"github:alice", 0, false},
		{"Org", 0, false},
		{"", 0, false},
		{"org ", 0, false},
		{"github-id:1\n", 0, false},
	} {
		got, err := ParseValue(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseValue(%q) = %d, %v; want %d, ok=%v", tc.in, got, err, tc.want, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrValue) {
			t.Errorf("ParseValue(%q): %v is not ErrValue", tc.in, err)
		}
	}
}

// TestValueRoundTripProperty: for any attribution To can make, the
// annotation value is in the admission grammar and parses back to the
// person's id (0 for the org), and only a person reason carries a user.
func TestValueRoundTripProperty(t *testing.T) {
	reasons := []v1alpha1.AttributionReason{
		v1alpha1.AttributionOrg, v1alpha1.AttributionRequester, v1alpha1.AttributionApprover,
		v1alpha1.AttributionCommenter, v1alpha1.AttributionReviewer, v1alpha1.AttributionSubmitter,
		v1alpha1.AttributionApproverUnregistered, v1alpha1.AttributionSubmitterUnregistered,
	}
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(randLogin(r))
		args[1] = reflect.ValueOf(randID(r))
		args[2] = reflect.ValueOf(reasons[r.Intn(len(reasons))])
	}
	prop := func(login string, id int64, reason v1alpha1.AttributionReason) bool {
		a := To(login, id, reason)
		v := Value(a)
		if !valueRE.MatchString(v) {
			return false
		}
		got, err := ParseValue(v)
		if err != nil {
			return false
		}
		if (a.User != nil) != v1alpha1.PersonReason(a.Reason) {
			return false
		}
		if a.User == nil {
			return got == 0 && v == ValueOrg
		}
		return got == id && a.User.ID == id && a.User.Login == login && Registered(login, id)
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

// TestParseValueRefusesOutsideGrammarProperty: any string ParseValue
// accepts is in the grammar and is the Value of the id it returns.
func TestParseValueRefusesOutsideGrammarProperty(t *testing.T) {
	cfg := quickConfig(5000)
	cfg.Values = func(args []reflect.Value, r *rand.Rand) {
		args[0] = reflect.ValueOf(randValueish(r))
	}
	prop := func(s string) bool {
		id, err := ParseValue(s)
		if err != nil {
			return true
		}
		want := ValueOrg
		if id > 0 {
			want = "github-id:" + strconv.FormatInt(id, 10)
		}
		return valueRE.MatchString(s) && s == want
	}
	if err := quick.Check(prop, cfg); err != nil {
		t.Error(err)
	}
}

func randLogin(r *rand.Rand) string {
	const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const extra = alnum + "_.-:[]@/ "
	n := r.Intn(70)
	b := make([]byte, n)
	for i := range b {
		if r.Intn(4) == 0 {
			b[i] = extra[r.Intn(len(extra))]
		} else {
			b[i] = alnum[r.Intn(len(alnum))]
		}
	}
	return string(b)
}

func randID(r *rand.Rand) int64 {
	switch r.Intn(5) {
	case 0:
		return 0
	case 1:
		return -r.Int63n(1000)
	case 2:
		return math.MaxInt64 - r.Int63n(3)
	default:
		return r.Int63n(1<<40) + 1
	}
}

func randValueish(r *rand.Rand) string {
	prefixes := []string{"", "org", "github-id:", "github-id:0", "github:", "Github-id:"}
	const digits = "0123456789-+ x"
	b := []byte(prefixes[r.Intn(len(prefixes))])
	for range r.Intn(22) {
		b = append(b, digits[r.Intn(len(digits))])
	}
	return string(b)
}

// TestImportsOnlyStdlibAndAPI keeps the package pure: the standard library
// and api/v1alpha1, nothing else.
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
				t.Errorf("%s imports %s: package attribution imports only the stdlib and api/v1alpha1", name, path)
			}
		}
	}
}
