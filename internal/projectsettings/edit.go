// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"
)

const (
	// EditedAnnotation is the annotation a save leaves on the Project: who
	// changed its settings last, when, and which fields (an encoded
	// ProjectSettingsEdit). It is the one piece of metadata the status
	// server writes, in the same patch as the change.
	EditedAnnotation = "maul.run/settings-edited"
	// FieldManager is the field manager the status server's settings
	// patches are recorded under, so that a later apply's conflict names
	// it, and the page can tell its own fields from another manager's.
	FieldManager = "maul-status-server"
	// MaxEditBytes bounds the annotation's value.
	MaxEditBytes = 1024
)

// NewEdit is the record of a save: the account id the session carried, the
// approver login intent-controller resolved it to, and the ids of the
// fields changed, each once.
func NewEdit(githubID int64, login string, at time.Time, changes []ProjectSettingsChange) ProjectSettingsEdit {
	e := ProjectSettingsEdit{GitHubID: githubID, Login: login, At: at.UTC().Format(time.RFC3339), Fields: []string{}}
	for _, c := range changes {
		if !slices.Contains(e.Fields, c.Field) {
			e.Fields = append(e.Fields, c.Field)
		}
	}
	return e
}

// EncodeEdit is the annotation's value: the record as JSON, at most
// MaxEditBytes. A record of more fields than fit keeps the first and counts
// the rest in More.
func EncodeEdit(e ProjectSettingsEdit) string {
	e.Fields = append([]string{}, e.Fields...)
	for {
		raw, err := json.Marshal(e)
		if err != nil {
			return ""
		}
		if len(raw) <= MaxEditBytes || len(e.Fields) == 0 {
			return string(raw)
		}
		e.Fields, e.More = e.Fields[:len(e.Fields)-1], e.More+1
	}
}

// ParseEdit reads the annotation back, strictly. Anything that may write
// the Project may write its annotations, and what this returns is shown on
// the page, so a value that is not exactly a record is no record: bounded,
// one JSON object with no unknown key, an account id, a login in GitHub's
// grammar, a time. A field id it names is kept only if it is an editable
// field's; the others are counted in More.
func ParseEdit(raw string) (ProjectSettingsEdit, bool) {
	if raw == "" || len(raw) > MaxEditBytes {
		return ProjectSettingsEdit{}, false
	}
	var e ProjectSettingsEdit
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return ProjectSettingsEdit{}, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return ProjectSettingsEdit{}, false
	}
	at, err := time.Parse(time.RFC3339, e.At)
	if err != nil || e.GitHubID <= 0 || e.More < 0 || len(e.Login) > MaxLoginLength ||
		!loginPattern.MatchString(e.Login) {
		return ProjectSettingsEdit{}, false
	}
	known := []string{}
	for _, id := range e.Fields {
		if isField(id) && !slices.Contains(known, id) {
			known = append(known, id)
		}
	}
	e.At, e.More, e.Fields = at.UTC().Format(time.RFC3339), e.More+len(e.Fields)-len(known), known
	return e, true
}

// isField reports whether id is an editable field's.
func isField(id string) bool {
	_, ok := rowByID(id)
	return ok || id == FieldRepositoryClass
}
