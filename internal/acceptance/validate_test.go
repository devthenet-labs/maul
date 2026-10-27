// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func mustValue(t *testing.T, src string) Value {
	t.Helper()
	v, err := ParseValue([]byte(src))
	if err != nil {
		t.Fatalf("ParseValue(%s): %v", src, err)
	}
	return v
}

func validHTTP() Criterion {
	return Criterion{
		ID: "ok", Kind: KindHTTP, Statement: "it works", Target: "api",
		Steps: []Step{{
			Name:    "first",
			Request: Request{Method: "POST", Path: "/api/x?y=1", Headers: map[string]string{"X-Trace": "maul-{{run}}"}},
			Expect:  Expect{Status: StatusList{201}},
			Capture: map[string]string{"id": "/id"},
		}, {
			Request: Request{Method: "GET", Path: "/api/x/{{capture.id}}"},
			Expect:  Expect{Status: StatusList{200}},
		}},
	}
}

func TestValidateAcceptsValid(t *testing.T) {
	cmd := Criterion{ID: "cmd", Kind: KindCommand, Statement: "s", In: &Where{Repository: "api"},
		Argv: []string{"go", "test", "./..."}, Expect: &Expect{Exit: new(int32)}, Timeout: "10m"}
	manual := Criterion{ID: "m", Kind: KindManual, Statement: "s", Reason: "only a human"}
	if err := Validate([]Criterion{validHTTP(), cmd, manual}); err != nil {
		t.Fatalf("Validate = %v", err)
	}
	if err := Validate(nil); err != nil {
		t.Errorf("Validate(nil) = %v", err)
	}
}

// TestValidateRefuses mutates a valid criterion one rule at a time; each
// mutation must be refused with a CriterionError naming the criterion.
func TestValidateRefuses(t *testing.T) {
	cases := map[string]func(t *testing.T, c *Criterion){
		"id grammar":      func(_ *testing.T, c *Criterion) { c.ID = "Bad" },
		"id too long":     func(_ *testing.T, c *Criterion) { c.ID = "a" + strings.Repeat("b", 32) },
		"kind unknown":    func(_ *testing.T, c *Criterion) { c.Kind = "smoke" },
		"kind browser":    func(_ *testing.T, c *Criterion) { c.Kind = KindBrowser },
		"kind agent":      func(_ *testing.T, c *Criterion) { c.Kind = KindAgent },
		"statement empty": func(_ *testing.T, c *Criterion) { c.Statement = " " },
		"statement long":  func(_ *testing.T, c *Criterion) { c.Statement = strings.Repeat("é", 201) },
		"statement lines": func(_ *testing.T, c *Criterion) { c.Statement = "a\nb" },
		"reason long":     func(_ *testing.T, c *Criterion) { c.Reason = strings.Repeat("r", 121) },
		"target missing":  func(_ *testing.T, c *Criterion) { c.Target = "" },
		"target grammar":  func(_ *testing.T, c *Criterion) { c.Target = "API" },
		"target too long": func(_ *testing.T, c *Criterion) { c.Target = strings.Repeat("a", 17) },
		"no steps":        func(_ *testing.T, c *Criterion) { c.Steps = nil },
		"nine steps":      func(_ *testing.T, c *Criterion) { c.Steps = append(c.Steps, make([]Step, 7)...) },
		"shorthand+steps": func(_ *testing.T, c *Criterion) { c.Request = &Request{Path: "/"} },
		"argv on http":    func(_ *testing.T, c *Criterion) { c.Argv = []string{"x"} },
		"in on http":      func(_ *testing.T, c *Criterion) { c.In = &Where{Component: "api"} },
		"timeout on http": func(_ *testing.T, c *Criterion) { c.Timeout = "1m" },
		"command expect":  func(_ *testing.T, c *Criterion) { c.Steps[0].Expect.Exit = new(int32) },
		"stdout on http":  func(_ *testing.T, c *Criterion) { c.Steps[0].Expect.StdoutContains = "x" },
		"step name":       func(_ *testing.T, c *Criterion) { c.Steps[0].Name = "Bad Name" },
		"step names dup":  func(_ *testing.T, c *Criterion) { c.Steps[1].Name = "first" },
		"method":          func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Method = "TRACE" },
		"method case":     func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Method = "post" },
		"path relative":   func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Path = "api" },
		"path host":       func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Path = "//evil/x" },
		"path scheme":     func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Path = "/x?u=http://evil" },
		"path userinfo":   func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Path = "/a@b" },
		"path dotdot":     func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Path = "/a/../b" },
		"path long":       func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Path = "/" + strings.Repeat("a", 512) },
		"headers count":   func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Headers = manyHeaders(9) },
		"header name":     func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Headers = map[string]string{"X_Y": "v"} },
		"header host":     func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Headers = map[string]string{"host": "v"} },
		"header cookie":   func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Headers = map[string]string{"Cookie": "v"} },
		"header te": func(_ *testing.T, c *Criterion) {
			c.Steps[0].Request.Headers = map[string]string{"transfer-encoding": "v"}
		},
		"header value url": func(_ *testing.T, c *Criterion) {
			c.Steps[0].Request.Headers = map[string]string{"X": "http://e"}
		},
		"header value crlf": func(_ *testing.T, c *Criterion) {
			c.Steps[0].Request.Headers = map[string]string{"X": "a\r\nHost: e"}
		},
		"header value long": func(_ *testing.T, c *Criterion) {
			c.Steps[0].Request.Headers = map[string]string{"X": strings.Repeat("v", 257)}
		},
		"json and body": func(t *testing.T, c *Criterion) {
			v := mustValue(t, `{}`)
			c.Steps[0].Request.JSON, c.Steps[0].Request.Body = &v, "x"
		},
		"body large": func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Body = strings.Repeat("b", 2049) },
		"json large": func(t *testing.T, c *Criterion) {
			v := mustValue(t, fmt.Sprintf("%q", strings.Repeat("j", 2048)))
			c.Steps[0].Request.JSON = &v
		},
		"status range":   func(_ *testing.T, c *Criterion) { c.Steps[0].Expect.Status = StatusList{99} },
		"status range 2": func(_ *testing.T, c *Criterion) { c.Steps[0].Expect.Status = StatusList{600} },
		"json pointers":  func(t *testing.T, c *Criterion) { c.Steps[0].Expect.JSON = manyPointers(t, 9) },
		"json pointer grammar": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"id": mustValue(t, `1`)}
		},
		"json pointer escape": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a~2": mustValue(t, `1`)}
		},
		"matcher unknown": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a": mustValue(t, `{"$regex":"x"}`)}
		},
		"matcher two": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a": mustValue(t, `{"$present":true,"$contains":"x"}`)}
		},
		"matcher nested dollar": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a": mustValue(t, `{"b":{"$present":true}}`)}
		},
		"present false": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a": mustValue(t, `{"$present":false}`)}
		},
		"contains number": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a": mustValue(t, `{"$contains":1}`)}
		},
		"approx no tolerance": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a": mustValue(t, `{"$approx":1}`)}
		},
		"approx negative tolerance": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a": mustValue(t, `{"$approx":1,"$tolerance":-1}`)}
		},
		"approx strings": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a": mustValue(t, `{"$approx":["1"],"$tolerance":1}`)}
		},
		"tolerance alone": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a": mustValue(t, `{"$tolerance":1}`)}
		},
		"order empty": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a": mustValue(t, `{"$order":[]}`)}
		},
		"order by grammar": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a": mustValue(t, `{"$order":[1],"$by":"id"}`)}
		},
		"by alone": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a": mustValue(t, `{"$by":"/id"}`)}
		},
		"literal large": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.JSON = map[string]Value{"/a": mustValue(t, fmt.Sprintf("%q", strings.Repeat("l", 300)))}
		},
		"expect headers count": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.Headers = map[string]Value{}
			for i := range 5 {
				c.Steps[0].Expect.Headers[fmt.Sprintf("X-%d", i)] = mustValue(t, `"v"`)
			}
		},
		"expect header number": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.Headers = map[string]Value{"X": mustValue(t, `1`)}
		},
		"expect header matcher": func(t *testing.T, c *Criterion) {
			c.Steps[0].Expect.Headers = map[string]Value{"X": mustValue(t, `{"$present":true}`)}
		},
		"body contains long": func(_ *testing.T, c *Criterion) { c.Steps[0].Expect.BodyContains = strings.Repeat("c", 201) },
		"captures count": func(_ *testing.T, c *Criterion) {
			c.Steps[0].Capture = map[string]string{"a": "/a", "b": "/b", "c": "/c", "d": "/d", "e": "/e"}
		},
		"capture name":     func(_ *testing.T, c *Criterion) { c.Steps[0].Capture = map[string]string{"Id": "/id"} },
		"capture pointer":  func(_ *testing.T, c *Criterion) { c.Steps[0].Capture = map[string]string{"id": "id"} },
		"capture reused":   func(_ *testing.T, c *Criterion) { c.Steps[1].Capture = map[string]string{"id": "/x"} },
		"capture forward":  func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Path = "/x/{{capture.id}}" },
		"capture unknown":  func(_ *testing.T, c *Criterion) { c.Steps[1].Request.Path = "/x/{{capture.nope}}" },
		"placeholder":      func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Path = "/x/{{env.HOME}}" },
		"placeholder open": func(_ *testing.T, c *Criterion) { c.Steps[0].Request.Path = "/x/{{run" },
		"placeholder in body": func(_ *testing.T, c *Criterion) {
			c.Steps[0].Request.Body = "{{secret}}"
		},
		"placeholder in expect": func(t *testing.T, c *Criterion) {
			c.Steps[1].Expect.JSON = map[string]Value{"/a": mustValue(t, `"{{capture.later}}"`)}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := validHTTP()
			mutate(t, &c)
			err := Validate([]Criterion{c})
			var ce *CriterionError
			if !errors.As(err, &ce) {
				t.Fatalf("Validate = %v, want a *CriterionError", err)
			}
			if ce.ID != c.ID {
				t.Errorf("error names %q, want %q", ce.ID, c.ID)
			}
		})
	}
}

func TestValidateRefusesCommandAndManual(t *testing.T) {
	cmd := func() Criterion {
		return Criterion{ID: "cmd", Kind: KindCommand, Statement: "s", In: &Where{Component: "api"},
			Argv: []string{"/bin/x"}, Expect: &Expect{StdoutContains: "ok"}}
	}
	manual := func() Criterion { return Criterion{ID: "m", Kind: KindManual, Statement: "s"} }
	cases := map[string]Criterion{}
	add := func(name string, base func() Criterion, mutate func(c *Criterion)) {
		c := base()
		mutate(&c)
		cases[name] = c
	}
	add("cmd no in", cmd, func(c *Criterion) { c.In = nil })
	add("cmd both in", cmd, func(c *Criterion) { c.In.Repository = "api" })
	add("cmd in grammar", cmd, func(c *Criterion) { c.In.Component = "A" })
	add("cmd no argv", cmd, func(c *Criterion) { c.Argv = nil })
	add("cmd argv count", cmd, func(c *Criterion) { c.Argv = make([]string, 17) })
	add("cmd argv long", cmd, func(c *Criterion) { c.Argv = []string{strings.Repeat("a", 201)} })
	add("cmd argv control", cmd, func(c *Criterion) { c.Argv = []string{"a\x00b"} })
	add("cmd argv empty", cmd, func(c *Criterion) { c.Argv = []string{""} })
	add("cmd timeout long", cmd, func(c *Criterion) { c.Timeout = "11m" })
	add("cmd timeout bad", cmd, func(c *Criterion) { c.Timeout = "soon" })
	add("cmd timeout zero", cmd, func(c *Criterion) { c.Timeout = "0s" })
	add("cmd status", cmd, func(c *Criterion) { c.Expect.Status = StatusList{200} })
	add("cmd steps", cmd, func(c *Criterion) { c.Steps = []Step{{}} })
	add("cmd target", cmd, func(c *Criterion) { c.Target = "api" })
	add("cmd stdout long", cmd, func(c *Criterion) { c.Expect.StdoutContains = strings.Repeat("s", 201) })
	add("cmd no expect", cmd, func(c *Criterion) { c.Expect = nil })
	add("manual target", manual, func(c *Criterion) { c.Target = "api" })
	add("manual request", manual, func(c *Criterion) { c.Request = &Request{Path: "/"} })
	add("manual argv", manual, func(c *Criterion) { c.Argv = []string{"x"} })
	add("manual expect", manual, func(c *Criterion) { c.Expect = &Expect{} })
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var ce *CriterionError
			if err := Validate([]Criterion{c}); !errors.As(err, &ce) || ce.ID != c.ID {
				t.Fatalf("Validate = %v, want a *CriterionError for %q", err, c.ID)
			}
		})
	}
}

func TestValidateListBounds(t *testing.T) {
	manual := func(i int) Criterion {
		return Criterion{ID: fmt.Sprintf("m%d", i), Kind: KindManual, Statement: "s"}
	}
	many := make([]Criterion, 0, MaxCriteria+1)
	for i := range MaxCriteria + 1 {
		many = append(many, manual(i))
	}
	if err := Validate(many); err == nil {
		t.Error("17 criteria accepted")
	}
	if err := Validate(many[:MaxCriteria]); err != nil {
		t.Errorf("16 criteria refused: %v", err)
	}
	if err := Validate([]Criterion{manual(1), manual(1)}); err == nil {
		t.Error("duplicate ids accepted")
	}
	// 48 steps in total, across criteria.
	steps := make([]Criterion, 0, 7)
	for i := range 7 {
		c := Criterion{ID: fmt.Sprintf("h%d", i), Kind: KindHTTP, Statement: "s", Target: "api"}
		for range 7 {
			c.Steps = append(c.Steps, Step{Request: Request{Path: "/"}})
		}
		steps = append(steps, c)
	}
	if err := Validate(steps); err == nil {
		t.Error("49 steps accepted")
	}
	if err := Validate(steps[:6]); err != nil {
		t.Errorf("42 steps refused: %v", err)
	}
}

func manyHeaders(n int) map[string]string {
	h := map[string]string{}
	for i := range n {
		h[fmt.Sprintf("X-H%d", i)] = "v"
	}
	return h
}

func manyPointers(t *testing.T, n int) map[string]Value {
	m := map[string]Value{}
	for i := range n {
		m[fmt.Sprintf("/p%d", i)] = mustValue(t, `1`)
	}
	return m
}
