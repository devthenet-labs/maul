// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

import (
	"reflect"
	"testing"
)

func TestNormalizeSteps(t *testing.T) {
	v := mustValue(t, `{"$present":true}`)
	exit := int32(0)
	cases := []struct {
		name string
		in   []Step
		want []Step
	}{
		{"nil stays nil", nil, nil},
		{"empty stays empty", []Step{}, []Step{}},
		{"defaults made explicit", []Step{{Request: Request{Path: "/"}}},
			[]Step{{Request: Request{Method: "GET", Path: "/"}, Expect: Expect{Status: StatusList{200}}}}},
		{"explicit kept", []Step{{Name: "a", Request: Request{Method: "POST", Path: "/x"},
			Expect: Expect{Status: StatusList{201, 204}}}},
			[]Step{{Name: "a", Request: Request{Method: "POST", Path: "/x"}, Expect: Expect{Status: StatusList{201, 204}}}}},
		{"empty maps become nil", []Step{{Request: Request{Path: "/", Headers: map[string]string{}},
			Expect:  Expect{JSON: map[string]Value{}, Headers: map[string]Value{}, StdoutJSON: map[string]Value{}},
			Capture: map[string]string{}}},
			[]Step{{Request: Request{Method: "GET", Path: "/"}, Expect: Expect{Status: StatusList{200}}}}},
		{"every step", []Step{{Request: Request{Path: "/a"}}, {Request: Request{Method: "HEAD", Path: "/b"},
			Expect: Expect{JSON: map[string]Value{"/x": v}, Exit: &exit}, Capture: map[string]string{"id": "/id"}}},
			[]Step{{Request: Request{Method: "GET", Path: "/a"}, Expect: Expect{Status: StatusList{200}}},
				{Request: Request{Method: "HEAD", Path: "/b"}, Expect: Expect{Status: StatusList{200},
					JSON: map[string]Value{"/x": v}, Exit: &exit}, Capture: map[string]string{"id": "/id"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NormalizeSteps(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("NormalizeSteps = %#v, want %#v", got, c.want)
			}
			if again := NormalizeSteps(got); !reflect.DeepEqual(again, got) {
				t.Errorf("not idempotent: %#v", again)
			}
		})
	}
}

// TestNormalizeStepsCopies: the result shares nothing with its input, and
// the input is left as it was.
func TestNormalizeStepsCopies(t *testing.T) {
	body := mustValue(t, `{"a":1}`)
	exit := int32(3)
	in := []Step{{Request: Request{Path: "/", Headers: map[string]string{"X-A": "1"}, JSON: &body},
		Expect:  Expect{Status: StatusList{200}, JSON: map[string]Value{"/a": body}, Exit: &exit},
		Capture: map[string]string{"id": "/id"}}}
	out := NormalizeSteps(in)
	out[0].Request.Headers["X-A"] = "2"
	*out[0].Request.JSON = mustValue(t, `2`)
	out[0].Expect.Status[0] = 500
	out[0].Expect.JSON["/a"] = mustValue(t, `3`)
	*out[0].Expect.Exit = 4
	out[0].Capture["id"] = "/other"
	switch {
	case in[0].Request.Method != "":
		t.Error("the input's method was defaulted")
	case in[0].Request.Headers["X-A"] != "1", in[0].Request.JSON.String() != `{"a":1}`:
		t.Error("the input's request is shared")
	case in[0].Expect.Status[0] != 200, in[0].Expect.JSON["/a"].String() != `{"a":1}`, *in[0].Expect.Exit != 3:
		t.Error("the input's expectation is shared")
	case in[0].Capture["id"] != "/id":
		t.Error("the input's captures are shared")
	}
}
