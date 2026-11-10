// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package acceptance

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/devthenet-labs/maul/internal/httpstep"
)

// The grammar's bounds (SPEC §3.1).
const (
	MaxCriteria       = 16
	MaxSteps          = 48
	MaxStepsPerHTTP   = 8
	MaxStatementChars = 200
	MaxReasonChars    = 120
	MaxArgs           = 16
	MaxArgChars       = 200
	MaxCommandTimeout = 10 * time.Minute
)

// idPattern is a criterion's id and an http step's name.
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// CriterionError is a criterion the grammar refuses. It is the only error
// Validate returns for a criterion, and the only static error there is: a
// failure observed at run time is a fail verdict, never an error.
type CriterionError struct {
	// ID is the refused criterion's id, as written.
	ID string
	// Reason says what is wrong, in maul's words.
	Reason string
}

func (e *CriterionError) Error() string {
	return fmt.Sprintf("acceptance criterion %q: %s", e.ID, e.Reason)
}

// Validate checks cs against the static grammar: at most MaxCriteria
// criteria with unique ids, at most MaxSteps http steps in all and a
// worst-case tester output (WireBound) within MaxWireBytes, each criterion
// valid for its kind. A refused criterion is a *CriterionError; a
// list-level refusal names the criterion that crossed the bound.
func Validate(cs []Criterion) error {
	if len(cs) > MaxCriteria {
		return &CriterionError{ID: cs[MaxCriteria].ID, Reason: fmt.Sprintf("more than %d criteria", MaxCriteria)}
	}
	seen := map[string]bool{}
	steps := 0
	wire := summaryWireBytes()
	for _, c := range cs {
		if seen[c.ID] {
			return &CriterionError{ID: c.ID, Reason: "id is used twice"}
		}
		seen[c.ID] = true
		if err := validateCriterion(c); err != nil {
			return &CriterionError{ID: c.ID, Reason: err.Error()}
		}
		steps += len(normalize(c).Steps)
		if steps > MaxSteps {
			return &CriterionError{ID: c.ID, Reason: fmt.Sprintf("more than %d steps in all", MaxSteps)}
		}
		if wire += criterionWireBound(c); wire > MaxWireBytes {
			return &CriterionError{ID: c.ID, Reason: fmt.Sprintf("the plan's observations could reach %d bytes, "+
				"over the %d-byte budget the tester's output must fit (MaxWireBytes); use fewer steps, "+
				"expected pointers, headers or captures", wire, MaxWireBytes)}
		}
	}
	return nil
}

func validateCriterion(c Criterion) error {
	if !idPattern.MatchString(c.ID) {
		return errors.New(`id must match ^[a-z][a-z0-9-]{0,31}$`)
	}
	if err := httpstep.CheckLine("statement", c.Statement, MaxStatementChars); err != nil {
		return err
	}
	if strings.TrimSpace(c.Statement) == "" {
		return errors.New("statement is required")
	}
	if err := httpstep.CheckLine("reason", c.Reason, MaxReasonChars); err != nil {
		return err
	}
	switch c.Kind {
	case KindHTTP:
		return validateHTTP(c)
	case KindCommand:
		return validateCommand(c)
	case KindManual:
		return validateManual(c)
	case KindBrowser, KindAgent:
		return fmt.Errorf("kind %s is not supported yet", c.Kind)
	default:
		return fmt.Errorf("kind %q is not one of http, command, manual", c.Kind)
	}
}
func validateManual(c Criterion) error {
	if c.Target != "" || c.Request != nil || c.Expect != nil || c.Capture != nil || c.Steps != nil ||
		c.In != nil || c.Argv != nil || c.Timeout != "" {
		return errors.New("a manual criterion has only an id, a statement and a reason")
	}
	return nil
}

func validateCommand(c Criterion) error {
	if c.Target != "" || c.Request != nil || c.Capture != nil || c.Steps != nil {
		return errors.New("a command criterion has in, argv, expect and timeout, not target, request or steps")
	}
	if err := validateWhere(c.In); err != nil {
		return err
	}
	if err := validateArgv(c.Argv); err != nil {
		return err
	}
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil || d <= 0 || d > MaxCommandTimeout {
			return fmt.Errorf("timeout %q must be a duration in (0, %s]", c.Timeout, MaxCommandTimeout)
		}
	}
	if c.Expect == nil {
		return errors.New("a command criterion needs an expect")
	}
	return validateCommandExpect(*c.Expect)
}

func validateWhere(in *Where) error {
	if in == nil || (in.Component == "") == (in.Repository == "") {
		return errors.New("in names exactly one of a component or a repository")
	}
	return checkKey("in", in.Component+in.Repository)
}

func checkKey(field, key string) error {
	if len(key) > httpstep.MaxKeyLength || !httpstep.KeyPattern.MatchString(key) {
		return fmt.Errorf("%s %q is not a repository key (a DNS label of at most %d characters)", field, key,
			httpstep.MaxKeyLength)
	}
	return nil
}

func validateArgv(argv []string) error {
	if len(argv) == 0 || len(argv) > MaxArgs {
		return fmt.Errorf("argv holds 1 to %d arguments", MaxArgs)
	}
	for _, a := range argv {
		if err := httpstep.CheckLine("an argument", a, MaxArgChars); err != nil {
			return err
		}
	}
	if argv[0] == "" {
		return errors.New("argv[0] is empty")
	}
	return nil
}

func validateCommandExpect(e httpstep.Expect) error {
	if e.Status != nil || e.JSON != nil || e.Headers != nil || e.BodyContains != "" {
		return errors.New("a command expect has only exit, stdoutContains and stdoutJSON")
	}
	if err := httpstep.CheckLine("stdoutContains", e.StdoutContains, httpstep.MaxContainsChars); err != nil {
		return err
	}
	return httpstep.ValidatePointers("stdoutJSON", e.StdoutJSON, nil)
}

func validateHTTP(c Criterion) error {
	if c.In != nil || c.Argv != nil || c.Timeout != "" {
		return errors.New("an http criterion has target and steps, not in, argv or timeout")
	}
	if err := checkKey("target", c.Target); err != nil {
		return err
	}
	shorthand := c.Request != nil || c.Expect != nil || c.Capture != nil
	if shorthand && c.Steps != nil {
		return errors.New("write either steps or one request/expect pair, not both")
	}
	if shorthand && c.Request == nil {
		return errors.New("a request is required")
	}
	steps := normalize(c).Steps
	if len(steps) == 0 || len(steps) > MaxStepsPerHTTP {
		return fmt.Errorf("an http criterion has 1 to %d steps", MaxStepsPerHTTP)
	}
	captured := map[string]bool{}
	names := map[string]bool{}
	for i, st := range steps {
		if st.Name != "" {
			if !idPattern.MatchString(st.Name) || names[st.Name] {
				return fmt.Errorf("step %d: name %q is not a unique ^[a-z][a-z0-9-]{0,31}$", i+1, st.Name)
			}
			names[st.Name] = true
		}
		if err := httpstep.ValidateStep(st, captured); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
	}
	return nil
}
