// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package changeset

import (
	"encoding/base64"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"github.com/bitwise-media-group/patchy/internal/envelope"
)

// proxyForms are the egress proxy's host forms a profile run's rules
// forbid (runnerguard.Guard.ProxyHostForms of the chart's Service name).
var proxyForms = []string{"patchy-egress-proxy.patchy.svc.cluster.local", "patchy-egress-proxy.patchy.svc",
	"patchy-egress-proxy.patchy", "http://patchy-egress-proxy"}

func upsert(path, content string) envelope.FileChange {
	return envelope.FileChange{Path: path, Mode: "100644",
		ContentB64: base64.StdEncoding.EncodeToString([]byte(content))}
}

// TestValidateForbidden: on a profile run an upsert whose content or path
// names the proxy, in any case and any of its forms, is refused naming the
// file; the same changeset passes with Forbidden empty, as on every other
// run, and a delete is never judged by its name.
func TestValidateForbidden(t *testing.T) {
	refused := []envelope.FileChange{
		upsert("requirements.txt",
			"--index-url http://patchy-egress-proxy.patchy.svc.cluster.local:8091/r/ca-pypi/simple/\nsix==1.16.0\n"),
		upsert(".npmrc", "registry=HTTP://Patchy-Egress-Proxy.PATCHY.svc:8091/r/ca-npm/\n"),
		upsert("Makefile", "GOPROXY=http://patchy-egress-proxy:8090/go\n"),
		upsert("uv.lock", `url = "http://patchy-egress-proxy.patchy:8091/r/ca-pypi/f/1/six.whl"`),
		upsert("docs/patchy-egress-proxy.patchy.svc.md", "nothing\n"),
	}
	for _, up := range refused {
		cs := &envelope.Changeset{BaseSHA: "abc123", Upserts: []envelope.FileChange{upsert("ok.go", "package ok\n"), up}}
		err := Validate(cs, Rules{Base: "abc123", MaxEntries: 10, Forbidden: proxyForms})
		if err == nil || !strings.Contains(err.Error(), up.Path) || !strings.Contains(err.Error(), "egress proxy") {
			t.Errorf("%s: Validate = %v, want a refusal naming the file", up.Path, err)
		}
		if err := Validate(cs, Rules{Base: "abc123", MaxEntries: 10}); err != nil {
			t.Errorf("%s: without Forbidden Validate = %v, want nil", up.Path, err)
		}
	}
	clean := &envelope.Changeset{BaseSHA: "abc123", Deletes: []string{"patchy-egress-proxy.patchy.svc.md"},
		Upserts: []envelope.FileChange{
			upsert("package-lock.json", `"resolved": "https://acme.jfrog.io/artifactory/api/npm/npm/x/-/x-1.0.0.tgz"`),
			upsert("README.md", "the patchy egress proxy fetches packages\n"),
		}}
	if err := Validate(clean, Rules{Base: "abc123", MaxEntries: 10, Forbidden: proxyForms}); err != nil {
		t.Errorf("a changeset with canonical URLs was refused: %v", err)
	}
}

// TestValidateForbiddenProperty: a changeset none of whose paths or
// contents holds a forbidden form gets the same verdict as with Forbidden
// empty, and adding a forbidden form, in any case, to any upsert's content
// of an otherwise accepted changeset refuses it. Seeded so the gate is
// deterministic.
func TestValidateForbiddenProperty(t *testing.T) {
	alphabet := "abcdefghijklmnopqrstuvwxyz0123456789-._:/ =\n"
	text := func(r *rand.Rand) string {
		b := make([]byte, r.Intn(80))
		for i := range b {
			b[i] = alphabet[r.Intn(len(alphabet))]
		}
		return string(b)
	}
	cfg := &quick.Config{
		MaxCount: 2000,
		Rand:     rand.New(rand.NewSource(20261015)),
		Values: func(args []reflect.Value, r *rand.Rand) {
			n := 1 + r.Intn(4)
			cs := &envelope.Changeset{BaseSHA: "abc123"}
			for range n {
				cs.Upserts = append(cs.Upserts, upsert(genPath(r), text(r)))
			}
			args[0] = reflect.ValueOf(cs)
			args[1] = reflect.ValueOf(r.Intn(n))
			args[2] = reflect.ValueOf(r.Intn(len(proxyForms)))
			args[3] = reflect.ValueOf(r.Int63())
		},
	}
	holds := func(cs *envelope.Changeset, at, form int, seed int64) bool {
		plain := Rules{Base: "abc123", MaxEntries: DefaultMaxEntries}
		profile := plain
		profile.Forbidden = proxyForms
		base := Validate(cs, plain)
		if !mentions(cs, proxyForms) && (Validate(cs, profile) == nil) != (base == nil) {
			return false
		}
		if base != nil {
			return true
		}
		// Plant a form, case-scrambled, in one upsert's content.
		f := []byte(proxyForms[form])
		r := rand.New(rand.NewSource(seed))
		for i := range f {
			if r.Intn(2) == 0 {
				f[i] = []byte(strings.ToUpper(string(f[i])))[0]
			}
		}
		planted := *cs
		planted.Upserts = append([]envelope.FileChange(nil), cs.Upserts...)
		old, _ := base64.StdEncoding.DecodeString(planted.Upserts[at].ContentB64)
		planted.Upserts[at] = upsert(planted.Upserts[at].Path, string(old)+" "+string(f)+":8091/")
		return Validate(&planted, profile) != nil && Validate(&planted, plain) == nil
	}
	if err := quick.Check(holds, cfg); err != nil {
		t.Error(err)
	}
}

// mentions reports whether any upsert's path or content holds a form, as
// the oracle reads it.
func mentions(cs *envelope.Changeset, forms []string) bool {
	for _, up := range cs.Upserts {
		content, _ := base64.StdEncoding.DecodeString(up.ContentB64)
		for _, f := range forms {
			if strings.Contains(strings.ToLower(up.Path), f) || strings.Contains(strings.ToLower(string(content)), f) {
				return true
			}
		}
	}
	return false
}
