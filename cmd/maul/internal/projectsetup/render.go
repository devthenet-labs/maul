// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsetup

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/forge"
)

// The Project CRD's own limits and patterns (api/v1alpha1/project_types.go),
// which the schema holds a kubectl-applied Project to.
const (
	maxRepositories = 8
	maxApprovers    = 20
	maxLoginLength  = 64
	maxURLLength    = 256
	maxLabelLength  = 50
)

var (
	repoURLPattern = regexp.MustCompile(`^https://[^/\s@?#]+/[^/\s?#]+/[^/\s?#]+$`)
	keyPattern     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	loginPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	// shorthandPattern is owner/repo in GitHub's own name grammar. The
	// CRD's URL pattern is wider (it admits @ and : in a path), so without
	// this an SSH remote, git@github.com:owner/repo.git, would be written
	// as a github.com URL the cluster admits and no Forge ever covers.
	shorthandPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// Input is what `maul setup project` was given.
type Input struct {
	Name       string
	Namespace  string
	IntentRepo string   // owner/repo or an https URL
	Repos      []string // [key=]owner/repo or [key=]https URL
	Approvers  []string
	// TriggerLabel and ApproveLabel are "" when not given.
	TriggerLabel string
	ApproveLabel string
}

// Repository is one repository of the Project: its https URL and, for an
// application repository, its key.
type Repository struct {
	Key   string
	URL   string
	host  string
	owner string
}

// account is the GitHub account the repository belongs to.
func (r Repository) account() Account { return Account{Host: r.host, Owner: r.owner} }

// Account is a GitHub account on a host: where an App is installed. Host is
// lowercased and keeps a port the repository URL names.
type Account struct {
	Host  string
	Owner string
}

// Web is the address the account's host serves people on.
func (a Account) Web() string { return "https://" + a.Host }

// Hostname is the host without any port: the name gh and `maul check
// project` (GH_HOST) know it by.
func (a Account) Hostname() string {
	if h, _, err := net.SplitHostPort(a.Host); err == nil {
		return h
	}
	return a.Host
}

// GitHubCom reports whether the account is on github.com, the one host
// `maul setup github-app` creates Apps on and GH_TOKEN is read for.
func (a Account) GitHubCom() bool { return a.Host == "github.com" }

// Project is a validated `maul setup project` invocation.
type Project struct {
	Name             string
	Namespace        string
	IntentRepository Repository
	Repositories     []Repository
	Approvers        []string
	// TriggerLabel and ApproveLabel are as given: "" leaves the label to
	// the CRD (Trigger and Approve are the effective ones).
	TriggerLabel string
	ApproveLabel string
}

// Trigger is the label that starts an intent: the one given, else
// maul:<name>, as intent-controller derives it.
func (p Project) Trigger() string {
	if p.TriggerLabel != "" {
		return p.TriggerLabel
	}
	return v1alpha1.DefaultTriggerLabelPrefix + p.Name
}

// Approve is the label that approves a plan: the one given, else the
// CRD's default.
func (p Project) Approve() string {
	if p.ApproveLabel != "" {
		return p.ApproveLabel
	}
	return v1alpha1.DefaultApproveLabel
}

// Owners is every account the App must be installed on, once each,
// compared case-insensitively: the intent repository's first, then the
// application repositories' in order.
func (p Project) Owners() []Account {
	var out []Account
	seen := map[Account]bool{}
	for _, r := range append([]Repository{p.IntentRepository}, p.Repositories...) {
		a := r.account()
		k := Account{Host: a.Host, Owner: strings.ToLower(a.Owner)}
		if !seen[k] {
			seen[k] = true
			out = append(out, a)
		}
	}
	return out
}

// Plan holds in to the Project CRD's rules. Every error names the flag (or
// argument) to change.
func Plan(in Input) (Project, error) {
	p := Project{Name: in.Name, Namespace: in.Namespace, TriggerLabel: in.TriggerLabel,
		ApproveLabel: in.ApproveLabel}
	if err := checkName(in.Name); err != nil {
		return p, err
	}
	if errs := validation.IsDNS1123Label(in.Namespace); len(errs) > 0 {
		return p, fmt.Errorf("-n %q: %s", in.Namespace, strings.Join(errs, "; "))
	}
	if in.IntentRepo == "" {
		return p, errors.New("--intent-repo is required: owner/repo or https://host/owner/repo")
	}
	var err error
	if p.IntentRepository, err = parseRepo("--intent-repo", in.IntentRepo); err != nil {
		return p, err
	}
	if p.Repositories, err = planRepositories(in.Repos); err != nil {
		return p, err
	}
	if p.Approvers, err = planApprovers(in.Approvers); err != nil {
		return p, err
	}
	return p, checkLabels(p)
}

// checkName holds the Project's name to a DNS label of at most 25
// characters, which keeps every Intent and IntentRun name derived from it
// inside 63.
func checkName(name string) error {
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return fmt.Errorf("project name %q: %s", name, strings.Join(errs, "; "))
	}
	if len(name) > v1alpha1.MaxProjectNameLength {
		return fmt.Errorf("project name %q has %d characters: a Project's name is at most %d, so the names of "+
			"its intents fit", name, len(name), v1alpha1.MaxProjectNameLength)
	}
	return nil
}

// parseRepo reads owner/repo (on github.com) or an https URL, held to the
// CRD's URL pattern.
func parseRepo(flag, raw string) (Repository, error) {
	u := raw
	if !strings.Contains(raw, "://") {
		if !shorthandPattern.MatchString(raw) {
			return Repository{}, fmt.Errorf("%s %q: write owner/repo or https://host/owner/repo", flag, raw)
		}
		u = "https://github.com/" + raw
	}
	if utf8.RuneCountInString(u) > maxURLLength {
		return Repository{}, fmt.Errorf("%s %q: a repository URL is at most %d characters", flag, raw, maxURLLength)
	}
	if !repoURLPattern.MatchString(u) {
		return Repository{}, fmt.Errorf("%s %q: write owner/repo or https://host/owner/repo (https, no "+
			"credentials, query or deeper path)", flag, raw)
	}
	_, repo, err := forge.ParseRepoURL(u)
	if err != nil {
		return Repository{}, fmt.Errorf("%s: %w", flag, err)
	}
	// The host keeps its port, which the pattern has just held to no
	// slash: the installations page is served there, not on 443.
	host := strings.ToLower(strings.SplitN(strings.TrimPrefix(u, "https://"), "/", 2)[0])
	return Repository{URL: u, host: host, owner: repo.Owner, Key: strings.ToLower(repo.Name)}, nil
}

// planRepositories reads every --repo: 1 to 8, each key valid and used
// once, no repository twice (compared as the CRD compares them).
func planRepositories(raws []string) ([]Repository, error) {
	switch {
	case len(raws) == 0:
		return nil, errors.New("--repo is required: an application repository, [key=]owner/repo")
	case len(raws) > maxRepositories:
		return nil, fmt.Errorf("--repo is given %d times: a Project has at most %d repositories", len(raws),
			maxRepositories)
	}
	out := make([]Repository, 0, len(raws))
	keys, urls := map[string]bool{}, map[string]bool{}
	for _, raw := range raws {
		r, err := parseKeyedRepo(raw)
		if err != nil {
			return nil, err
		}
		u := strings.ToLower(strings.TrimSuffix(r.URL, ".git"))
		if urls[u] {
			return nil, fmt.Errorf("--repo %q: names the same repository as another --repo", raw)
		}
		if keys[r.Key] {
			return nil, fmt.Errorf("--repo %q: key %s is used twice: write --repo <short>=owner/repo with "+
				"another key", raw, r.Key)
		}
		keys[r.Key], urls[u] = true, true
		out = append(out, r)
	}
	return out, nil
}

// parseKeyedRepo reads [key=]owner/repo or [key=]https URL. Text before an
// = is a key only when it holds no / or :, so a URL is never split.
func parseKeyedRepo(raw string) (Repository, error) {
	key, ref, keyed := strings.Cut(raw, "=")
	if !keyed || strings.ContainsAny(key, "/:") {
		key, ref, keyed = "", raw, false
	}
	r, err := parseRepo("--repo", ref)
	if err != nil {
		return r, err
	}
	if !keyed {
		if !validKey(r.Key) {
			return r, fmt.Errorf("--repo %q: repository key %s is not valid (lowercase, at most %d characters): "+
				"write --repo <short>=owner/repo", raw, r.Key, v1alpha1.MaxRepositoryKeyLength)
		}
		return r, nil
	}
	if !validKey(key) {
		return r, fmt.Errorf("--repo %q: key %q is not valid: lowercase letters, digits and hyphens, at most %d "+
			"characters", raw, key, v1alpha1.MaxRepositoryKeyLength)
	}
	r.Key = key
	return r, nil
}

// validKey is a repository key the CRD admits.
func validKey(k string) bool {
	return len(k) <= v1alpha1.MaxRepositoryKeyLength && keyPattern.MatchString(k)
}

// planApprovers holds every --approver to GitHub's login grammar (no Bot),
// at most 20, each once.
func planApprovers(logins []string) ([]string, error) {
	switch {
	case len(logins) == 0:
		return nil, errors.New("--approver is required: a GitHub login whose approval counts")
	case len(logins) > maxApprovers:
		return nil, fmt.Errorf("--approver is given %d times: a Project has at most %d approvers", len(logins),
			maxApprovers)
	}
	seen := map[string]bool{}
	for _, l := range logins {
		if len(l) > maxLoginLength || !loginPattern.MatchString(l) {
			return nil, fmt.Errorf("--approver %q: not a GitHub login (a person's, at most %d characters; a Bot "+
				"never approves)", l, maxLoginLength)
		}
		if seen[strings.ToLower(l)] {
			return nil, fmt.Errorf("--approver %q is given twice", l)
		}
		seen[strings.ToLower(l)] = true
	}
	return logins, nil
}

// checkLabels holds the labels to GitHub's 50 characters and to differing
// from each other, as the schema does once it has defaulted the approve
// label.
func checkLabels(p Project) error {
	for _, l := range []struct{ flag, value string }{
		{"--trigger-label", p.TriggerLabel}, {"--approve-label", p.ApproveLabel},
	} {
		if utf8.RuneCountInString(l.value) > maxLabelLength {
			return fmt.Errorf("%s %q: a GitHub label is at most %d characters", l.flag, l.value, maxLabelLength)
		}
		// GitHub trims a label's name: one with outer blanks would be
		// admitted by the schema and matched by no issue.
		if strings.TrimSpace(l.value) != l.value {
			return fmt.Errorf("%s %q: a GitHub label neither starts nor ends with a blank", l.flag, l.value)
		}
	}
	if strings.EqualFold(p.Trigger(), p.Approve()) {
		return fmt.Errorf("the trigger label %s and the approve label %s must differ: pass --trigger-label or "+
			"--approve-label", p.Trigger(), p.Approve())
	}
	return nil
}

// manifest is a maul.run/v1alpha1 Project with only the fields this command
// sets: no status, no creationTimestamp, defaults left to the CRD.
type manifest struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   metadata `json:"metadata"`
	Spec       spec     `json:"spec"`
}

type metadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type spec struct {
	IntentRepository string       `json:"intentRepository"`
	Approvers        approvers    `json:"approvers"`
	Repositories     []repository `json:"repositories"`
	Labels           *labels      `json:"labels,omitempty"`
}

type approvers struct {
	Logins []string `json:"logins"`
}

type repository struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type labels struct {
	Trigger string `json:"trigger,omitempty"`
	Approve string `json:"approve,omitempty"`
}

// Render is p as a Project manifest, byte-stable.
func Render(p Project) ([]byte, error) {
	m := manifest{
		APIVersion: v1alpha1.GroupVersion.String(),
		Kind:       "Project",
		Metadata:   metadata{Name: p.Name, Namespace: p.Namespace},
		Spec: spec{
			IntentRepository: p.IntentRepository.URL,
			Approvers:        approvers{Logins: p.Approvers},
		},
	}
	for _, r := range p.Repositories {
		m.Spec.Repositories = append(m.Spec.Repositories, repository{Name: r.Key, URL: r.URL})
	}
	if p.TriggerLabel != "" || p.ApproveLabel != "" {
		m.Spec.Labels = &labels{Trigger: p.TriggerLabel, Approve: p.ApproveLabel}
	}
	out, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("render the Project: %w", err)
	}
	return out, nil
}
