// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"maps"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestParseNodeSelector(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    map[string]string
		wantErr string
	}{
		{raw: "", want: nil},
		{raw: "  ", want: nil},
		{raw: "{}", want: map[string]string{}},
		{raw: `{"node-role.example.com/preview":"true"}`, want: map[string]string{"node-role.example.com/preview": "true"}},
		{raw: `{"a":"b","c":"d"}`, want: map[string]string{"a": "b", "c": "d"}},
		{raw: "{", wantErr: "preview node selector"},
		{raw: `{"a":1}`, wantErr: "preview node selector"},
		{raw: `["a"]`, wantErr: "preview node selector"},
		{raw: `{"a":"b"} {}`, wantErr: "trailing data"},
	} {
		got, err := ParseNodeSelector(tc.raw)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("ParseNodeSelector(%q) error = %v, want %q", tc.raw, err, tc.wantErr)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseNodeSelector(%q) = %v, %v; want %v", tc.raw, got, err, tc.want)
		}
	}
}

// TestNodeIsolationValidate: required needs a selector and a taint key,
// none needs neither, and every key and value must be a label's.
func TestNodeIsolationValidate(t *testing.T) {
	generic := func(s *Settings) {
		s.NodePool, s.NodeClass = "", ""
		s.NodeSelector = map[string]string{"node-role.example.com/preview": "true"}
	}
	for _, tc := range []struct {
		name    string
		edit    func(*Settings)
		wantErr string
	}{
		{name: "EKS pool and class", edit: func(*Settings) {}},
		{name: "explicit required", edit: func(s *Settings) { s.NodeIsolation = NodeIsolationRequired }},
		{name: "generic selector", edit: generic},
		{name: "generic selector beside the EKS keys", edit: func(s *Settings) {
			s.NodeSelector = map[string]string{"node-role.example.com/preview": "true"}
		}},
		{name: "selector repeating the pool", edit: func(s *Settings) {
			s.NodeSelector = map[string]string{nodePoolLabel: s.NodePool}
		}},
		{name: "pool alone", edit: func(s *Settings) { s.NodeClass = "" }},
		{name: "selector contradicting the pool", edit: func(s *Settings) {
			s.NodeSelector = map[string]string{nodePoolLabel: "other"}
		}, wantErr: "contradicts"},
		{name: "selector contradicting the class", edit: func(s *Settings) {
			s.NodeSelector = map[string]string{nodeClassLabel: "other"}
		}, wantErr: "contradicts"},
		{name: "selector without a taint", edit: func(s *Settings) { generic(s); s.TaintKey = "" },
			wantErr: "node isolation is required"},
		{name: "pool and class without a taint", edit: func(s *Settings) { s.TaintKey = "" },
			wantErr: "node isolation is required"},
		{name: "taint without a selector", edit: func(s *Settings) { s.NodePool, s.NodeClass = "", "" },
			wantErr: "node isolation is required"},
		{name: "nothing, required", edit: func(s *Settings) { s.NodePool, s.NodeClass, s.TaintKey = "", "", "" },
			wantErr: "node isolation is required"},
		{name: "nothing, none", edit: func(s *Settings) {
			s.NodePool, s.NodeClass, s.TaintKey, s.NodeIsolation = "", "", "", NodeIsolationNone
		}},
		{name: "none with a selector", edit: func(s *Settings) {
			generic(s)
			s.TaintKey, s.NodeIsolation = "", NodeIsolationNone
		}, wantErr: "node isolation is none"},
		{name: "none with a taint", edit: func(s *Settings) {
			s.NodePool, s.NodeClass, s.NodeIsolation = "", "", NodeIsolationNone
		}, wantErr: "node isolation is none"},
		{name: "unknown mode", edit: func(s *Settings) { s.NodeIsolation = "optional" }, wantErr: "neither"},
		{name: "bad key", edit: func(s *Settings) {
			s.NodeSelector = map[string]string{"bad key": "x"}
		}, wantErr: "node selector key"},
		{name: "bad value", edit: func(s *Settings) {
			s.NodeSelector = map[string]string{"preview": "not a value"}
		}, wantErr: "node selector value"},
		{name: "bad pool", edit: func(s *Settings) { s.NodePool = "-pool" }, wantErr: "node selector value"},
		{name: "bad taint key", edit: func(s *Settings) { s.TaintKey = "a/b/c" }, wantErr: "taint key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testSettings()
			tc.edit(&s)
			err := s.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want ok", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

// TestNodeIsolationRender: a generic selector renders as given beside the
// EKS keys the pool and class fill, and none renders no selector and no
// toleration.
func TestNodeIsolationRender(t *testing.T) {
	_, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	s := testSettings()
	s.NodeSelector = map[string]string{"node-role.example.com/preview": "true"}
	pod := s.deployment(p, 0, 0).Spec.Template.Spec
	want := map[string]string{"node-role.example.com/preview": "true", nodePoolLabel: "maul-preview",
		nodeClassLabel: "maul-preview"}
	if !maps.Equal(pod.NodeSelector, want) {
		t.Errorf("node selector = %v, want %v", pod.NodeSelector, want)
	}
	s.NodePool, s.NodeClass = "", ""
	pod = s.deployment(p, 0, 0).Spec.Template.Spec
	if !maps.Equal(pod.NodeSelector, s.NodeSelector) || len(pod.Tolerations) != 1 ||
		pod.Tolerations[0].Key != s.TaintKey || pod.Tolerations[0].Effect != corev1.TaintEffectNoExecute {
		t.Errorf("generic isolation renders %v and %v", pod.NodeSelector, pod.Tolerations)
	}
	s = testSettings()
	s.NodePool, s.NodeClass, s.TaintKey, s.NodeIsolation = "", "", "", NodeIsolationNone
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	pod = s.deployment(p, 0, 0).Spec.Template.Spec
	if pod.NodeSelector != nil || pod.Tolerations != nil {
		t.Errorf("isolation none renders %v and %v", pod.NodeSelector, pod.Tolerations)
	}
}

// isolationCase is random node isolation settings drawn from a few valid and
// invalid keys and values.
type isolationCase struct{ Settings Settings }

func (isolationCase) Generate(r *rand.Rand, _ int) reflect.Value {
	pick := func(xs ...string) string { return xs[r.Intn(len(xs))] }
	s := testSettings()
	s.NodePool = pick("", "", "maul-preview", "other", "-bad")
	s.NodeClass = pick("", "", "maul-preview", "other")
	s.TaintKey = pick("", "", "maul.devthe.net/preview-only", "preview", "a/b/c")
	s.NodeIsolation = pick("", NodeIsolationRequired, NodeIsolationNone, "optional")
	s.NodeSelector = nil
	for range r.Intn(3) {
		if s.NodeSelector == nil {
			s.NodeSelector = map[string]string{}
		}
		s.NodeSelector[pick("node-role.example.com/preview", nodePoolLabel, nodeClassLabel, "zone", "bad key")] =
			pick("true", "maul-preview", "other", "", "not a value")
	}
	return reflect.ValueOf(isolationCase{Settings: s})
}

// TestNodeIsolationProperty: whatever the settings, Validate accepts them
// only if the Pod they render is isolated as their mode says: required puts
// it on a non-empty selector holding every configured key and value with
// exactly the one NoExecute toleration; none renders neither. Seeded.
func TestNodeIsolationProperty(t *testing.T) {
	_, p := testPreview("demo-1", time.Date(2026, 10, 1, 11, 0, 0, 0, time.UTC))
	cfg := &quick.Config{MaxCount: 4000, Rand: rand.New(rand.NewSource(20261016))}
	check := func(c isolationCase) bool {
		s := c.Settings
		if s.Validate() != nil {
			return true
		}
		pod := s.deployment(p, 0, 0).Spec.Template.Spec
		if s.NodeIsolation == NodeIsolationNone {
			return pod.NodeSelector == nil && pod.Tolerations == nil
		}
		for k, v := range s.NodeSelector {
			if pod.NodeSelector[k] != v {
				return false
			}
		}
		if s.NodePool != "" && pod.NodeSelector[nodePoolLabel] != s.NodePool ||
			s.NodeClass != "" && pod.NodeSelector[nodeClassLabel] != s.NodeClass {
			return false
		}
		ok := len(pod.NodeSelector) > 0 && len(pod.Tolerations) == 1 && pod.Tolerations[0].Key == s.TaintKey &&
			s.TaintKey != "" && pod.Tolerations[0].Value == "true" &&
			pod.Tolerations[0].Effect == corev1.TaintEffectNoExecute
		if !ok {
			t.Logf("accepted %+v renders selector %v, tolerations %v", s, pod.NodeSelector, pod.Tolerations)
		}
		return ok
	}
	if err := quick.Check(check, cfg); err != nil {
		t.Error(err)
	}
}
