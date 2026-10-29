// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import "time"

// The capture (DESIGN 2.9): preview-controller writes each component's
// redacted log tail and the slot's events into the PV's slot tool
// ConfigMap on each poll; the tools container reads them, as files of the
// same names under its mount, for `maul-tool logs <component>` and
// `maul-tool events`. Both sides use these keys and this shape.
const (
	// CaptureLogPrefix keys a component's log tail: logs.<component>.
	CaptureLogPrefix = "logs."
	// CaptureEventsKey keys the slot's events and the components'
	// container states: a JSON array of CapturedEvent, oldest first.
	CaptureEventsKey = "events.json"
	// CaptureObserved is a CapturedEvent's Source: the kubelet or the API
	// server wrote it, never the application.
	CaptureObserved = "observed"
)

// CaptureLogKey is component's log tail key.
func CaptureLogKey(component string) string { return CaptureLogPrefix + component }

// CapturedEvent is one slot event (Kind "event") or container state (Kind
// "state"). Everything in it is observed; Message is the kubelet's or the
// API server's text, redacted and bounded, which can still quote the
// application (a probe's output), so the tools container serves it as
// data.
type CapturedEvent struct {
	Time      time.Time `json:"time"`
	Kind      string    `json:"kind"`
	Source    string    `json:"source"`
	Component string    `json:"component,omitempty"`
	Object    string    `json:"object"`
	Container string    `json:"container,omitempty"`
	Type      string    `json:"type,omitempty"`
	Reason    string    `json:"reason"`
	Message   string    `json:"message,omitempty"`
	Count     int32     `json:"count,omitempty"`
	ExitCode  *int32    `json:"exitCode,omitempty"`
	Restarts  int32     `json:"restarts,omitempty"`
}
