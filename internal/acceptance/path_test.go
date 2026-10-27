// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"encoding/json"
	"math/rand"
	"net/url"
	"strings"
	"testing"
)

func TestCheckPath(t *testing.T) {
	good := []string{
		"/", "/api", "/api/battles/42/leaderboard", "/a?b=c&d=e", "/a?", "/a-b_c.d~e/f",
		"/x?q=a%20b", "/x/%41", "/v1/items;matrix=1", "/x?next=/y", "/.well-known/thing", "/a:b",
	}
	bad := []string{
		"", "a", "//", "//host/x", "/a//b", "/a/./b", "/a/../b", "/..", "/.", "/a/..", "/a/.?x",
		"/a@b", "/a\\b", "/a?u=http://x", "/a?x=1\x00", "/a\tb", "/a b", "/a\x7f", "/é", "/a#frag",
		"/a/%2e%2e/b", "/a/%2E/b", "/a%2fb", "/a%2Fb", "/a%5cb", "/a%00", "/a%zz", "/a%4",
		"/" + strings.Repeat("a", MaxPathBytes),
	}
	for _, p := range good {
		if err := CheckPath(p); err != nil {
			t.Errorf("CheckPath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range bad {
		if err := CheckPath(p); err == nil {
			t.Errorf("CheckPath(%q) accepted", p)
		}
	}
}

// TestCheckPathStaysOnHost is the property the tester relies on: any path
// CheckPath admits, joined onto a fixed host the way the tester builds its
// URL, parses back to that host with that path, never another authority.
func TestCheckPathStaysOnHost(t *testing.T) {
	r := rand.New(rand.NewSource(seed))
	alphabet := []byte("/aZ09-._~!$&'()*+,;=:@%?#\\ 2eEfF5c")
	admitted := 0
	for range 20000 {
		n := 1 + r.Intn(12)
		b := make([]byte, 1, 1+n)
		b[0] = '/'
		for range n {
			b = append(b, alphabet[r.Intn(len(alphabet))])
		}
		p := string(b)
		if CheckPath(p) != nil {
			continue
		}
		admitted++
		u, err := url.Parse("http://preview-x:80" + p)
		if err != nil {
			t.Fatalf("admitted %q does not parse: %v", p, err)
		}
		if u.Host != "preview-x:80" || u.User != nil || u.Fragment != "" {
			t.Fatalf("admitted %q left the host: %#v", p, u)
		}
		for _, seg := range strings.Split(u.Path, "/") {
			if seg == "." || seg == ".." {
				t.Fatalf("admitted %q decodes to a dot segment: %q", p, u.Path)
			}
		}
	}
	if admitted < 1000 {
		t.Fatalf("only %d of the corpus admitted; the generator is too narrow", admitted)
	}
}

func TestCheckPointer(t *testing.T) {
	for _, p := range []string{"", "/", "/a", "/a/0", "/a~0b/c~1d", "/ünïcode"} {
		if err := CheckPointer(p); err != nil {
			t.Errorf("CheckPointer(%q) = %v", p, err)
		}
	}
	for _, p := range []string{"a", "/a~", "/a~2", "/a\x00", "/" + strings.Repeat("p", MaxPointerBytes)} {
		if err := CheckPointer(p); err == nil {
			t.Errorf("CheckPointer(%q) accepted", p)
		}
	}
}

func TestResolvePointer(t *testing.T) {
	var doc any
	dec := json.NewDecoder(strings.NewReader(`{"a":{"b/c":[10,{"d~e":"x"}]},"":1,"n":null}`))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		ptr  string
		want string
		ok   bool
	}{
		{"", `{"":1,"a":{"b/c":[10,{"d~e":"x"}]},"n":null}`, true},
		{"/a/b~1c/0", `10`, true},
		{"/a/b~1c/1/d~0e", `"x"`, true},
		{"/", `1`, true},
		{"/n", `null`, true},
		{"/a/b~1c/2", "", false},
		{"/a/b~1c/-", "", false},
		{"/a/b~1c/01", "", false},
		{"/a/b~1c/x", "", false},
		{"/missing", "", false},
		{"/a/b~1c/0/deeper", "", false},
	}
	for _, c := range cases {
		v, ok := resolvePointer(doc, c.ptr)
		if ok != c.ok {
			t.Errorf("resolve %q ok = %v, want %v", c.ptr, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		got, _ := json.Marshal(v)
		if string(got) != c.want {
			t.Errorf("resolve %q = %s, want %s", c.ptr, got, c.want)
		}
	}
}
