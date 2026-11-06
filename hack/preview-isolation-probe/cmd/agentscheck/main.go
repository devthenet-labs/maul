// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// agentscheck holds a live agents-namespace tool pod to the agents run's
// last rows (run.sh agents; SPEC S2-21), from the maul checkout the run is
// started from. It reads only what run.sh hands it, never a cluster:
//
//	agentscheck pod --taint-key <key> < pod.json
//	    the pod's shape (SP-6, SP-7): shareProcessNamespace explicitly
//	    false; tools, sandbox-head and sandbox-base as 65532, 65540 and
//	    65541; no ServiceAccount token, no ephemeral container; no
//	    toleration of the preview taint or of every taint.
//	agentscheck ledger --log <file> --termination <file> --nonce <hex>
//	    the tools container's log and termination message after the
//	    evaluation finished: the ledger verifies (toolwire.Verify, as the
//	    controller reads it) and none of the lines the sandbox program
//	    forged reached either, raw or as a ledger line.
//
// Each prints one JSON line per row and a summary, PASS, SECURITY_FAILURE
// or INCONCLUSIVE, and exits 0, 2 or 3.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/devthenet-labs/maul/internal/toolwire"
)

const (
	outcomePass            = "PASS"
	outcomeSecurityFailure = "SECURITY_FAILURE"
	outcomeInconclusive    = "INCONCLUSIVE"

	rowHeld   = "held"
	rowBroken = "BROKEN"
)

// forgedLabel starts the raw lines the sandbox program forges
// (sandboxprog's forgedLabel).
const forgedLabel = "PROBE-FORGED"

// The tool pod's UIDs (jobs.ToolPod).
var wantUIDs = map[string]int64{"tools": 65532, "sandbox-head": 65540, "sandbox-base": 65541}

type rows struct {
	out     io.Writer
	failed  bool
	unknown bool
}

func (r *rows) row(name string, held bool, detail string) {
	outcome := rowHeld
	if !held {
		outcome, r.failed = rowBroken, true
	}
	r.emit(name, outcome, detail)
}

func (r *rows) emit(name, outcome, detail string) {
	_ = json.NewEncoder(r.out).Encode(map[string]string{"name": name, "outcome": outcome, "detail": detail})
}

func (r *rows) summary() string {
	outcome := outcomePass
	switch {
	case r.failed:
		outcome = outcomeSecurityFailure
	case r.unknown:
		outcome = outcomeInconclusive
	}
	r.emit("summary", outcome, "")
	return outcome
}

// checkLedger: the ledger verifies, no log line is a forged raw line or a
// forged ledger line, and the termination message carries no nonce. No
// termination message yet proves nothing.
func checkLedger(log []byte, termination, nonce string, out io.Writer) string {
	r := &rows{out: out}
	if strings.TrimSpace(termination) == "" {
		r.unknown = true
		r.emit("ledger", outcomeInconclusive, "the tools container has no termination message: has it finished?")
		return r.summary()
	}
	_, err := toolwire.Verify(log, termination)
	r.row("ledger-verifies", err == nil, fmt.Sprint(err))
	var forged []string
	for line := range strings.SplitSeq(string(log), "\n") {
		if strings.HasPrefix(line, forgedLabel) || strings.Contains(line, `{"forged":"`) {
			forged = append(forged, line)
		}
	}
	r.row("tools-log-unforged", len(forged) == 0, strings.Join(forged, " | "))
	r.row("termination-unforged", !strings.Contains(termination, nonce), "")
	return r.summary()
}

// checkPod holds pod (JSON) to the tool pod's shape.
func checkPod(raw []byte, taintKey string, out io.Writer) string {
	r := &rows{out: out}
	var pod corev1.Pod
	if err := json.Unmarshal(raw, &pod); err != nil || pod.Labels["maul.run/role"] != "tools" {
		r.unknown = true
		r.emit("pod", outcomeInconclusive, "not an agents tool pod")
		return r.summary()
	}
	spec := pod.Spec
	r.row("share-process-namespace", spec.ShareProcessNamespace != nil && !*spec.ShareProcessNamespace,
		"must be explicitly false")
	uids := map[string]int64{}
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		if _, ok := wantUIDs[c.Name]; ok && c.SecurityContext != nil && c.SecurityContext.RunAsUser != nil {
			uids[c.Name] = *c.SecurityContext.RunAsUser
		}
	}
	distinct := len(uids) == len(wantUIDs)
	for name, want := range wantUIDs {
		distinct = distinct && uids[name] == want
	}
	r.row("uids", distinct, fmt.Sprintf("%v, want %v", uids, wantUIDs))
	r.row("no-token", spec.AutomountServiceAccountToken != nil && !*spec.AutomountServiceAccountToken &&
		!projectsToken(spec.Volumes), "")
	r.row("no-ephemeral-container", len(spec.EphemeralContainers) == 0, "")
	var tolerated []string
	for _, t := range spec.Tolerations {
		if t.Key == taintKey || (t.Key == "" && t.Operator == corev1.TolerationOpExists) {
			tolerated = append(tolerated, t.Key+"/"+string(t.Operator))
		}
	}
	r.row("no-preview-toleration", len(tolerated) == 0, strings.Join(tolerated, ", "))
	return r.summary()
}

func projectsToken(vols []corev1.Volume) bool {
	for _, v := range vols {
		if v.Projected == nil {
			continue
		}
		for _, s := range v.Projected.Sources {
			if s.ServiceAccountToken != nil {
				return true
			}
		}
	}
	return false
}

var (
	nonceShape = regexp.MustCompile(`^[0-9a-f]{16,62}$`)
	taintShape = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]*[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
)

var errUsage = errors.New("usage: agentscheck pod --taint-key <key> < pod.json | " +
	"agentscheck ledger --log <file> --termination <file> --nonce <hex>")

func run(args []string, stdin io.Reader, out io.Writer) int {
	code := map[string]int{outcomePass: 0, outcomeSecurityFailure: 2, outcomeInconclusive: 3}
	usage := func() int {
		fmt.Fprintln(os.Stderr, errUsage)
		_ = json.NewEncoder(out).Encode(map[string]string{"name": "summary", "outcome": outcomeInconclusive})
		return 3
	}
	if len(args) == 0 {
		return usage()
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	taint := fs.String("taint-key", "", "the preview nodes' taint key")
	logFile := fs.String("log", "", "the tools container's log")
	termFile := fs.String("termination", "", "the tools container's termination message")
	nonce := fs.String("nonce", "", "the run's nonce")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usage()
	}
	switch args[0] {
	case "pod":
		if *taint != "" && !taintShape.MatchString(*taint) {
			return usage()
		}
		raw, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
		if err != nil {
			return usage()
		}
		return code[checkPod(raw, *taint, out)]
	case "ledger":
		if !nonceShape.MatchString(*nonce) {
			return usage()
		}
		log, err1 := os.ReadFile(*logFile)
		term, err2 := os.ReadFile(*termFile)
		if err1 != nil || err2 != nil {
			return usage()
		}
		return code[checkLedger(log, string(term), *nonce, out)]
	}
	return usage()
}

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout)) }
