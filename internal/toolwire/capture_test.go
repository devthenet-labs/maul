// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package toolwire

import (
	"encoding/json"
	"regexp"
	"testing"
	"time"
)

// configMapKey is the API server's ConfigMap key grammar: every capture key
// must be one, since it is both a ConfigMap key (preview-controller writes
// it) and a file name under the mount (the tools container reads it).
var configMapKey = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)

func TestCaptureKeys(t *testing.T) {
	for _, key := range []string{CaptureLogKey("api"), CaptureLogKey("web-2"), CaptureEventsKey} {
		if !configMapKey.MatchString(key) {
			t.Errorf("capture key %q is not a ConfigMap key", key)
		}
	}
	if got, want := CaptureLogKey("api"), CaptureLogPrefix+"api"; got != want {
		t.Errorf("CaptureLogKey = %q, want %q", got, want)
	}
	if CaptureEventsKey == CaptureLogKey("events") || CaptureEventsKey == CaptureLogPrefix {
		t.Error("the events key collides with a component's log key")
	}
}

// The events JSON shape is the one both sides read: pinned field by field.
func TestCapturedEventShape(t *testing.T) {
	code := int32(137)
	e := CapturedEvent{Time: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC), Kind: "state", Source: CaptureObserved,
		Component: "api", Object: "Pod/api-1", Container: "api", Type: "Warning", Reason: "OOMKilled",
		Message: "m", Count: 2, ExitCode: &code, Restarts: 3}
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"time":"2026-10-28T09:00:00Z","kind":"state","source":"observed","component":"api",` +
		`"object":"Pod/api-1","container":"api","type":"Warning","reason":"OOMKilled","message":"m",` +
		`"count":2,"exitCode":137,"restarts":3}`
	if string(got) != want {
		t.Errorf("event JSON\n got %s\nwant %s", got, want)
	}
	bare, err := json.Marshal(CapturedEvent{Kind: "event", Source: CaptureObserved, Object: "Pod/x", Reason: "BackOff"})
	if err != nil {
		t.Fatal(err)
	}
	want = `{"time":"0001-01-01T00:00:00Z","kind":"event","source":"observed","object":"Pod/x","reason":"BackOff"}`
	if string(bare) != want {
		t.Errorf("minimal event JSON\n got %s\nwant %s", bare, want)
	}
}
