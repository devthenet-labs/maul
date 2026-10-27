// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtester

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/devthenet-labs/maul/internal/acceptance"
)

// EnvSpec is the environment variable preview-controller renders the spec
// into, a literal value on the tester's container (no ConfigMap, no mount).
const EnvSpec = "MAUL_VERIFY_SPEC"

// MaxSpecBytes bounds the encoded spec.
const MaxSpecBytes = 32 << 10

// maxAttempts mirrors v1alpha1.MaxVerificationAttempts (this package
// imports nothing of the API).
const maxAttempts = 3

// maxBudgetSeconds bounds a spec's budget: the tester Job's deadline is at
// most an hour.
const maxBudgetSeconds = 3600

// The room BudgetFor leaves below the Job's deadline, for the pod to be
// scheduled and started and the summary to be written: a quarter of the
// deadline, at least minBudgetMargin and at most maxBudgetMargin.
const (
	minBudgetMargin = 15 * time.Second
	maxBudgetMargin = 2 * time.Minute
)

// BudgetFor is the budget, in whole seconds, of a tester whose Job has the
// given deadline: the deadline less a margin, so the tester's own budget
// ends, and its summary is written, before the kubelet stops it. At least
// one second.
func BudgetFor(deadline time.Duration) int32 {
	margin := min(max(deadline/4, minBudgetMargin), maxBudgetMargin)
	secs := (deadline - margin) / time.Second
	return int32(min(max(secs, 1), maxBudgetSeconds))
}

// maxTargets bounds the targets map: a Preview has at most four components;
// the slack keeps the bound a bound without tying it to that number. It is
// the most targets a summary records a readiness wait for.
const maxTargets = acceptance.MaxWaitTargets

// maxTargetKey bounds a target key: a repository key, as the grammar and
// the summary's readiness waits hold it.
const maxTargetKey = 16

// Spec is what one tester run observes: a PreviewVerification's criteria,
// the tokens their {{run}} placeholders take, and, per previewed
// repository key, the Service address its requests go to. preview-controller
// derives every target from the Preview (PreviewComponentServiceName, port
// 80); no criterion can name a host.
type Spec struct {
	Round   int32 `json:"round"`
	Attempt int32 `json:"attempt"`
	// Run is the attempt's six random lowercase hex characters.
	Run string `json:"run"`
	// BudgetSeconds is how long the tester observes: past it, the
	// criterion in flight and every one not yet reached are recorded as
	// timed out and the summary is written. preview-controller sets it
	// below the Job's deadline (BudgetFor).
	BudgetSeconds int32 `json:"budgetSeconds"`
	// Criteria are the automatable http criteria, in plan order.
	Criteria []acceptance.Criterion `json:"criteria"`
	// Targets maps a criterion's target (a repository key) to "<host>:<port>",
	// the host a Service name, never an IP literal.
	Targets map[string]string `json:"targets"`
}

var (
	runPattern = regexp.MustCompile(`^[0-9a-f]{6}$`)
	keyPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// hostPattern is a lowercase DNS name: a Service name, optionally
	// qualified by namespace and cluster domain.
	hostPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$`)
)

// RunTokens are the tokens the spec's {{run}} placeholders take.
func (s Spec) RunTokens() acceptance.RunTokens {
	return acceptance.RunTokens{Round: s.Round, Attempt: s.Attempt, Run: s.Run}
}

// ParseSpec decodes and validates a spec: at most MaxSpecBytes, strict
// JSON (unknown fields refused), and Validate's rules.
func ParseSpec(b []byte) (Spec, error) {
	if len(b) > MaxSpecBytes {
		return Spec{}, fmt.Errorf("spec is over %d bytes", MaxSpecBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return Spec{}, fmt.Errorf("decode spec: %w", err)
	}
	if dec.More() {
		return Spec{}, errors.New("decode spec: trailing data")
	}
	if err := s.Validate(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

// Encode validates s and encodes it as EnvSpec's value.
func (s Spec) Encode() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encode spec: %w", err)
	}
	if len(b) > MaxSpecBytes {
		return nil, fmt.Errorf("spec is over %d bytes", MaxSpecBytes)
	}
	return b, nil
}

// Validate holds a spec to what the tester runs: a round, an attempt
// within its bound, a run token and a budget; one to MaxCriteria http criteria the
// grammar admits; and for each criterion's target, a Service address.
func (s Spec) Validate() error {
	switch {
	case s.Round < 1:
		return errors.New("spec: round must be at least 1")
	case s.Attempt < 1 || s.Attempt > maxAttempts:
		return fmt.Errorf("spec: attempt must be 1..%d", maxAttempts)
	case !runPattern.MatchString(s.Run):
		return errors.New("spec: run must be six lowercase hex characters")
	case s.BudgetSeconds < 1 || s.BudgetSeconds > maxBudgetSeconds:
		return fmt.Errorf("spec: budgetSeconds must be 1..%d", maxBudgetSeconds)
	case len(s.Criteria) == 0:
		return errors.New("spec: no criteria")
	case len(s.Targets) > maxTargets:
		return fmt.Errorf("spec: more than %d targets", maxTargets)
	}
	if err := acceptance.Validate(s.Criteria); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	for key, addr := range s.Targets {
		if err := checkTarget(key, addr); err != nil {
			return err
		}
	}
	for _, c := range s.Criteria {
		if c.Kind != acceptance.KindHTTP {
			return fmt.Errorf("spec: criterion %q is %s; the tester runs only http criteria", c.ID, c.Kind)
		}
		if _, ok := s.Targets[c.Target]; !ok {
			return fmt.Errorf("spec: criterion %q targets %q, which has no address", c.ID, c.Target)
		}
	}
	return nil
}

// checkTarget admits "<DNS name>:<port>" and nothing else: no scheme,
// userinfo, path or IP literal, so the address can only name a Service.
func checkTarget(key, addr string) error {
	if len(key) > maxTargetKey || !keyPattern.MatchString(key) {
		return fmt.Errorf("spec: target key %q is not a repository key", key)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("spec: target %q: %w", key, err)
	}
	n, err := strconv.Atoi(port)
	switch {
	case len(host) > 253 || !hostPattern.MatchString(host) || net.ParseIP(host) != nil:
		return fmt.Errorf("spec: target %q host %q is not a Service name", key, host)
	case err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port:
		return fmt.Errorf("spec: target %q port %q is not a port", key, port)
	}
	return nil
}
