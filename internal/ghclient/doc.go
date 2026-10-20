// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package ghclient is maul's GitHub REST client: GitHub App and
// personal-access-token authentication, installation-scoped clients and
// short-lived scoped tokens, and a narrow, fake-able surface (IssueStore,
// AlertStore, RepoStore) over the handful of endpoints the controllers
// use. Consumers depend on the per-concern interfaces, never on go-github
// types, so tests fake them with a struct.
//
// Every client shares a rate-limit-aware transport that waits out GitHub's
// advertised primary and secondary rate-limit delays before retrying, so
// callers never see a bare 403/429 that a short pause would have cured.
package ghclient
