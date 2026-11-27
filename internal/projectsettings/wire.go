// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectsettings

// Schema is the version of the editable document (ProjectSettings). A save
// echoes the schema its page was served with, so a page cached from before a
// field was added can never clear a field it does not know: a save whose
// schema is not this one is refused, and the page reloads.
const Schema = 2

// ProjectSettings is the editable document: every setting of a Project the
// status page may change, and nothing else. The page is served it (Read)
// and posts the whole of it back; Apply writes only what differs from the
// live Project, so the page needs no notion of a field it left alone.
//
// Durations are whole seconds and money is micro-USD, as integers. Zero
// means unset wherever the Project's own field says so; the three fields
// where zero is a value (no rounds at all) are null when unset.
type ProjectSettings struct {
	// Approvers are spec.approvers.logins.
	Approvers []string `json:"approvers"`
	// Verification is the evaluator's part of spec.verification.
	Verification ProjectSettingsVerification `json:"verification"`
	// Models are spec.models.
	Models ProjectSettingsModels `json:"models"`
	// Limits are the per-intent limits of spec.limits.
	Limits ProjectSettingsLimits `json:"limits"`
	// Stages are the per-stage limits of spec.limits.
	Stages ProjectSettingsStages `json:"stages"`
	// Delegation is spec.delegation.
	Delegation ProjectSettingsDelegation `json:"delegation"`
	// Checks are spec.checks.
	Checks ProjectSettingsChecks `json:"checks"`
	// Repositories are the Project's repositories by name, each with the
	// one field of it that may change. A save may leave a repository out
	// (it is left alone) and may name none the Project does not have.
	Repositories []ProjectSettingsRepository `json:"repositories"`
}

// ProjectSettingsVerification is the editable part of spec.verification.
type ProjectSettingsVerification struct {
	// Enabled is verification.enabled.
	Enabled bool `json:"enabled"`
	// Handoff is verification.handoff: "gated", "early", or "" for the
	// install's default.
	Handoff string `json:"handoff"`
	// HandoffMaxSeconds is verification.handoffMax; 0 is unset.
	HandoffMaxSeconds int64 `json:"handoffMaxSeconds"`
	// MaxToolCalls is verification.maxToolCalls; 0 is unset.
	MaxToolCalls int32 `json:"maxToolCalls"`
}

// ProjectSettingsModels are spec.models: one canonical model per stage, ""
// for the install's.
type ProjectSettingsModels struct {
	Plan     string `json:"plan"`
	Build    string `json:"build"`
	Evaluate string `json:"evaluate"`
}

// ProjectSettingsLimits are the per-intent limits.
type ProjectSettingsLimits struct {
	// MaxCostMicroUSD is limits.maxCostMicroUSD; 0 is the schema default.
	MaxCostMicroUSD int64 `json:"maxCostMicroUSD"`
	// MaxEvaluateCostMicroUSD is limits.maxEvaluateCostMicroUSD; 0 is unset.
	MaxEvaluateCostMicroUSD int64 `json:"maxEvaluateCostMicroUSD"`
	// MaxRevisions is limits.maxRevisions; null is the schema default.
	MaxRevisions *int32 `json:"maxRevisions"`
	// MaxCheckFixes is limits.maxCheckFixes; null is the schema default.
	MaxCheckFixes *int32 `json:"maxCheckFixes"`
	// MaxVerifyFixes is limits.maxVerifyFixes; null is unset.
	MaxVerifyFixes *int32 `json:"maxVerifyFixes"`
	// MaxVerifications is limits.maxVerifications; 0 is unset.
	MaxVerifications int32 `json:"maxVerifications"`
	// MaxActiveIntents is limits.maxActiveIntents; 0 is the schema default.
	MaxActiveIntents int32 `json:"maxActiveIntents"`
}

// ProjectSettingsStages are the per-stage limits, in spec.limits' spelling
// of the stages.
type ProjectSettingsStages struct {
	Plan     ProjectSettingsStage `json:"plan"`
	Build    ProjectSettingsStage `json:"build"`
	Revise   ProjectSettingsStage `json:"revise"`
	Evaluate ProjectSettingsStage `json:"evaluate"`
}

// ProjectSettingsStage is one stage's limits. Zero means the controller's
// own ceiling for the stage (or, for the cost, the ceiling it is spent
// against).
type ProjectSettingsStage struct {
	MaxTurns        int32 `json:"maxTurns"`
	TokenBudget     int64 `json:"tokenBudget"`
	MaxCostMicroUSD int64 `json:"maxCostMicroUSD"`
	TimeoutSeconds  int64 `json:"timeoutSeconds"`
}

// ProjectSettingsDelegation is spec.delegation: the Project's opt-in to
// delegation in its build and revise runs. A switch takes effect only where
// intent-controller allows it (ProjectSettingsOptions).
type ProjectSettingsDelegation struct {
	// Subagents is delegation.subagents.
	Subagents bool `json:"subagents"`
	// Workflows is delegation.workflows; it needs Subagents.
	Workflows bool `json:"workflows"`
}

// ProjectSettingsChecks are spec.checks.
type ProjectSettingsChecks struct {
	// Fix is checks.fix.
	Fix []string `json:"fix"`
	// TimeoutSeconds is checks.timeout; 0 is the schema default.
	TimeoutSeconds int64 `json:"timeoutSeconds"`
	// RerunFailed is checks.rerunFailed.
	RerunFailed bool `json:"rerunFailed"`
}

// ProjectSettingsRepository is one repository's editable setting.
type ProjectSettingsRepository struct {
	// Name is the repository's key: which entry this is, never a change.
	Name string `json:"name"`
	// AgentResourceClass is repositories[].agentResourceClass; "" is the
	// default size.
	AgentResourceClass string `json:"agentResourceClass"`
}

// ProjectSettingsView is what the settings page is served.
type ProjectSettingsView struct {
	// Project is the Project's name.
	Project string `json:"project"`
	// Generation is the spec generation the settings were read at: a save
	// echoes it, and is refused when the spec has changed since.
	Generation int64 `json:"generation"`
	// ObservedGeneration is the generation intent-controller last acted on;
	// after a save the page waits for it to reach the new generation.
	ObservedGeneration int64 `json:"observedGeneration"`
	// Schema is Schema.
	Schema int `json:"schema"`
	// CanEdit reports that the viewer may save; Reason is the fixed key
	// why not otherwise (a Reason* constant).
	CanEdit bool   `json:"canEdit"`
	Reason  string `json:"reason"`
	// Settings are the editable settings as they are now.
	Settings ProjectSettings `json:"settings"`
	// Fields are the bounds of every editable field.
	Fields []ProjectSettingsField `json:"fields"`
	// Approvers are the approver logins with what GitHub said of each.
	Approvers []ProjectSettingsApprover `json:"approvers"`
	// ApproversResolvedAt is when GitHub last answered for the approvers
	// (RFC 3339, "" when never), and ApproversStale that it is too long
	// ago to act on.
	ApproversResolvedAt string `json:"approversResolvedAt"`
	ApproversStale      bool   `json:"approversStale"`
	// Options are what the controller admits: the choices on offer.
	Options ProjectSettingsOptions `json:"options"`
	// Allow are the operator's switches.
	Allow ProjectSettingsAllow `json:"allow"`
	// Facts are the Project's settings the page shows and never changes.
	Facts ProjectSettingsFacts `json:"facts"`
	// LastEdit is the page's last save of this Project, nil when none.
	LastEdit *ProjectSettingsEdit `json:"lastEdit"`
	// Conditions are the Project's conditions in fixed wording. View
	// leaves them empty: the wording is the status server's.
	Conditions []ProjectSettingsCondition `json:"conditions"`
}

// ProjectSettingsField is one editable field's bounds, for the form.
type ProjectSettingsField struct {
	// ID is the field's path below spec ("limits.maxRevisions"); the one
	// per-repository field is "repositories.agentResourceClass".
	ID string `json:"id"`
	// Group is the field's lock group.
	Group string `json:"group"`
	// Kind is how its value is written (a Kind).
	Kind string `json:"kind"`
	// Min and Max are the Project schema's own range: a number's, a
	// duration's in seconds, or a list's item count. Both zero: none.
	Min int64 `json:"min"`
	Max int64 `json:"max"`
	// Default is what an unset number lands on: the schema's or the code's
	// default, or for a stage limit the controller's ceiling. Zero when
	// that is not known or the field is not a number.
	Default int64 `json:"default"`
	// Ceiling is the most a save may raise the field to (the operator's
	// page ceiling, or the controller's own), and Floor the least a save
	// may lower it to. Zero: none. A value already past one may be kept,
	// or moved back toward it.
	Ceiling int64 `json:"ceiling"`
	Floor   int64 `json:"floor"`
	// Locked is why the field cannot be changed at all ("" when it can):
	// a Locked* constant.
	Locked string `json:"locked"`
	// Effect is when a change takes effect (an Effect).
	Effect string `json:"effect"`
	// Managers are the other field managers that apply the field (Helm,
	// kubectl): a change made here has to be made there too.
	Managers []ProjectSettingsManager `json:"managers"`
}

// ProjectSettingsManager is another field manager that applies a field, as
// the Project's managedFields record it, and the operation it applies by,
// which decides what its next apply does to an edit made here.
type ProjectSettingsManager struct {
	// Name is the manager's name ("helm", "kubectl-client-side-apply").
	Name string `json:"name"`
	// Operation is ManagerApply or ManagerUpdate.
	Operation string `json:"operation"`
}

// ProjectSettingsApprover is one approver login and what GitHub said of it.
type ProjectSettingsApprover struct {
	Login string `json:"login"`
	// State is an Approver* constant.
	State string `json:"state"`
	// You reports that the login resolved to the viewer's own account.
	You bool `json:"you"`
	// Managers are the other field managers that apply this login.
	Managers []ProjectSettingsManager `json:"managers"`
}

// ProjectSettingsOptions are what intent-controller admits for the Project
// (status.admits), as the choices the form offers.
type ProjectSettingsOptions struct {
	// Published reports that status.admits is there at all. While it is
	// not, no model or resource class can be chosen and no limit raised.
	Published bool `json:"published"`
	// Models are the models a stage may be given, and DefaultModels what
	// each stage runs when it is given none.
	Models        []string              `json:"models"`
	DefaultModels ProjectSettingsModels `json:"defaultModels"`
	// ResourceClasses are the sizes a repository may pick.
	ResourceClasses []ProjectSettingsClass `json:"resourceClasses"`
	// Evaluator reports that the controller runs the agent evaluator, and
	// CostBudgets that the stages' cost budgets are enforced.
	Evaluator   bool `json:"evaluator"`
	CostBudgets bool `json:"costBudgets"`
	// Subagents and Workflows report that the controller lets the
	// Project's delegation switches take effect: a switch it does not
	// allow can be turned off, never on.
	Subagents bool `json:"subagents"`
	Workflows bool `json:"workflows"`
}

// ProjectSettingsClass is one resource class with its size, as the operator
// wrote it.
type ProjectSettingsClass struct {
	Name          string `json:"name"`
	RequestCPU    string `json:"requestCPU"`
	RequestMemory string `json:"requestMemory"`
	LimitCPU      string `json:"limitCPU"`
	LimitMemory   string `json:"limitMemory"`
}

// ProjectSettingsAllow are the operator's switches: what the page may do
// that is off unless the operator said otherwise.
type ProjectSettingsAllow struct {
	// Approvers lets the page change the approver list.
	Approvers bool `json:"approvers"`
	// DisableEvaluator lets the page turn verification off, hand previews
	// over early, and lower the evaluator's limits under the floors.
	DisableEvaluator bool `json:"disableEvaluator"`
	// EmptyChecks lets the page empty checks.fix.
	EmptyChecks bool `json:"emptyChecks"`
}

// ProjectSettingsFacts are the operator's settings the page shows read-only.
type ProjectSettingsFacts struct {
	IntentRepository       string                          `json:"intentRepository"`
	Repositories           []ProjectSettingsRepositoryFact `json:"repositories"`
	TriggerLabel           string                          `json:"triggerLabel"`
	ApproveLabel           string                          `json:"approveLabel"`
	Harness                string                          `json:"harness"`
	Surfaces               []string                        `json:"surfaces"`
	Suspend                bool                            `json:"suspend"`
	RequireRepositoryImage bool                            `json:"requireRepositoryImage"`
	ManualGatesStatus      bool                            `json:"manualGatesStatus"`
	PostLogs               bool                            `json:"postLogs"`
	ActiveIntents          int32                           `json:"activeIntents"`
}

// ProjectSettingsRepositoryFact is one repository as the operator set it.
type ProjectSettingsRepositoryFact struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Previewed reports that the repository is previewed: at PreviewPath,
	// running PreviewImage on PreviewPort, ready at PreviewReadinessPath.
	Previewed            bool   `json:"previewed"`
	PreviewPath          string `json:"previewPath"`
	PreviewImage         string `json:"previewImage"`
	PreviewPort          int32  `json:"previewPort"`
	PreviewReadinessPath string `json:"previewReadinessPath"`
	// ClassManagers are the other field managers that apply this
	// repository's agentResourceClass.
	ClassManagers []ProjectSettingsManager `json:"classManagers"`
}

// ProjectSettingsEdit is the record of a save, kept on the Project in the
// EditedAnnotation and shown as "last changed by".
type ProjectSettingsEdit struct {
	// GitHubID and Login are the editor: the account id the session
	// carried and the approver login intent-controller resolved to it.
	GitHubID int64  `json:"githubID"`
	Login    string `json:"login"`
	// At is when, RFC 3339 in UTC.
	At string `json:"at"`
	// Fields are the ids of the fields the save changed, and More how many
	// more it changed than fit.
	Fields []string `json:"fields"`
	More   int      `json:"more"`
}

// ProjectSettingsCondition is one Project condition in fixed wording.
type ProjectSettingsCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason"`
	Text   string `json:"text"`
}

// ProjectSettingsRequest is a save.
type ProjectSettingsRequest struct {
	// Generation and Schema are the view's, echoed.
	Generation int64 `json:"generation"`
	Schema     int   `json:"schema"`
	// Settings is the whole editable document as the form now has it.
	Settings ProjectSettings `json:"settings"`
	// Confirm are the keys of the confirmations the person gave.
	Confirm []string `json:"confirm"`
}

// ProjectSettingsResult is the answer to a save.
type ProjectSettingsResult struct {
	// Reason is "" when the save was made (or changed nothing), else the
	// fixed key why not.
	Reason string `json:"reason"`
	// Generation is the Project's generation after the answer.
	Generation int64 `json:"generation"`
	// Changes are what the save changed, or would change.
	Changes []ProjectSettingsChange `json:"changes"`
	// Refusals are the changes that may not be made, each with why.
	Refusals []ProjectSettingsRefusal `json:"refusals"`
	// Confirm are the confirmations the save still needs.
	Confirm []ProjectSettingsConfirmation `json:"confirm"`
}

// ProjectSettingsChange is one field a save changes.
type ProjectSettingsChange struct {
	// Field is the field's id, and Item the repository it is for when the
	// field is per repository.
	Field string `json:"field"`
	Item  string `json:"item"`
	// Group is the field's lock group.
	Group string `json:"group"`
	// From and To are the old and new values as text: a number in
	// decimal, a duration in seconds, a list comma-separated, "" for
	// unset.
	From string `json:"from"`
	To   string `json:"to"`
	// Added and Removed are a list's new and dropped entries.
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	// Effect is when the change takes effect (an Effect).
	Effect string `json:"effect"`
}

// ProjectSettingsRefusal is one change that may not be made.
type ProjectSettingsRefusal struct {
	Field string `json:"field"`
	Item  string `json:"item"`
	// Reason is a Refused* constant.
	Reason string `json:"reason"`
	// Limit is the bound the value passed, in the value's own unit, when
	// the reason is a bound.
	Limit int64 `json:"limit"`
	// Value is what the refusal is about when it is not the whole field:
	// the login, the model, the class.
	Value string `json:"value"`
}

// ProjectSettingsConfirmation is one weakening change a person must confirm
// before it is saved.
type ProjectSettingsConfirmation struct {
	// Key is what a save sends back in confirm. It names the change and
	// its value, so a confirmation given for one change covers no other.
	Key string `json:"key"`
	// Kind is a Confirm* constant.
	Kind  string `json:"kind"`
	Field string `json:"field"`
	// Value is what is confirmed: the login, the new value.
	Value string `json:"value"`
}

// WireTypes is one value of every wire type, for the test that holds each
// to the status page's TypeScript interface of the same name.
func WireTypes() []any {
	return []any{
		ProjectSettings{}, ProjectSettingsVerification{}, ProjectSettingsModels{}, ProjectSettingsLimits{},
		ProjectSettingsStages{}, ProjectSettingsStage{}, ProjectSettingsDelegation{}, ProjectSettingsChecks{},
		ProjectSettingsRepository{},
		ProjectSettingsView{}, ProjectSettingsField{}, ProjectSettingsManager{}, ProjectSettingsApprover{},
		ProjectSettingsOptions{},
		ProjectSettingsClass{}, ProjectSettingsAllow{}, ProjectSettingsFacts{}, ProjectSettingsRepositoryFact{},
		ProjectSettingsEdit{}, ProjectSettingsCondition{}, ProjectSettingsRequest{}, ProjectSettingsResult{},
		ProjectSettingsChange{}, ProjectSettingsRefusal{}, ProjectSettingsConfirmation{},
	}
}
