// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEditNames(t *testing.T) {
	if EditedAnnotation != "maul.run/settings-edited" || FieldManager != "maul-status-server" ||
		MaxEditBytes != 1024 {
		t.Errorf("annotation %q, field manager %q, limit %d", EditedAnnotation, FieldManager, MaxEditBytes)
	}
}

func TestEditRoundTrip(t *testing.T) {
	at := time.Date(2026, 11, 23, 13, 4, 5, 999, time.FixedZone("CET", 3600))
	changes := []ProjectSettingsChange{
		{Field: FieldMaxRevisions}, {Field: FieldRepositoryClass, Item: "app"},
		{Field: FieldRepositoryClass, Item: "lib"}, {Field: "models.plan"},
	}
	edit := NewEdit(225740, "alice", at, changes)
	want := ProjectSettingsEdit{GitHubID: 225740, Login: "alice", At: "2026-11-23T12:04:05Z",
		Fields: []string{FieldMaxRevisions, FieldRepositoryClass, "models.plan"}}
	if !reflect.DeepEqual(edit, want) {
		t.Errorf("NewEdit = %+v, want %+v", edit, want)
	}
	raw := EncodeEdit(edit)
	const wire = `{"githubID":225740,"login":"alice","at":"2026-11-23T12:04:05Z",` +
		`"fields":["limits.maxRevisions","repositories.agentResourceClass","models.plan"],"more":0}`
	if raw != wire {
		t.Errorf("EncodeEdit = %s\nwant         %s", raw, wire)
	}
	if back, ok := ParseEdit(raw); !ok || !reflect.DeepEqual(back, want) {
		t.Errorf("ParseEdit = %+v, %v", back, ok)
	}
}

// The annotation is at most 1 KiB whatever a save changed: field ids are
// dropped from the end and counted.
func TestEditFitsTheAnnotation(t *testing.T) {
	changes := make([]ProjectSettingsChange, 0, len(Fields()))
	for _, f := range Fields() {
		changes = append(changes, ProjectSettingsChange{Field: f.ID})
	}
	edit := NewEdit(1<<62, strings.Repeat("a", MaxLoginLength), testNow, changes)
	if len(edit.Fields) != len(Fields()) {
		t.Fatalf("NewEdit kept %d of %d fields", len(edit.Fields), len(Fields()))
	}
	raw := EncodeEdit(edit)
	if len(raw) > MaxEditBytes {
		t.Fatalf("the annotation is %d bytes", len(raw))
	}
	back, ok := ParseEdit(raw)
	if !ok {
		t.Fatalf("ParseEdit refuses its own encoding: %s", raw)
	}
	if len(back.Fields)+back.More != len(Fields()) {
		t.Errorf("kept %d fields and counted %d more, of %d", len(back.Fields), back.More, len(Fields()))
	}
	if !reflect.DeepEqual(back.Fields, edit.Fields[:len(back.Fields)]) {
		t.Error("the fields kept are not the first ones")
	}

	// Every editable field's id fits today; a record of more than fits
	// (the table grown, say) still keeps the first ones and counts the rest.
	long := make([]ProjectSettingsChange, 0, 3*len(Fields()))
	for i := range 3 {
		for _, f := range Fields() {
			long = append(long, ProjectSettingsChange{Field: f.ID + strings.Repeat("x", i)})
		}
	}
	big := NewEdit(1<<62, strings.Repeat("a", MaxLoginLength), testNow, long)
	raw = EncodeEdit(big)
	var cut ProjectSettingsEdit
	if err := json.Unmarshal([]byte(raw), &cut); err != nil || len(raw) > MaxEditBytes {
		t.Fatalf("an over-long record encodes as %d bytes (%v)", len(raw), err)
	}
	if cut.More == 0 || len(cut.Fields)+cut.More != len(long) ||
		!reflect.DeepEqual(cut.Fields, big.Fields[:len(cut.Fields)]) {
		t.Errorf("an over-long record kept %d fields and counted %d more, of %d", len(cut.Fields), cut.More,
			len(long))
	}
}

// What the page shows as "last changed by" came out of an annotation, which
// anything that can write the Project can write. So it is read strictly: a
// value that is not exactly an edit record is no record, and a field id it
// names is shown only if it is one.
func TestParseEditIsStrict(t *testing.T) {
	good := `{"githubID":7,"login":"alice","at":"2026-11-23T12:00:00Z","fields":["limits.maxRevisions"],"more":0}`
	if _, ok := ParseEdit(good); !ok {
		t.Fatal("the well-formed record is refused")
	}
	for name, raw := range map[string]string{
		"empty":           "",
		"not JSON":        "alice changed things",
		"an unknown key":  `{"githubID":7,"login":"alice","at":"2026-11-23T12:00:00Z","fields":[],"more":0,"x":1}`,
		"trailing data":   good + good,
		"no id":           `{"githubID":0,"login":"alice","at":"2026-11-23T12:00:00Z","fields":[],"more":0}`,
		"a negative id":   `{"githubID":-7,"login":"alice","at":"2026-11-23T12:00:00Z","fields":[],"more":0}`,
		"no login":        `{"githubID":7,"login":"","at":"2026-11-23T12:00:00Z","fields":[],"more":0}`,
		"markup as login": `{"githubID":7,"login":"<b>x</b>","at":"2026-11-23T12:00:00Z","fields":[],"more":0}`,
		"no time":         `{"githubID":7,"login":"alice","at":"yesterday","fields":[],"more":0}`,
		"a negative more": `{"githubID":7,"login":"alice","at":"2026-11-23T12:00:00Z","fields":[],"more":-1}`,
		"too long": fmt.Sprintf(`{"githubID":7,"login":"alice","at":"2026-11-23T12:00:00Z","fields":[%s],"more":0}`,
			strings.TrimSuffix(strings.Repeat(`"limits.maxRevisions",`, 60), ",")),
	} {
		if e, ok := ParseEdit(raw); ok {
			t.Errorf("%s: accepted as %+v", name, e)
		}
	}
	e, ok := ParseEdit(`{"githubID":7,"login":"alice","at":"2026-11-23T12:00:00Z",` +
		`"fields":["limits.maxRevisions","<script>","spec.suspend","models.plan"],"more":2}`)
	if !ok || !reflect.DeepEqual(e.Fields, []string{"limits.maxRevisions", "models.plan"}) || e.More != 4 {
		t.Errorf("ParseEdit = %+v, %v: want the two real fields, the other two counted", e, ok)
	}
}
