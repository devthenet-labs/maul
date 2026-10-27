// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// MaxBodyBytes bounds a request body, after substitution.
const MaxBodyBytes = 2 << 10

// ErrCaptureMissing is a {{capture.<name>}} whose step captured nothing.
var ErrCaptureMissing = errors.New("capture missing")

// captureName is a capture's name, as declared and as referenced.
var captureName = regexp.MustCompile(`^[a-z][a-z0-9]{0,15}$`)

// placeholderRef is the text between "{{" and "}}".
func placeholderRef(inner string) (capture string, err error) {
	if inner == "run" {
		return "", nil
	}
	if name, ok := strings.CutPrefix(inner, "capture."); ok && captureName.MatchString(name) {
		return name, nil
	}
	return "", fmt.Errorf("unknown placeholder {{%s}}: only {{run}} and {{capture.<name>}} exist", inner)
}

// scan walks s, calling lit for literal text and ref for each placeholder
// (capture "" being {{run}}). A "{{" that does not open a valid placeholder
// is an error, so text cannot smuggle one past the grammar.
func scan(s string, lit func(string), ref func(capture string) error) error {
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			lit(s)
			return nil
		}
		lit(s[:i])
		end := strings.Index(s[i+2:], "}}")
		if end < 0 {
			return errors.New(`unclosed "{{"`)
		}
		name, err := placeholderRef(s[i+2 : i+2+end])
		if err != nil {
			return err
		}
		if err := ref(name); err != nil {
			return err
		}
		s = s[i+2+end+2:]
	}
}

// placeholders lists the captures s references, in order, repeats kept,
// or the first grammar error.
func placeholders(s string) ([]string, error) {
	var out []string
	err := scan(s, func(string) {}, func(c string) error {
		if c != "" {
			out = append(out, c)
		}
		return nil
	})
	return out, err
}

// Substitute replaces {{run}} with run and each {{capture.<name>}} with that
// capture, once: substituted text is never expanded again. Any other "{{…}}"
// is an error, as is a capture caps lacks (ErrCaptureMissing).
func Substitute(s, run string, caps map[string]string) (string, error) {
	return substituteWith(s, run, caps, func(v string) string { return v })
}

// substituteWith is Substitute with captures passed through escape, the
// run token being maul's own and safe in every context.
func substituteWith(s, run string, caps map[string]string, escape func(string) string) (string, error) {
	var b strings.Builder
	err := scan(s, func(t string) { b.WriteString(t) }, func(c string) error {
		if c == "" {
			b.WriteString(run)
			return nil
		}
		v, ok := caps[c]
		if !ok {
			return fmt.Errorf("{{capture.%s}}: %w", c, ErrCaptureMissing)
		}
		b.WriteString(escape(v))
		return nil
	})
	return b.String(), err
}

// BuiltRequest is a step's request as the tester sends it.
type BuiltRequest struct {
	Method string
	// Path is the escaped path, RawQuery the escaped query: the request
	// target is Path, then "?" and RawQuery when the template had one.
	Path     string
	RawQuery string
	hasQuery bool
	// Headers are the step's headers, Content-Type added for a JSON body.
	Headers map[string]string
	Body    []byte
}

// Target is the request target as sent: path and query, as
// StepObservation.Path records it.
func (b BuiltRequest) Target() string {
	if b.hasQuery {
		return b.Path + "?" + b.RawQuery
	}
	return b.Path
}

// BuildRequest substitutes run and the captures into st's request. A
// capture is escaped into its context (a path segment, a query value) and
// the result re-checked with CheckPath, so a value an application returned
// can never add a segment, a parameter or a dot segment; a header value
// holding a control character is refused; the body is held to MaxBodyBytes.
// A missing capture is ErrCaptureMissing.
func BuildRequest(st Step, run string, caps map[string]string) (BuiltRequest, error) {
	st = normalize(Criterion{Kind: KindHTTP, Steps: []Step{st}}).Steps[0]
	out := BuiltRequest{Method: st.Request.Method}
	tmplPath, tmplQuery, hasQuery := strings.Cut(st.Request.Path, "?")
	var err error
	if out.Path, err = substituteWith(tmplPath, run, caps, url.PathEscape); err != nil {
		return BuiltRequest{}, err
	}
	if out.RawQuery, err = substituteWith(tmplQuery, run, caps, url.QueryEscape); err != nil {
		return BuiltRequest{}, err
	}
	out.hasQuery = hasQuery
	if err := CheckPath(out.Target()); err != nil {
		return BuiltRequest{}, err
	}
	if out.Headers, err = buildHeaders(st.Request.Headers, run, caps); err != nil {
		return BuiltRequest{}, err
	}
	if out.Body, err = buildBody(st.Request, run, caps); err != nil {
		return BuiltRequest{}, err
	}
	if st.Request.JSON != nil && !hasHeader(out.Headers, "Content-Type") {
		if out.Headers == nil {
			out.Headers = map[string]string{}
		}
		out.Headers["Content-Type"] = "application/json"
	}
	return out, nil
}

func buildHeaders(hs map[string]string, run string, caps map[string]string) (map[string]string, error) {
	if len(hs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(hs))
	for name, tmpl := range hs {
		v, err := Substitute(tmpl, run, caps)
		if err != nil {
			return nil, err
		}
		if err := checkHeaderValue(v); err != nil {
			return nil, fmt.Errorf("header %s: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

func buildBody(r Request, run string, caps map[string]string) ([]byte, error) {
	var body string
	switch {
	case r.JSON != nil:
		v, err := substituteValue(*r.JSON, run, caps)
		if err != nil {
			return nil, err
		}
		body = v.String()
	case r.Body != "":
		var err error
		if body, err = Substitute(r.Body, run, caps); err != nil {
			return nil, err
		}
	default:
		return nil, nil
	}
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("body is over %d bytes", MaxBodyBytes)
	}
	return []byte(body), nil
}

// substituteValue substitutes into every string of a JSON value (object
// keys are left alone); the result is re-encoded, so a capture can never
// break the value's structure.
func substituteValue(v Value, run string, caps map[string]string) (Value, error) {
	out, err := substituteAny(v.decoded(), run, caps)
	if err != nil {
		return Value{}, err
	}
	return valueOf(out)
}

func substituteAny(v any, run string, caps map[string]string) (any, error) {
	switch t := v.(type) {
	case string:
		return Substitute(t, run, caps)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			var err error
			if out[i], err = substituteAny(e, run, caps); err != nil {
				return nil, err
			}
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			var err error
			if out[k], err = substituteAny(e, run, caps); err != nil {
				return nil, err
			}
		}
		return out, nil
	default:
		return v, nil
	}
}

// valueStrings calls f on every string of a JSON value, keys excluded.
func valueStrings(v any, f func(string) error) error {
	switch t := v.(type) {
	case string:
		return f(t)
	case []any:
		for _, e := range t {
			if err := valueStrings(e, f); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, e := range t {
			if err := valueStrings(e, f); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasHeader(hs map[string]string, name string) bool {
	for k := range hs {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}
