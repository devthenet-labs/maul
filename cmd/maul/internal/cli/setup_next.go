// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/devthenet-labs/maul/cmd/maul/internal/ghapp"
)

// setupNext is what `setup github-app` leaves the person to do, rendered as
// the numbered next: block it ends with.
type setupNext struct {
	owner string // the App owner's login, as GitHub spells it
	org   bool   // the owner is an organization
	// installURL is where the App is installed; "" (no slug) points at
	// settingsURL instead.
	installURL  string
	settingsURL string
	secretFile  string // "-" when the Secret went to stdout
	forgeFile   string
	namespace   string
	intents     bool
	// serviceAccount is the one the Secret reader Role binds; "" when no
	// Role was written.
	serviceAccount string
}

// render is the next: block, one numbered command or click per line.
func (n setupNext) render() string {
	var b strings.Builder
	b.WriteString("maul: next:\n")
	switch {
	case n.installURL == "":
		fmt.Fprintf(&b, "  1. Install the App from its page under %s\n", n.settingsURL)
	case n.org:
		fmt.Fprintf(&b, "  1. Install the App on %s (choose the organization, not your own account):\n     %s\n",
			n.owner, n.installURL)
	default:
		fmt.Fprintf(&b, "  1. Install the App on your account %s:\n     %s\n", n.owner, n.installURL)
	}
	if n.intents {
		b.WriteString("     Select the intent repository and every application repository.\n")
	} else {
		b.WriteString("     Select the repositories maul works on.\n")
	}
	forge := shellQuote(n.forgeFile)
	if n.secretFile == "-" {
		fmt.Fprintf(&b, "  2. kubectl apply -f %s\n", forge)
		b.WriteString("     The Secret went to stdout: create it in the cluster from wherever you sent it.\n")
	} else {
		secret := shellQuote(n.secretFile)
		fmt.Fprintf(&b, "  2. kubectl create -f %s && rm %s && kubectl apply -f %s\n", secret, secret, forge)
	}
	// On a first install neither exists yet when the App is created.
	fmt.Fprintf(&b, "     Run it once maul is installed: it needs namespace %s and maul's CRDs.\n", n.namespace)
	if n.intents {
		fmt.Fprintf(&b, "  3. maul setup project <name> --intent-repo %s/<intent-repo> --repo %s/<app-repo> "+
			"--approver <github-login> -n %s\n", n.owner, n.owner, n.namespace)
	}
	switch n.serviceAccount {
	case "":
	case ghapp.DefaultIntentControllerSA:
		// Running again would create a second App, so the fix for this one
		// is the file.
		fmt.Fprintf(&b, "maul: the Role binds ServiceAccount %s in %s; pass --intent-controller-sa if your release "+
			"is not named maul.\nmaul: for the App just created, change the subject's name in %s instead "+
			"(kubectl -n %s get serviceaccounts lists it): another run creates another App.\n",
			n.serviceAccount, n.namespace, forge, n.namespace)
	default:
		fmt.Fprintf(&b, "maul: the Role binds ServiceAccount %s in %s.\n", n.serviceAccount, n.namespace)
	}
	return b.String()
}

// shellSafe is a word a POSIX shell reads as itself.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)

// shellQuote is s as one shell word, so a printed command can be pasted.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// checkOrgOwner asks GitHub, with the person's own token, whether they are
// an Owner of org before an App is created under it: only an Owner can, and
// GitHub's form otherwise creates nothing or an App under their own
// account. A member is refused. No token, or an answer that settles
// nothing, is a warning: the check is a convenience, never a gate maul
// relies on. The token goes on this one request and nowhere else.
func checkOrgOwner(ctx context.Context, opts *Options, deps setupDeps, org string) error {
	token := ""
	if deps.getenv != nil {
		if token = deps.getenv("GH_TOKEN"); token == "" {
			token = deps.getenv("GITHUB_TOKEN")
		}
	}
	if token == "" {
		warnOwner(opts.ErrOut, org, "no GH_TOKEN or GITHUB_TOKEN to check with")
		return nil
	}
	m, err := orgMembership(ctx, deps, org, token)
	if err != nil {
		warnOwner(opts.ErrOut, org, err.Error())
		return nil
	}
	if m.State == "active" && m.Role == "admin" {
		notef(opts.ErrOut, "maul: you are an Owner of %s\n", org)
		return nil
	}
	who := m.Role
	if who == "" {
		who = "member"
	}
	if m.State != "active" {
		who = m.State + " " + who
	}
	return fmt.Errorf("you are a %s, not an Owner, of %s: ask an Owner to run this", who, org)
}

// warnOwner says the Owner check settled nothing, and why.
func warnOwner(w io.Writer, org, why string) {
	notef(w, "maul: warning: you must be an Owner of %s; otherwise GitHub creates nothing, or creates the App "+
		"under your own account (not checked: %s)\n", org, why)
}

// membership is the part of GitHub's org membership the check reads.
type membership struct {
	State string `json:"state"`
	Role  string `json:"role"`
}

// orgMembership is GET /user/memberships/orgs/{org} as token's user. Its
// errors never carry the token.
func orgMembership(ctx context.Context, deps setupDeps, org, token string) (membership, error) {
	var m membership
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(deps.apiURL, "/")+"/user/memberships/orgs/"+url.PathEscape(org), nil)
	if err != nil {
		return m, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := deps.client.Do(req)
	if err != nil {
		return m, fmt.Errorf("asking GitHub failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return m, fmt.Errorf("GitHub answered %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&m); err != nil {
		return m, fmt.Errorf("GitHub's answer did not read: %w", err)
	}
	return m, nil
}
