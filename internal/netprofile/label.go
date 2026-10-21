// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

// LabelPrefix starts the label an agent Job and its pod carry, set to
// "true", when a profile is applied. The NetworkPolicies on both sides of
// the profile's port select it, so it is the identity the proxy's port
// trusts. The chart's templates spell the same prefix.
const LabelPrefix = "maul.run/network-"

// LabelValue is the only value a profile label is set to.
const LabelValue = "true"

// EnvProfile is the agent container variable that names the applied
// profile, so the agent runner can tell the agent the route exists.
const EnvProfile = "MAUL_NETWORK_PROFILE"

// EnvRoutes is the agent container variable that lists the public routes
// of the applied profile when it has a tunnel ("tunnel" or "go,tunnel"),
// so the agent runner can tell the agent which exist (step 3). It is unset
// on a profile without hosts, whose route is the step-1 go route.
const EnvRoutes = "MAUL_NETWORK_ROUTES"

// EnvHosts is the agent container variable carrying the applied profile's
// tunnel hosts, sorted and comma-joined, for the agent's prompt (step 3).
const EnvHosts = "MAUL_NETWORK_HOSTS"

// EnvProfiles is the variable (and flag) carrying the profile set's JSON in
// the egress proxy and the job controllers.
const EnvProfiles = "MAUL_NETWORK_PROFILES"

// LabelKey is the label key of the profile called name. The name grammar
// keeps it a valid label key: its name part is at most 40 characters.
func LabelKey(name string) string { return LabelPrefix + name }
