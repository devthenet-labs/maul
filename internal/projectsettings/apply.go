// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

import (
	"fmt"

	"github.com/devthenet-labs/maul/api/v1alpha1"
)

// Read is a Project's editable settings as the page is served them. Fields
// the schema defaults read as their default even in a Project the API
// server never defaulted, and no list is nil. It does not change the spec.
func Read(spec *v1alpha1.ProjectSpec) ProjectSettings {
	var doc ProjectSettings
	for _, r := range table {
		store(r.slot(&doc), r.read(spec))
	}
	doc.Repositories = make([]ProjectSettingsRepository, 0, len(spec.Repositories))
	for _, repo := range spec.Repositories {
		doc.Repositories = append(doc.Repositories,
			ProjectSettingsRepository{Name: repo.Name, AgentResourceClass: repo.AgentResourceClass})
	}
	return doc
}

// Why a document cannot be applied at all: it is not one the page could
// have posted for this Project.
const (
	// DocumentUnknownRepository: it names a repository the Project lacks.
	DocumentUnknownRepository = "unknown-repository"
	// DocumentDuplicateRepository: it names a repository twice.
	DocumentDuplicateRepository = "duplicate-repository"
	// DocumentNotADuration: a duration that is negative or beyond
	// MaxSeconds.
	DocumentNotADuration = "not-a-duration"
)

// DocumentError is a document Apply refuses whole, changing nothing: the
// Field (and for a repository, the Item) it is about and the Reason.
type DocumentError struct {
	Field  string
	Item   string
	Reason string
}

func (e *DocumentError) Error() string {
	if e.Item != "" {
		return fmt.Sprintf("project settings: %s %q: %s", e.Field, e.Item, e.Reason)
	}
	return fmt.Sprintf("project settings: %s: %s", e.Field, e.Reason)
}

// Apply changes p's spec to hold the settings s, and reports what changed.
// Give it a copy: the write path patches the difference between the Project
// it read and the copy Apply changed.
//
// It compares in the document's own terms: each field of the table is read
// from the live spec as the page was served it, and written only where s
// differs. So a save that changes no setting changes nothing in p at all,
// however the spec spells a value (a duration written "10m" or "10m0s", a
// block absent or empty, a default stored or implied), and its merge patch
// is empty. A set keeps its stored order. Nothing outside the table can
// change: of a repository only agentResourceClass, found by name, never the
// list's length or order, and a repository s leaves out is left alone.
//
// Apply judges nothing; Check does. It refuses only a document that could
// not have come from the page (DocumentError), and then changes nothing.
func Apply(p *v1alpha1.Project, s ProjectSettings) ([]ProjectSettingsChange, error) {
	if err := checkDocument(&p.Spec, &s); err != nil {
		return nil, err
	}
	before := p.Spec.DeepCopy()
	for _, r := range table {
		if want := r.normal(load(r.slot(&s))); !r.read(&p.Spec).equal(want) {
			r.write(&p.Spec, want)
		}
	}
	for _, repo := range s.Repositories {
		p.Spec.Repositories[repositoryIndex(&p.Spec, repo.Name)].AgentResourceClass = repo.AgentResourceClass
	}
	return Changes(before, &p.Spec), nil
}

// checkDocument refuses a document Apply cannot apply.
func checkDocument(spec *v1alpha1.ProjectSpec, s *ProjectSettings) error {
	for _, r := range table {
		if n := load(r.slot(s)).n; r.Kind == KindSeconds && (n < 0 || n > MaxSeconds) {
			return &DocumentError{Field: r.ID, Reason: DocumentNotADuration}
		}
	}
	seen := map[string]bool{}
	for _, repo := range s.Repositories {
		switch {
		case repositoryIndex(spec, repo.Name) < 0:
			return &DocumentError{Field: FieldRepositoryClass, Item: repo.Name, Reason: DocumentUnknownRepository}
		case seen[repo.Name]:
			return &DocumentError{Field: FieldRepositoryClass, Item: repo.Name, Reason: DocumentDuplicateRepository}
		}
		seen[repo.Name] = true
	}
	return nil
}

// repositoryIndex is where the spec lists the repository of that name, -1
// when it lists none.
func repositoryIndex(spec *v1alpha1.ProjectSpec, name string) int {
	for i, repo := range spec.Repositories {
		if repo.Name == name {
			return i
		}
	}
	return -1
}

// Changes are the editable settings that differ between two specs, in the
// table's order, then the repositories' in updated's order. It is what
// Apply reports, and what Check and Confirmations judge: they read the
// difference from the two objects themselves, never from a caller's list.
func Changes(old, updated *v1alpha1.ProjectSpec) []ProjectSettingsChange {
	out := []ProjectSettingsChange{}
	for _, r := range table {
		was, is := r.read(old), r.read(updated)
		if was.equal(is) {
			continue
		}
		c := newChange(r.Field, "", was.text(r.Field), is.text(r.Field))
		if r.Kind.list() {
			c.Added = append(c.Added, missing(is.list, was.list)...)
			c.Removed = append(c.Removed, missing(was.list, is.list)...)
		}
		out = append(out, c)
	}
	for _, repo := range updated.Repositories {
		i := repositoryIndex(old, repo.Name)
		if i < 0 || old.Repositories[i].AgentResourceClass == repo.AgentResourceClass {
			continue
		}
		out = append(out, newChange(classField, repo.Name, old.Repositories[i].AgentResourceClass,
			repo.AgentResourceClass))
	}
	return out
}

func newChange(f Field, item, from, to string) ProjectSettingsChange {
	return ProjectSettingsChange{Field: f.ID, Item: item, Group: string(f.Group), From: from, To: to,
		Added: []string{}, Removed: []string{}, Effect: string(f.Effect)}
}
