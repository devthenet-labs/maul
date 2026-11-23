// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intentview

import v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"

// ProjectConditions are the Project conditions the status page's project
// settings show, in the order it lists them: whether the Project runs,
// whether its intents are verified, and whether its approvers are resolved.
// A test in internal/controller/intent pins each reason the project
// reconciler sets on them to wording of its own here.
var ProjectConditions = []string{
	v1alpha1.ConditionReady, "VerificationOff", v1alpha1.ConditionApproversResolved,
}

// projectConditionText is the public wording per Project condition and
// reason; the "" reason is the condition's fallback. Never a condition's
// message, which names repositories, errors and flags.
var projectConditionText = map[string]map[string]string{
	v1alpha1.ConditionReady: {
		"Validated":                    "the project is ready: its repositories resolve and the App can act on them",
		"UnsupportedRepositories":      "the project lists several repositories, and multi-repository intents are off",
		"HarnessNotEnabled":            "the project asks for a harness the cluster does not run intents on",
		"ModelNotAllowed":              "the project chooses a model the cluster does not run its intents on",
		"RunBudgetOverHourlyLimit":     "a run's cost budget is more than half the cluster's hourly cost limit",
		"RunBudgetUnderRequestReserve": "an evaluator run's cost budget is too small for even two of its requests",
		"ForgeSecretUnreadable":        "the cluster cannot read the GitHub credential the project needs",
		"ForgeUnresolved":              "a repository of the project is covered by no single GitHub connection",
		"AppNotInstalled":              "the GitHub App is not installed on a repository with what intents need",
		"AmbiguousIntentRepository":    "another project uses the same intent repository and trigger label",
		"":                             "the project is not ready",
	},
	"VerificationOff": {
		"EvaluatorOff":                  "the cluster runs without the agent evaluator, so no intent is verified",
		"PreviewsDisabled":              "previews are off on the cluster, and verification runs against them",
		"SurfaceCombinationUnsupported": "the project sets a preview beside a cli or library surface",
		"AgentsToolPodNotEnabled":       "the cluster runs without the tool pod a cli or library surface is verified in",
		"":                              "the project's intents are not verified",
	},
	v1alpha1.ConditionApproversResolved: {
		"Resolved":         "GitHub answered who each approver is",
		"GitHubUnreadable": "GitHub could not be asked who the approvers are; the last answer is kept",
		"":                 "the approvers are not resolved",
	},
}

// ProjectConditionText is the public wording of a Project condition's
// reason: its own, else the condition's fallback; "" for a condition the
// page does not show.
func ProjectConditionText(condition, reason string) string {
	texts, ok := projectConditionText[condition]
	if !ok {
		return ""
	}
	if t, ok := texts[reason]; ok && reason != "" {
		return t
	}
	return texts[""]
}
