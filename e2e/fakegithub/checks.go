// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package fakegithub

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// CheckAnnotation is one check-run annotation a test seeds.
type CheckAnnotation struct {
	Path    string
	Line    int
	Message string
}

// CheckRun is the fake's check-run state on one commit. Seeding the same ID
// again updates its state, as a GitHub check moves from queued to completed.
// RunID is the Actions workflow run it is a job of; its check suite is
// CheckSuiteID, or the run's own (WorkflowRun.CheckSuiteID's default) when
// that is 0.
type CheckRun struct {
	ID           int64
	Name         string
	Status       string
	Conclusion   string
	Title        string
	Summary      string
	Text         string
	RunID        int64
	CheckSuiteID int64
	AppSlug      string
	Annotations  []CheckAnnotation
}

// WorkflowRun is one Actions workflow run. Its check suite is CheckSuiteID,
// or, when that is 0, its own ID: the fake's one suite per run.
type WorkflowRun struct {
	ID           int64
	CheckSuiteID int64
	HeadSHA      string
	Status       string
	Conclusion   string
}

func (r WorkflowRun) suite() int64 {
	if r.CheckSuiteID != 0 {
		return r.CheckSuiteID
	}
	return r.ID
}

// CommitStatus is one commit-status record. Repeating a context creates a
// newer status; GitHub's raw list returns newest first. Ids rise with every
// status, as GitHub's do. Creator is who posted it: the caller for one
// posted through the API, whoever a test says for one it seeds (no one, if
// unset).
type CommitStatus struct {
	ID          int64
	Context     string
	State       string
	Description string
	TargetURL   string
	Creator     Actor
	CreatedAt   time.Time
}

// WorkflowJob relates one Actions job to a check run and holds its log.
type WorkflowJob struct {
	ID         int64
	CheckRunID int64
	HeadSHA    string
	Name       string
	Conclusion string
	Log        string
}

// SetCheckRun creates or updates a check run on sha.
func (s *Server) SetCheckRun(sha string, run CheckRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.checkRuns[sha], func(v CheckRun) bool { return v.ID == run.ID }); i >= 0 {
		s.checkRuns[sha][i] = run
	} else {
		s.checkRuns[sha] = append(s.checkRuns[sha], run)
	}
}

// SetWorkflowRun creates or updates an Actions workflow run.
func (s *Server) SetWorkflowRun(run WorkflowRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workflowRuns[run.ID] = run
}

// Reruns are the workflow runs whose failed jobs were re-run, in the order
// they were asked for.
func (s *Server) Reruns() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reruns)
}

// SetCommitStatus adds a raw status to sha, newest first, as another
// account posting it would: one without an id gets the next, so it is the
// latest; one without a time is stamped now.
func (s *Server) SetCommitStatus(sha string, status CommitStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addStatus(sha, status)
}

// addStatus records status on sha, newest first, and returns it as stored.
// Callers hold s.mu.
func (s *Server) addStatus(sha string, status CommitStatus) CommitStatus {
	if status.ID == 0 {
		s.nextStatusID++
		status.ID = s.nextStatusID
	}
	s.nextStatusID = max(s.nextStatusID, status.ID)
	if status.CreatedAt.IsZero() {
		status.CreatedAt = s.now()
	}
	s.statuses[sha] = append([]CommitStatus{status}, s.statuses[sha]...)
	return status
}

// Statuses returns every status on sha, newest first.
func (s *Server) Statuses(sha string) []CommitStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.statuses[sha])
}

// LatestStatus returns the newest status of context on sha.
func (s *Server) LatestStatus(sha, context string) (CommitStatus, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.statuses[sha] {
		if st.Context == context {
			return st, true
		}
	}
	return CommitStatus{}, false
}

// statusStates are the states GitHub accepts for a commit status.
var statusStates = []string{"error", "failure", "pending", "success"}

// createCommitStatus answers POST /repos/{o}/{r}/statuses/{sha} as GitHub
// does: 201 with the new status, created by the caller; 422 for a state it
// does not know or a description over 140 characters.
func (s *Server) createCommitStatus(w http.ResponseWriter, r *http.Request) {
	actor := s.caller(r)
	var body struct {
		State       string `json:"state"`
		Context     string `json:"context"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !slices.Contains(statusStates, body.State) || len([]rune(body.Description)) > 140 {
		unprocessable(w, "Validation Failed")
		return
	}
	if body.Context == "" {
		body.Context = "default"
	}
	s.mu.Lock()
	st := s.addStatus(r.PathValue("sha"), CommitStatus{Context: body.Context, State: body.State,
		Description: body.Description, TargetURL: body.TargetURL, Creator: actor})
	s.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, statusJSON(st))
}

// statusJSON renders a status as GitHub's API does.
func statusJSON(v CommitStatus) map[string]any {
	out := map[string]any{"id": v.ID, "context": v.Context, "state": v.State, "description": v.Description,
		"created_at": v.CreatedAt, "updated_at": v.CreatedAt}
	if v.TargetURL != "" {
		out["target_url"] = v.TargetURL
	}
	if v.Creator.Login != "" {
		out["creator"] = v.Creator
	}
	return out
}

// SetWorkflowJob creates or updates a workflow job on runID.
func (s *Server) SetWorkflowJob(runID int64, job WorkflowJob) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.workflowJobs[runID], func(v WorkflowJob) bool { return v.ID == job.ID }); i >= 0 {
		s.workflowJobs[runID][i] = job
	} else {
		s.workflowJobs[runID] = append(s.workflowJobs[runID], job)
	}
	s.jobLogs[job.ID] = job.Log
}

func (s *Server) listCheckRuns(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	s.mu.Lock()
	runs := slices.Clone(s.checkRuns[sha])
	s.mu.Unlock()
	out := make([]map[string]any, 0, len(runs))
	for _, v := range runs {
		appSlug := v.AppSlug
		if appSlug == "" && v.RunID != 0 {
			appSlug = "github-actions"
		}
		suite := v.CheckSuiteID
		if suite == 0 {
			suite = WorkflowRun{ID: v.RunID}.suite()
		}
		out = append(out, map[string]any{
			"id": v.ID, "name": v.Name, "head_sha": sha, "status": v.Status,
			"conclusion":  v.Conclusion,
			"app":         map[string]any{"slug": appSlug},
			"check_suite": map[string]any{"id": suite},
			"details_url": fmt.Sprintf("https://github.com/%s/%s/actions/runs/%d/job/%d",
				r.PathValue("owner"), r.PathValue("repo"), v.RunID, v.ID),
			"output": map[string]any{"title": v.Title, "summary": v.Summary, "text": v.Text,
				"annotations_count": len(v.Annotations)},
		})
	}
	writeJSON(w, map[string]any{"total_count": len(out), "check_runs": out})
}

func (s *Server) listCheckAnnotations(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	var annotations []CheckAnnotation
	found := false
	for _, runs := range s.checkRuns {
		for _, run := range runs {
			if run.ID == id {
				annotations = slices.Clone(run.Annotations)
				found = true
				break
			}
		}
	}
	s.mu.Unlock()
	if !found {
		notFound(w)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if limit > 0 && len(annotations) > limit {
		annotations = annotations[:limit]
	}
	out := make([]map[string]any, 0, len(annotations))
	for _, a := range annotations {
		out = append(out, map[string]any{"path": a.Path, "start_line": a.Line,
			"end_line": a.Line, "message": a.Message})
	}
	writeJSON(w, out)
}

func (s *Server) listCommitStatuses(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	statuses := slices.Clone(s.statuses[r.PathValue("sha")])
	s.mu.Unlock()
	out := make([]map[string]any, 0, len(statuses))
	for _, v := range statuses {
		out = append(out, statusJSON(v))
	}
	writeJSON(w, out)
}

func (s *Server) listWorkflowJobs(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	jobs := slices.Clone(s.workflowJobs[runID])
	s.mu.Unlock()
	out := make([]map[string]any, 0, len(jobs))
	for _, v := range jobs {
		out = append(out, map[string]any{
			"id": v.ID, "name": v.Name, "head_sha": v.HeadSHA, "conclusion": v.Conclusion,
			"check_run_url": fmt.Sprintf("https://api.github.com/repos/%s/%s/check-runs/%d",
				r.PathValue("owner"), r.PathValue("repo"), v.CheckRunID),
		})
	}
	writeJSON(w, map[string]any{"total_count": len(out), "jobs": out})
}

// listWorkflowRuns answers GET /repos/{o}/{r}/actions/runs, filtered by
// check_suite_id as GitHub filters it.
func (s *Server) listWorkflowRuns(w http.ResponseWriter, r *http.Request) {
	suite, _ := strconv.ParseInt(r.URL.Query().Get("check_suite_id"), 10, 64)
	s.mu.Lock()
	var runs []WorkflowRun
	for _, run := range s.workflowRuns {
		if suite == 0 || run.suite() == suite {
			runs = append(runs, run)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(runs, func(a, b WorkflowRun) int { return cmp.Compare(b.ID, a.ID) })
	out := make([]map[string]any, 0, len(runs))
	for _, v := range runs {
		out = append(out, map[string]any{"id": v.ID, "check_suite_id": v.suite(), "head_sha": v.HeadSHA,
			"status": v.Status, "conclusion": v.Conclusion})
	}
	writeJSON(w, map[string]any{"total_count": len(out), "workflow_runs": out})
}

// rerunFailedJobs answers POST /repos/{o}/{r}/actions/runs/{id}/rerun-failed-jobs
// as GitHub does: 404 for a run it does not know, 403 for one still running,
// and otherwise 201, the run queued again as its next attempt.
func (s *Server) rerunFailedJobs(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	run, ok := s.workflowRuns[id]
	running := ok && run.Status != "completed"
	if ok && !running {
		s.reruns = append(s.reruns, id)
		run.Status, run.Conclusion = "queued", ""
		s.workflowRuns[id] = run
	}
	s.mu.Unlock()
	switch {
	case !ok:
		notFound(w)
	case running:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"This workflow is already running"}`)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{}`)
	}
}

func (s *Server) jobLogRedirect(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	_, ok := s.jobLogs[id]
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/api/v3/_logs/%d", id))
	w.WriteHeader(http.StatusFound)
}

func (s *Server) jobLogDownload(w http.ResponseWriter, r *http.Request) {
	// The redirect destination is a credential-less signed object-store URL
	// in GitHub. Catch a client that accidentally forwards its App token.
	if r.Header.Get("Authorization") != "" {
		forbidden(w)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		notFound(w)
		return
	}
	s.mu.Lock()
	log, ok := s.jobLogs[id]
	s.mu.Unlock()
	if !ok {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, log)
}
