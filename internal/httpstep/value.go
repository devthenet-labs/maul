// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
)

// StatusList is a list of HTTP statuses that also decodes from a single
// integer. It always encodes as a list, so a CRD mirror is a plain []int32.
type StatusList []int32

// UnmarshalJSON accepts an integer or a list of integers.
func (s *StatusList) UnmarshalJSON(b []byte) error {
	var one int32
	if err := json.Unmarshal(b, &one); err == nil {
		*s = StatusList{one}
		return nil
	}
	var list []int32
	if err := json.Unmarshal(b, &list); err != nil {
		return errors.New("status must be an integer or a list of integers")
	}
	*s = list
	return nil
}

// UnmarshalYAML accepts an integer or a list of integers, nothing a
// decoder would coerce into one. It uses the function-taking form yaml
// decoders still honour, so this package needs no YAML library.
func (s *StatusList) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	items, isList := raw.([]any)
	if !isList {
		items = []any{raw}
	}
	out := make(StatusList, 0, len(items))
	for _, it := range items {
		n, ok := it.(int)
		if !ok || n < math.MinInt32 || n > math.MaxInt32 {
			return errors.New("status must be an integer or a list of integers")
		}
		out = append(out, int32(n))
	}
	*s = out
	return nil
}

// Value is a JSON value held in canonical form: compact, object keys
// sorted, numbers as written. The zero Value is null.
type Value struct{ raw string }

// ParseValue parses one JSON value into its canonical form.
func ParseValue(b []byte) (Value, error) {
	v, err := decodeJSON(b)
	if err != nil {
		return Value{}, err
	}
	return ValueOf(v)
}

// ValueOf encodes a decoded value (numbers as json.Number) canonically.
func ValueOf(v any) (Value, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return Value{}, err
	}
	return Value{raw: string(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))}, nil
}

// decodeJSON decodes exactly one JSON value, numbers kept as json.Number.
func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

// String is the canonical JSON text.
func (v Value) String() string {
	if v.raw == "" {
		return "null"
	}
	return v.raw
}

// Decoded is v decoded, numbers as json.Number.
func (v Value) Decoded() any {
	out, err := decodeJSON([]byte(v.String()))
	if err != nil {
		return nil // unreachable: raw is always canonical JSON
	}
	return out
}

// AnyString reports whether pred holds for one of v's strings, object keys
// excluded: the strings substitution reaches (substituteValue), so a token
// that is only a key is the same literal in every run.
func (v Value) AnyString(pred func(string) bool) bool {
	found := false
	_ = valueStrings(v.Decoded(), func(s string) error {
		found = found || pred(s)
		return nil
	})
	return found
}

// MarshalJSON writes the canonical text.
func (v Value) MarshalJSON() ([]byte, error) { return []byte(v.String()), nil }

// UnmarshalJSON reads any JSON value.
func (v *Value) UnmarshalJSON(b []byte) error {
	out, err := ParseValue(b)
	if err != nil {
		return err
	}
	*v = out
	return nil
}

// UnmarshalYAML reads any YAML value JSON can hold: string-keyed maps,
// lists and scalars, finite numbers. An unquoted timestamp is refused: it
// would be checked as a time the approved plan never showed.
func (v *Value) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	if hasTime(raw) {
		return errors.New("value holds an unquoted timestamp; quote it")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("value is not JSON: %w", err)
	}
	out, err := ParseValue(b)
	if err != nil {
		return err
	}
	*v = out
	return nil
}

func hasTime(v any) bool {
	switch t := v.(type) {
	case time.Time:
		return true
	case []any:
		return slices.ContainsFunc(t, hasTime)
	case map[string]any:
		for _, e := range t {
			if hasTime(e) {
				return true
			}
		}
	}
	return false
}
