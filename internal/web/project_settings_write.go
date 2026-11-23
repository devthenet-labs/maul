// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/intentview"
	"github.com/devthenet-labs/maul/internal/projectsettings"
	"github.com/devthenet-labs/maul/internal/web/auth"
)

// The Project settings page's write side: POST /api/projects/{name}/settings
// takes one application/json ProjectSettingsRequest and answers a
// ProjectSettingsResult. The handler is thin over the pure core's Decide,
// which judges the save on the Project read uncached (the gate first, so a
// person who may not edit learns nothing else); the handler bounds the
// request, writes what Decide decided and records it. The write is a JSON
// merge patch of the changed leaves and the edit annotation only, with the
// resourceVersion it was decided at and under the page's own field manager,
// so it never claims a field Helm or a controller applies and never
// touches status. A conflict (intent-controller writes status every poll)
// reads and decides again, the gate included.
//
// Statuses: 200 made or nothing to make; 400 a body or document the page
// could not have sent; 401 no session; 403 the gate's reason, the
// read-only page, or the API server's refusal; 404 as for the read; 409
// generation or schema changed, a confirmation missing, or conflicts past
// the retries; 413; 415; 422 refused, by Check or by the API server; 429.

// Why a save failed past Decide, beside SettingsReadOnly and the core's
// Result* and gate reasons: fixed keys the page words.
const (
	// SettingsAPIInvalid is the audit reason of a save the API server
	// refused as Invalid (the schema's own rules, cross-field CEL, an
	// admission policy). The page's answer is projectsettings.ResultRefused
	// with a RefusedInvalid refusal per field the refusal names.
	SettingsAPIInvalid = "api-invalid"
	// SettingsClusterRefused: the API server forbade the write (RBAC, an
	// admission policy denying as Forbidden).
	SettingsClusterRefused = "cluster-refused"
	// SettingsConflict: the Project kept changing under the save past every
	// retry. The page reloads.
	SettingsConflict = "conflict"
)

// Bounds of a save.
const (
	// maxSettingsBody caps a save's body: the document of a Project at its
	// schema's limits, as JSON, is well under it.
	maxSettingsBody = 16 << 10
	// settingsSaves saves per settingsSaveWindow per person.
	settingsSaves      = 10
	settingsSaveWindow = time.Minute
	// maxSettingsSavers is how many people the limiter tracks before it
	// sweeps out those with no save in the window.
	maxSettingsSavers = 1024
	// maxLoggedCauses bounds the API server's causes an audit line names.
	maxLoggedCauses = 8
)

// Results of a save as the writes counter names them, beside the reasons.
const (
	settingsResultChanged     = "changed"
	settingsResultUnchanged   = "unchanged"
	settingsResultRateLimited = "rate-limited"
	settingsResultBadRequest  = "bad-request"
	settingsResultError       = "error"
)

// meterScope identifies this package's meter.
const meterScope = "github.com/devthenet-labs/maul/internal/web"

// settingsWritesCounter is made once, on first use, from the global meter
// provider telemetry.Init installs.
var settingsWritesCounter = sync.OnceValue(func() metric.Int64Counter {
	c, err := otel.Meter(meterScope).Int64Counter("maul.status.project_settings.writes",
		metric.WithDescription("Project settings saves by result: changed, unchanged, a gate reason, "+
			"refused, confirm-required, generation-changed, schema-changed, invalid-document, api-invalid, "+
			"cluster-refused, conflict, read-only, rate-limited, bad-request, error"))
	if err != nil {
		otel.Handle(err)
	}
	return c
})

func countSettingsWrite(ctx context.Context, result string) {
	if c := settingsWritesCounter(); c != nil {
		c.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
	}
}

// handleProjectSettingsSave saves one Project's settings.
func (s *Server) handleProjectSettingsSave(w http.ResponseWriter, r *http.Request) {
	id, ok := s.identify(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	name := r.PathValue("name")
	key, err := s.settingsVisible(r, id, name)
	switch {
	case errors.Is(err, errNotVisible):
		notFound(w)
		return
	case err != nil:
		s.log.LogAttrs(ctx, slog.LevelError, "load project settings", slog.String("project", name),
			slog.Any("error", err))
		countSettingsWrite(ctx, settingsResultError)
		http.Error(w, "failed to load the project's settings", http.StatusInternalServerError)
		return
	}
	if s.settings.readOnly {
		res := emptySettingsResult(SettingsReadOnly)
		s.logSettingsRefused(ctx, name, id, res, nil)
		answerSettings(w, http.StatusForbidden, res)
		return
	}
	if !s.settings.saves.allow(id.Username, s.now()) {
		countSettingsWrite(ctx, settingsResultRateLimited)
		http.Error(w, "too many saves: at most 10 a minute", http.StatusTooManyRequests)
		return
	}
	req, code, msg := decodeSettingsRequest(w, r)
	if code != 0 {
		countSettingsWrite(ctx, settingsResultBadRequest)
		http.Error(w, msg, code)
		return
	}
	d, err := s.saveSettings(ctx, key, req, id.GitHubID)
	s.answerSave(w, r, name, id, req.Generation, d, err)
}

// decodeSettingsRequest reads a save: Content-Type application/json, at
// most maxSettingsBody bytes, one object naming no field the document
// lacks. code is the status to refuse it with, 0 when it is read.
func decodeSettingsRequest(w http.ResponseWriter, r *http.Request) (projectsettings.ProjectSettingsRequest, int,
	string) {
	var req projectsettings.ProjectSettingsRequest
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return req, http.StatusUnsupportedMediaType, "the body must be application/json"
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSettingsBody))
	dec.DisallowUnknownFields()
	tooBig := func(err error) bool {
		var mb *http.MaxBytesError
		return errors.As(err, &mb)
	}
	if err := dec.Decode(&req); err != nil {
		if tooBig(err) {
			return req, http.StatusRequestEntityTooLarge, "the body is too large"
		}
		return req, http.StatusBadRequest, "the body must be one {generation, schema, settings, confirm} object " +
			"naming no other field"
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if tooBig(err) {
			return req, http.StatusRequestEntityTooLarge, "the body is too large"
		}
		return req, http.StatusBadRequest, "the body must be one JSON object"
	}
	return req, 0, ""
}

// saveSettings decides the save on the Project read uncached and writes
// what was decided, reading and deciding again on a conflict. The decision
// returned is the last one made, so an error from the write is told against
// the very object that was sent.
func (s *Server) saveSettings(ctx context.Context, key types.NamespacedName, req projectsettings.ProjectSettingsRequest,
	githubID int64) (projectsettings.Decision, error) {
	var d projectsettings.Decision
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var live v1alpha1.Project
		if err := s.reader.Get(ctx, key, &live); err != nil {
			return err
		}
		d = projectsettings.Decide(&live, req, s.settings.policy, githubID, s.now())
		if d.Patched == nil {
			return nil
		}
		patch := client.MergeFromWithOptions(&live, client.MergeFromWithOptimisticLock{})
		if err := s.client.Patch(ctx, d.Patched, patch, client.FieldOwner(projectsettings.FieldManager)); err != nil {
			return err
		}
		// The page waits for intent-controller to observe this generation.
		d.Result.Generation = d.Patched.Generation
		return nil
	})
	return d, err
}

// answerSave answers the save and records it.
func (s *Server) answerSave(w http.ResponseWriter, r *http.Request, name string, id auth.Identity, from int64,
	d projectsettings.Decision, err error) {
	ctx := r.Context()
	res := d.Result
	ensureSettingsLists(&res)
	switch {
	case err == nil && d.Patched != nil:
		s.logSettingsChanged(ctx, name, id, from, d)
		answerSettings(w, http.StatusOK, res)
	case err == nil && res.Reason == "":
		countSettingsWrite(ctx, settingsResultUnchanged)
		answerSettings(w, http.StatusOK, res)
	case err == nil:
		s.logSettingsRefused(ctx, name, id, res, nil)
		answerSettings(w, settingsStatus(res.Reason), res)
	case apierrors.IsNotFound(err):
		notFound(w)
	case apierrors.IsConflict(err):
		res.Reason = SettingsConflict
		s.logSettingsRefused(ctx, name, id, res, nil)
		answerSettings(w, http.StatusConflict, res)
	case apierrors.IsInvalid(err):
		causes := statusCauses(err)
		res.Reason, res.Refusals = projectsettings.ResultRefused, invalidRefusals(causes, d.Patched, res.Changes)
		audit := res
		audit.Reason = SettingsAPIInvalid
		s.logSettingsRefused(ctx, name, id, audit, causes)
		answerSettings(w, http.StatusUnprocessableEntity, res)
	case apierrors.IsForbidden(err):
		res.Reason = SettingsClusterRefused
		s.log.LogAttrs(ctx, slog.LevelWarn, "project settings write forbidden by the API server",
			slog.String("project", name), slog.Any("error", err))
		s.logSettingsRefused(ctx, name, id, res, nil)
		answerSettings(w, http.StatusForbidden, res)
	default:
		s.log.LogAttrs(ctx, slog.LevelError, "save project settings", slog.String("project", name),
			slog.String("user", id.Username), slog.Any("error", err))
		countSettingsWrite(ctx, settingsResultError)
		http.Error(w, "failed to save the project's settings", http.StatusInternalServerError)
	}
}

// settingsStatus is the status a refused decision is answered with.
func settingsStatus(reason string) int {
	switch reason {
	case projectsettings.ReasonNoGitHubIdentity, projectsettings.ReasonNotApprover,
		projectsettings.ReasonNoWriteAccess, projectsettings.ReasonApproversUnresolved,
		projectsettings.ReasonApproversStale:
		return http.StatusForbidden
	case projectsettings.ResultGenerationChanged, projectsettings.ResultSchemaChanged,
		projectsettings.ResultConfirmRequired:
		return http.StatusConflict
	case projectsettings.ResultRefused:
		return http.StatusUnprocessableEntity
	default: // projectsettings.ResultInvalidDocument
		return http.StatusBadRequest
	}
}

func answerSettings(w http.ResponseWriter, code int, res projectsettings.ProjectSettingsResult) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(res)
}

func emptySettingsResult(reason string) projectsettings.ProjectSettingsResult {
	res := projectsettings.ProjectSettingsResult{Reason: reason}
	ensureSettingsLists(&res)
	return res
}

// ensureSettingsLists keeps every list of an answer a list, never null.
func ensureSettingsLists(res *projectsettings.ProjectSettingsResult) {
	if res.Changes == nil {
		res.Changes = []projectsettings.ProjectSettingsChange{}
	}
	if res.Refusals == nil {
		res.Refusals = []projectsettings.ProjectSettingsRefusal{}
	}
	if res.Confirm == nil {
		res.Confirm = []projectsettings.ProjectSettingsConfirmation{}
	}
}

// statusCauses are the causes of the API server's answer.
func statusCauses(err error) []metav1.StatusCause {
	var st apierrors.APIStatus
	if !errors.As(err, &st) {
		return nil
	}
	if d := st.Status().Details; d != nil {
		return d.Causes
	}
	return nil
}

// repositoryIndex reads the leading repositories[i] of a path below spec.
var (
	repositoryIndex = regexp.MustCompile(`^repositories\[(\d+)\]`)
	pathIndex       = regexp.MustCompile(`\[[^\]]*\]`)
)

// invalidRefusals tells the API server's Invalid answer as refusals the page
// can put beside its inputs: for each cause, every changed field at or
// below the path it names, or above it (a cause inside a field), and for a
// path no change is at the path itself. A cross-field rule names the path it
// is declared on (spec.limits), so it blames each change there. A cause on
// the whole spec, outside it, or no cause at all (an admission policy's
// denial carries none) is one refusal with no field. Never empty, never a
// message: the API server's words are the audit line's.
func invalidRefusals(causes []metav1.StatusCause, sent *v1alpha1.Project,
	changes []projectsettings.ProjectSettingsChange) []projectsettings.ProjectSettingsRefusal {
	out := []projectsettings.ProjectSettingsRefusal{}
	add := func(field, item string) {
		for _, r := range out {
			if r.Field == field && r.Item == item {
				return
			}
		}
		out = append(out, projectsettings.ProjectSettingsRefusal{Field: field, Item: item,
			Reason: projectsettings.RefusedInvalid})
	}
	for _, c := range causes {
		path, item := causePath(c.Field, sent)
		if path == "" {
			add("", "")
			continue
		}
		matched := false
		for _, ch := range changes {
			if item != "" && ch.Item != item {
				continue
			}
			if ch.Field == path || strings.HasPrefix(ch.Field, path+".") || strings.HasPrefix(path, ch.Field+".") {
				add(ch.Field, ch.Item)
				matched = true
			}
		}
		if !matched {
			add(path, item)
		}
	}
	if len(out) == 0 {
		add("", "")
	}
	return out
}

// causePath is a cause's field as a path below spec without list indexes,
// and the repository it is in when it is in one (by name, from the object
// sent). "" for the whole spec or anything outside it.
func causePath(field string, sent *v1alpha1.Project) (path, item string) {
	rest, ok := strings.CutPrefix(field, "spec.")
	if !ok {
		return "", ""
	}
	if m := repositoryIndex.FindStringSubmatch(rest); m != nil {
		if i, err := strconv.Atoi(m[1]); err == nil && sent != nil && i < len(sent.Spec.Repositories) {
			item = sent.Spec.Repositories[i].Name
		}
		rest = "repositories" + rest[len(m[0]):]
	}
	return pathIndex.ReplaceAllString(rest, ""), item
}

// settingsAuditChange is one change as the audit line records it.
type settingsAuditChange struct {
	Field string `json:"field"`
	Item  string `json:"item"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// settingsAuditRefusal is one refusal as the audit line records it.
type settingsAuditRefusal struct {
	Field  string `json:"field"`
	Item   string `json:"item"`
	Reason string `json:"reason"`
}

// logSettingsChanged is the audit line of a save made, and counts it: the
// editor's account, the approver login intent-controller resolved it to,
// the generation it was decided at (the page's, which Decide held the live
// one to) and the one it made, and every change.
func (s *Server) logSettingsChanged(ctx context.Context, name string, id auth.Identity, from int64,
	d projectsettings.Decision) {
	changes := make([]settingsAuditChange, 0, len(d.Result.Changes))
	for _, c := range d.Result.Changes {
		changes = append(changes, settingsAuditChange{Field: c.Field, Item: c.Item, From: c.From, To: c.To})
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "project settings changed", slog.String("project", name),
		slog.String("user", id.Username), slog.Int64("github_id", id.GitHubID), slog.String("login", d.Gate.Login),
		slog.Int64("generation", from), slog.Int64("new_generation", d.Result.Generation),
		slog.Any("changes", changes))
	countSettingsWrite(ctx, settingsResultChanged)
}

// logSettingsRefused is the audit line of a save not made, and counts it.
// causes are the API server's, bounded, for a refusal of its own.
func (s *Server) logSettingsRefused(ctx context.Context, name string, id auth.Identity,
	res projectsettings.ProjectSettingsResult, causes []metav1.StatusCause) {
	refusals := make([]settingsAuditRefusal, 0, len(res.Refusals))
	for _, r := range res.Refusals {
		refusals = append(refusals, settingsAuditRefusal{Field: r.Field, Item: r.Item, Reason: r.Reason})
	}
	attrs := []slog.Attr{slog.String("project", name), slog.String("user", id.Username),
		slog.Int64("github_id", id.GitHubID), slog.String("reason", res.Reason),
		slog.Int64("generation", res.Generation), slog.Any("refusals", refusals)}
	if len(causes) > 0 {
		said := make([]string, 0, min(len(causes), maxLoggedCauses))
		for _, c := range causes[:min(len(causes), maxLoggedCauses)] {
			said = append(said, intentview.Text(c.Field+": "+c.Message, maxSettingsRef))
		}
		attrs = append(attrs, slog.Any("causes", said))
	}
	s.log.LogAttrs(ctx, slog.LevelInfo, "project settings refused", attrs...)
	countSettingsWrite(ctx, res.Reason)
}

// saveLimiter bounds each person's saves to settingsSaves in any
// settingsSaveWindow, by the times of the saves it admitted. It is driven
// by the server's clock.
type saveLimiter struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

func newSaveLimiter() *saveLimiter { return &saveLimiter{seen: map[string][]time.Time{}} }

// allow admits one save by user at now, or refuses it without counting it.
func (l *saveLimiter) allow(user string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-settingsSaveWindow)
	recent := inWindow(l.seen[user], cut)
	if len(recent) >= settingsSaves {
		l.seen[user] = recent
		return false
	}
	l.seen[user] = append(recent, now)
	if len(l.seen) > maxSettingsSavers {
		for u, ts := range l.seen {
			if ts = inWindow(ts, cut); len(ts) == 0 {
				delete(l.seen, u)
			} else {
				l.seen[u] = ts
			}
		}
	}
	return true
}

// inWindow drops the times at or before cut from ts, which is in order.
func inWindow(ts []time.Time, cut time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	return ts[i:]
}
