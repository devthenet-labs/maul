// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

// LabelPrefix starts the label an agent Job and its pod carry, set to
// "true", when a profile is applied. The NetworkPolicies on both sides of
// the profile's port select it, so it is the identity the proxy's port
// trusts. The chart's templates spell the same prefix.
const LabelPrefix = "patchy.bitwisemedia.uk/network-"

// LabelValue is the only value a profile label is set to.
const LabelValue = "true"

// EnvProfile is the agent container variable that names the applied
// profile, so the agent runner can tell the agent the route exists.
const EnvProfile = "PATCHY_NETWORK_PROFILE"

// EnvProfiles is the variable (and flag) carrying the profile set's JSON in
// the egress proxy and the job controllers.
const EnvProfiles = "PATCHY_NETWORK_PROFILES"

// LabelKey is the label key of the profile called name. The name grammar
// keeps it a valid label key: its name part is at most 40 characters.
func LabelKey(name string) string { return LabelPrefix + name }
