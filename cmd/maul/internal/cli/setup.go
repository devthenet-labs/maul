// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/devthenet-labs/maul/cmd/maul/internal/browser"
	"github.com/devthenet-labs/maul/cmd/maul/internal/ghapp"
	"github.com/devthenet-labs/maul/cmd/maul/internal/printer"
)

// defaultSetupNamespace is the Secret's namespace when -n is not given: the
// release namespace the install guide uses.
const defaultSetupNamespace = "maul"

// newSetupCmd is the `setup` verb: create what maul needs outside the
// cluster. Cluster-free, like `dev`, `mirror` and `check image`, except for
// `setup previews --detect`'s read-only look.
func newSetupCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Create what maul needs outside the cluster",
		Long: "Create the things maul needs before it is installed, from your workstation. Nothing\n" +
			"here talks to a cluster unless setup previews --detect asks it to read one: otherwise the\n" +
			"kubeconfig flags are inert, and -n only names the namespace a written manifest carries.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newSetupGitHubAppCmd(opts), newSetupPreviewsCmd(opts))
	return cmd
}

// setupGitHubAppFlags are `setup github-app`'s flags.
type setupGitHubAppFlags struct {
	org         string
	user        bool
	security    bool
	intents     bool
	checks      bool
	rerunFailed bool
	webhookURL  string
	name        string
	homepageURL string
	secretName  string
	output      string
	forgeOutput string
	forgeName   string
	intentSA    string
	force       bool
	noBrowser   bool
	dryRun      bool
	timeout     time.Duration
}

// setupDeps is what `setup github-app` reaches beyond its flags: GitHub's
// web and API hosts, the browser, the terminal a pasted code is read from,
// and the HTTP client the code is exchanged with. Tests fake every one.
type setupDeps struct {
	webURL string
	apiURL string
	open   func(string) error
	stdin  io.Reader
	client *http.Client
	// tempDir holds the --no-browser start page; "" is the system's.
	tempDir string
	// terminal reports whether a stream is a terminal: -o - refuses one.
	terminal func(io.Writer) bool
	// getenv reads the environment: GH_TOKEN or GITHUB_TOKEN for the
	// Owner check. nil reads nothing.
	getenv func(string) string
}

// newSetupGitHubAppCmd creates the GitHub App through the manifest flow.
func newSetupGitHubAppCmd(opts *Options) *cobra.Command {
	f := &setupGitHubAppFlags{}
	cmd := &cobra.Command{
		Use:   "github-app",
		Short: "Create the GitHub App maul authenticates as, and write its Secret",
		Long: "Create the GitHub App maul authenticates as through GitHub's App manifest flow,\n" +
			"and write its credentials as the Secret manifest the Forge and Integration\n" +
			"resources name in spec.secretRef.\n\n" +
			"The App asks for the least that the features you choose use, and nothing\n" +
			"else; choose at least one:\n" +
			"  --security  the findings pipeline: code scanning alerts, issues, contents and\n" +
			"              pull requests write, and the webhook events code_scanning_alert,\n" +
			"              issues, issue_comment and pull_request (needs --webhook-url, the\n" +
			"              integration-controller's https://<host>/github/webhooks).\n" +
			"  --intents   intent-driven development: issues, contents, pull requests and\n" +
			"              statuses write. intent-controller polls GitHub, so intents add no\n" +
			"              webhook.\n" +
			"  --checks    with --intents: checks and actions read, which a\n" +
			"              Project's check-fix rounds (spec.checks.fix) need.\n" +
			"  --rerun-failed\n" +
			"              with --checks: actions write, which a Project's\n" +
			"              spec.checks.rerunFailed needs to re-run the failed jobs of the\n" +
			"              Actions runs behind a failed check once before a check-fix round.\n" +
			"Metadata read comes with every App. The intent permissions are the table\n" +
			"intent-controller proves before a Project is Ready.\n\n" +
			"The flow: maul serves a page on a random 127.0.0.1 port and opens it in your\n" +
			"browser; the page posts the manifest to GitHub's \"create a GitHub App\" form for\n" +
			"--org (you need to be an owner of it) or, with --user, your own account. Check\n" +
			"the form and click \"Create GitHub App\": GitHub sends the browser back to the\n" +
			"local page with a one-time code, which maul accepts only with the state it\n" +
			"started the flow with, exchanges for the App's credentials, and then stops\n" +
			"listening. The page is served once: if your browser says it was served\n" +
			"already, something else read it first, so stop maul and run it again.\n" +
			"With --no-browser nothing listens: maul writes the page to a file you open\n" +
			"in any browser, GitHub sends you back to your GitHub Apps settings, and you\n" +
			"paste that page's address (or just its code) into the terminal. A code works\n" +
			"once, within an hour.\n\n" +
			"With --org and a GitHub token in GH_TOKEN or GITHUB_TOKEN (gh auth token\n" +
			"prints yours), maul first asks GitHub whether you are an Owner of the\n" +
			"organization, and refuses to go on if you are only a member. Without a\n" +
			"token it warns and goes on. The token is used for that one request only.\n\n" +
			"The Secret manifest (--secret-name, default maul-github-<org> lowercased, or\n" +
			"maul-github with --user, in -n, default maul) holds appID, privateKey and, for\n" +
			"an App with a webhook, webhookSecret.\n" +
			"It is written to -o, default <secret-name>.secret.yaml, with mode 0600 (not\n" +
			"enforced on Windows), and an existing file is never replaced without --force.\n" +
			"-o - writes it to stdout instead, to pipe into an encryption tool such as\n" +
			"sops, and refuses a stdout that is a terminal or a file other users can\n" +
			"read (as a shell's > file is under the usual umask: use -o <file>). The\n" +
			"private key is printed nowhere else and GitHub keeps no copy: create or\n" +
			"encrypt the file, then delete it.\n\n" +
			"Beside it, maul writes the forge file (--forge-output, default\n" +
			"<secret-name>.forge.yaml in the Secret file's directory, always a file): no\n" +
			"secret, so keep it. It holds the Forge (--forge-name, default the App owner's\n" +
			"login lowercased) covering the App owner's repositories with that Secret\n" +
			"and, with --intents, a Role and RoleBinding that let intent-controller's\n" +
			"ServiceAccount (--intent-controller-sa, default maul-intent-controller, as\n" +
			"in a release named maul) get exactly that one Secret.\n\n" +
			"Everything else goes to stderr, ending with a numbered next: block: the\n" +
			"link to install the App (\"Only select repositories\" is enough: for\n" +
			"intents, the intent repository and every application repository), the\n" +
			"kubectl command that creates the Secret, deletes its file and applies the\n" +
			"forge file, and, with --intents, the maul setup project command to run next.\n\n" +
			"--dry-run prints the manifest as JSON and creates nothing. github.com only.",
		Example: "  maul setup github-app --org acme --intents --checks\n" +
			"  maul setup github-app --org acme --security --webhook-url https://maul.acme.dev/github/webhooks\n" +
			"  maul setup github-app --org acme --intents --dry-run\n" +
			"  maul setup github-app --org acme --intents -o - | sops --encrypt --input-type yaml " +
			"--output-type yaml /dev/stdin > maul-github-acme.enc.yaml\n" +
			"  maul setup github-app --user --intents --no-browser",
		Args:              cobra.NoArgs,
		ValidArgsFunction: noFileCompletion,
		RunE: func(cmd *cobra.Command, _ []string) error {
			deps := setupDeps{
				webURL:   ghapp.DefaultWebURL,
				apiURL:   ghapp.DefaultAPIURL,
				open:     browser.Open,
				stdin:    cmd.InOrStdin(),
				client:   &http.Client{Timeout: opts.RequestTimeout},
				terminal: printer.IsTerminal,
				getenv:   os.Getenv,
			}
			if opts.setupDeps != nil {
				deps = *opts.setupDeps
			}
			return runSetupGitHubApp(cmd.Context(), opts, f, deps)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.org, "org", "", "organization to create the App under (you must be an owner)")
	fl.BoolVar(&f.user, "user", false, "create the App under your own account instead of an organization")
	fl.BoolVar(&f.security, "security", false, "the findings pipeline: alerts, tracking issues, remediation PRs")
	fl.BoolVar(&f.intents, "intents", false, "intent-driven development (no webhook)")
	fl.BoolVar(&f.checks, "checks", false, "with --intents: the reads check-fix rounds need")
	fl.BoolVar(&f.rerunFailed, "rerun-failed", false,
		"with --checks: actions write, to re-run failed Actions jobs before a check-fix round")
	fl.StringVar(&f.webhookURL, "webhook-url", "",
		"with --security: the integration-controller's https://<host>/github/webhooks")
	fl.StringVar(&f.name, "name", "", "the App's name, at most 34 characters (default maul-<org>, or maul)")
	fl.StringVar(&f.homepageURL, "homepage-url", ghapp.DefaultHomepageURL, "the App's homepage")
	fl.StringVar(&f.secretName, "secret-name", "",
		"name of the Secret written (default maul-github-<org> lowercased, or maul-github with --user)")
	// -o names a file here, not an output format: this command's only output
	// is the Secret manifest. The local flag shadows the global one.
	fl.StringVarP(&f.output, "output", "o", "",
		"file to write the Secret manifest to, or - for stdout (default <secret-name>.secret.yaml)")
	fl.StringVar(&f.forgeOutput, "forge-output", "",
		"file to write the Forge (and with --intents its Secret reader Role) to "+
			"(default <secret-name>.forge.yaml beside the Secret file)")
	fl.StringVar(&f.forgeName, "forge-name", "", "name of the Forge written (default the App owner's login lowercased)")
	fl.StringVar(&f.intentSA, "intent-controller-sa", ghapp.DefaultIntentControllerSA,
		"with --intents: intent-controller's ServiceAccount, which the Role lets read the Secret")
	fl.BoolVar(&f.force, "force", false, "replace an existing output file")
	fl.BoolVar(&f.noBrowser, "no-browser", false,
		"open no browser and listen on no port: write the page to a file and paste the code back")
	fl.BoolVar(&f.dryRun, "dry-run", false, "print the manifest as JSON; create nothing")
	fl.DurationVar(&f.timeout, "timeout", 15*time.Minute, "how long to wait for GitHub to send the browser back")
	_ = cmd.RegisterFlagCompletionFunc("org", noFileCompletion)
	_ = cmd.RegisterFlagCompletionFunc("webhook-url", noFileCompletion)
	_ = cmd.RegisterFlagCompletionFunc("name", noFileCompletion)
	_ = cmd.RegisterFlagCompletionFunc("homepage-url", noFileCompletion)
	_ = cmd.RegisterFlagCompletionFunc("secret-name", noFileCompletion)
	_ = cmd.RegisterFlagCompletionFunc("forge-name", noFileCompletion)
	_ = cmd.RegisterFlagCompletionFunc("intent-controller-sa", noFileCompletion)
	return cmd
}

// setupPlan is a validated `setup github-app` invocation.
type setupPlan struct {
	owner      ghapp.Owner
	config     ghapp.Config
	secretName string
	namespace  string
	output     string // "-" for stdout
	// forgeOutput is the forge file, always a file.
	forgeOutput string
	// forgeName is the Forge's name; "" (--user) takes the App owner's
	// login, known only once the App exists.
	forgeName string
	// serviceAccount is the one the Secret reader Role binds; "" without
	// --intents, which writes no Role.
	serviceAccount string
}

// planSetup validates the flags into a plan; every refusal is a usage error.
func planSetup(opts *Options, f *setupGitHubAppFlags) (setupPlan, error) {
	var p setupPlan
	switch {
	case f.org != "" && f.user:
		return p, errUsage(errors.New("--org and --user are exclusive: the App has one owner"))
	case f.org == "" && !f.user:
		return p, errUsage(errors.New("name the App's owner: --org <organization>, or --user"))
	case f.org != "":
		if err := ghapp.ValidateOrg(f.org); err != nil {
			return p, errUsage(fmt.Errorf("--org: %w", err))
		}
	}
	if f.timeout <= 0 {
		return p, errUsage(errors.New("--timeout must be positive"))
	}
	p.owner = ghapp.Owner{Org: f.org}
	p.namespace = opts.Namespace
	if p.namespace == "" {
		p.namespace = defaultSetupNamespace
	}
	if errs := validation.IsDNS1123Label(p.namespace); len(errs) > 0 {
		return p, errUsage(fmt.Errorf("-n %q: %s", p.namespace, strings.Join(errs, "; ")))
	}
	if err := planFiles(&p, f); err != nil {
		return p, err
	}
	name := f.name
	if name == "" {
		name = ghapp.DefaultName(p.owner)
	}
	p.config = ghapp.Config{
		Features: ghapp.Features{Security: f.security, Intents: f.intents, Checks: f.checks,
			RerunFailed: f.rerunFailed},
		Name:        name,
		HomepageURL: f.homepageURL,
		WebhookURL:  f.webhookURL,
	}
	if _, err := ghapp.Build(p.config); err != nil {
		return p, errUsage(err)
	}
	return p, nil
}

// planFiles fills in what the two files are called and name: the Secret,
// the Forge, the ServiceAccount the Secret reader Role binds, and both
// paths. Everything is checked here, before an App exists.
func planFiles(p *setupPlan, f *setupGitHubAppFlags) error {
	p.secretName = f.secretName
	if p.secretName == "" {
		p.secretName = ghapp.DefaultSecretNameFor(p.owner)
	}
	if err := validateName("--secret-name", p.secretName); err != nil {
		return err
	}
	// The Role and RoleBinding are named after the Secret, within the same
	// 253 characters.
	if n := len(ghapp.SecretReaderName(p.secretName)); n > validation.DNS1123SubdomainMaxLength {
		return errUsage(fmt.Errorf("--secret-name %q is too long: its Secret reader Role %q would have %d "+
			"characters, more than %d", p.secretName, ghapp.SecretReaderName(p.secretName), n,
			validation.DNS1123SubdomainMaxLength))
	}
	p.forgeName = f.forgeName
	if p.forgeName == "" {
		p.forgeName = strings.ToLower(f.org)
	} else if err := validateName("--forge-name", p.forgeName); err != nil {
		return err
	}
	if f.intents {
		if err := validateName("--intent-controller-sa", f.intentSA); err != nil {
			return err
		}
		p.serviceAccount = f.intentSA
	}
	p.output = f.output
	if p.output == "" {
		p.output = p.secretName + ".secret.yaml"
	}
	var err error
	p.forgeOutput, err = forgeOutput(f.forgeOutput, p.output, p.secretName)
	return err
}

// validateName refuses, as a usage error naming flag, a value that is not
// a DNS subdomain, as Kubernetes object names must be.
func validateName(flag, value string) error {
	if errs := validation.IsDNS1123Subdomain(value); len(errs) > 0 {
		return errUsage(fmt.Errorf("%s %q: %s", flag, value, strings.Join(errs, "; ")))
	}
	return nil
}

// forgeOutput is the forge file's path: flag, or <secretName>.forge.yaml
// beside the Secret file (in the working directory with -o -). It is never
// stdout, and never the Secret file itself.
func forgeOutput(flag, output, secretName string) (string, error) {
	path := flag
	switch {
	case path == "-":
		return "", errUsage(errors.New("--forge-output names a file: the forge file is kept and applied " +
			"again, so it is never written to stdout"))
	case path == "" && output == "-":
		path = secretName + ".forge.yaml"
	case path == "":
		path = filepath.Join(filepath.Dir(output), secretName+".forge.yaml")
	}
	if output != "-" && filepath.Clean(path) == filepath.Clean(output) {
		return "", errUsage(fmt.Errorf("--forge-output %s is the Secret file: name another", path))
	}
	return path, nil
}

// runSetupGitHubApp creates the App and writes its Secret.
func runSetupGitHubApp(ctx context.Context, opts *Options, f *setupGitHubAppFlags, deps setupDeps) error {
	plan, err := planSetup(opts, f)
	if err != nil {
		return err
	}
	if f.dryRun {
		return printManifest(opts, plan, deps)
	}
	// Before anything exists on GitHub: an App whose credentials have nowhere
	// to go is an App to delete by hand.
	if plan.output == "-" {
		if err := checkStdout(opts.Out, deps.terminal); err != nil {
			return err
		}
	} else if err := ghapp.CheckWritable(plan.output, f.force); err != nil {
		return err
	}
	if err := ghapp.CheckWritable(plan.forgeOutput, f.force); err != nil {
		return err
	}
	if plan.owner.Org != "" {
		if err := checkOrgOwner(ctx, opts, deps, plan.owner.Org); err != nil {
			return err
		}
	}
	state, err := ghapp.NewState()
	if err != nil {
		return err
	}
	createURL := ghapp.CreateURL(deps.webURL, plan.owner, state)
	var app *ghapp.App
	if f.noBrowser {
		app, err = pasteApp(ctx, opts, plan, deps, createURL, state)
	} else {
		var code string
		if code, err = browserCode(ctx, opts, plan, deps, createURL, state, f.timeout); err == nil {
			app, err = ghapp.Convert(ctx, deps.client, deps.apiURL, code)
			err = convertFailed(err, plan, deps)
		}
	}
	if errors.Is(err, ghapp.ErrNoCode) {
		// The App may exist although its code never arrived: a browser on
		// another machine cannot reach this one's loopback address, and a
		// person can give up after clicking Create.
		return fmt.Errorf("%w; if GitHub created the App, delete it at %s, or within the hour run again with "+
			"--no-browser and paste the code= value from the address GitHub sent the browser to",
			err, ghapp.AppsURL(deps.webURL, plan.owner))
	}
	if err != nil {
		return err
	}
	// The code proves only that it carried this run's state, which a local
	// process could have read: an App another account owns is someone
	// else's, and its key must not become this Forge's credential. A user
	// App has no login to hold it to.
	if plan.owner.Org != "" && !strings.EqualFold(app.Owner, plan.owner.Org) {
		// The App is not under --org's settings: link its own page, which
		// GitHub serves to whoever owns it.
		page := ghapp.AppsURL(deps.webURL, ghapp.Owner{})
		if app.Slug != "" {
			page = ghapp.SettingsURL(deps.webURL, ghapp.Owner{}, app.Slug)
		}
		return fmt.Errorf("refused: the code converts to App %q owned by %q, not by --org %s, so its "+
			"credentials were not written; if you created an App on GitHub's form just now, delete it at %s",
			app.Name, app.Owner, plan.owner.Org, page)
	}
	return writeSecret(opts, plan, deps, app, f.force)
}

// checkStdout refuses, before anything is created, a stdout -o - must not
// write the private key to: a terminal, or a regular file other users can
// read. The shell creates `> file` before maul runs, with its umask
// (typically 0644), and a reader that opens it then keeps reading after any
// chmod, so the file cannot be made private in time; -o <file> creates it
// 0600. A pipe, or a file only its owner can open, is fine.
func checkStdout(w io.Writer, terminal func(io.Writer) bool) error {
	if terminal(w) {
		return errUsage(errors.New("-o - writes the Secret, private key included, to stdout, which is a " +
			"terminal: pipe it into the tool that keeps it (sops, kubectl apply -f -), or name a file"))
	}
	f, ok := w.(*os.File)
	if !ok {
		return nil
	}
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("-o -: stdout: %w", err)
	}
	if info.Mode().IsRegular() && info.Mode().Perm()&0o077 != 0 {
		return errUsage(fmt.Errorf("-o - writes the Secret, private key included, to stdout, which is a file "+
			"other users can read (mode %04o): name the file with -o <file> instead, which maul creates with "+
			"mode 0600", info.Mode().Perm()))
	}
	return nil
}

// convertFailed is err, from exchanging a code, with where to clean up:
// GitHub may have created the App all the same. nil stays nil.
func convertFailed(err error, plan setupPlan, deps setupDeps) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w; if GitHub created the App, delete it or generate a new private key at %s",
		err, ghapp.AppsURL(deps.webURL, plan.owner))
}

// printManifest is --dry-run: the manifest on stdout, where it would go on
// stderr.
func printManifest(opts *Options, plan setupPlan, deps setupDeps) error {
	m, err := ghapp.Build(plan.config)
	if err != nil {
		return errUsage(err)
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(opts.Out, "%s\n", out)
	notef(opts.ErrOut, "maul: dry run: nothing created. The flow posts this manifest to %s with a fresh "+
		"state, and redirect_url set to where GitHub sends the browser back.\n",
		ghapp.CreateURL(deps.webURL, plan.owner, ""))
	return err
}

// browserCode runs the loopback flow: serve the start page, open it, and
// wait for GitHub to send the browser back with the code.
func browserCode(ctx context.Context, opts *Options, plan setupPlan, deps setupDeps, createURL, state string,
	timeout time.Duration) (string, error) {
	cb, err := ghapp.Listen()
	if err != nil {
		return "", err
	}
	defer cb.Close()
	page, err := startPage(plan, cb.RedirectURL(), createURL)
	if err != nil {
		return "", err
	}
	cb.Serve(state, page)
	notef(opts.ErrOut, "maul: opening %s in your browser: it sends the App manifest to GitHub.\n"+
		"maul: check the form there and click \"Create GitHub App\"; waiting up to %s.\n", cb.StartURL(), timeout)
	if err := deps.open(cb.StartURL()); err != nil {
		notef(opts.ErrOut, "maul: no browser opened (%v): open %s yourself.\n", err, cb.StartURL())
	}
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return cb.Wait(wait)
}

// pasteApp runs the flow without a listener: the start page goes to a
// file, GitHub lands the browser on the owner's App settings, and the
// person pastes that address back; its code is exchanged for the App. A
// paste that holds no code for this run asks again, and so does a bare
// code GitHub does not accept: nothing tied it to this run, so it may be
// anything typed at the prompt, while this run's own code is still unspent.
// The terminal closing or the run being cancelled is ErrNoCode.
func pasteApp(ctx context.Context, opts *Options, plan setupPlan, deps setupDeps, createURL,
	state string) (*ghapp.App, error) {
	page, err := startPage(plan, ghapp.AppsURL(deps.webURL, plan.owner), createURL)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(deps.tempDir, "maul-github-app-*.html")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	_, err = file.Write(page)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("write the start page: %w", err)
	}
	notef(opts.ErrOut, "maul: open %s in a browser signed in to GitHub (copy it to that machine if need be).\n"+
		"maul: it sends the App manifest to GitHub; check the form and click \"Create GitHub App\".\n"+
		"maul: GitHub then opens your GitHub Apps settings. Paste that page's address here (or just its "+
		"code=... value):\n", file.Name())
	lines := readLines(deps.stdin)
	for {
		var line string
		select {
		case l, ok := <-lines:
			if !ok {
				return nil, fmt.Errorf("%w: the terminal closed before a code was pasted", ghapp.ErrNoCode)
			}
			line = l
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %w", ghapp.ErrNoCode, context.Cause(ctx))
		}
		code, bare, err := ghapp.ParseCode(line, state)
		if err != nil {
			notef(opts.ErrOut, "maul: %v. Paste the address again (or just its code=... value):\n", err)
			continue
		}
		app, err := ghapp.Convert(ctx, deps.client, deps.apiURL, code)
		if bare && errors.Is(err, ghapp.ErrCodeRejected) {
			notef(opts.ErrOut, "maul: %v. If that was not the code from the address GitHub sent you to, "+
				"paste that address (or just its code=... value):\n", err)
			continue
		}
		return app, convertFailed(err, plan, deps)
	}
}

// readLines delivers r's lines, each at most 4 KiB, closing the channel
// at the end of r or at a line too long. Its reader cannot be cancelled (a
// read from a terminal cannot), so the caller stops listening instead.
func readLines(r io.Reader) <-chan string {
	out := make(chan string)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 4096), 4096)
		for sc.Scan() {
			out <- sc.Text()
		}
	}()
	return out
}

// startPage builds the manifest with redirectURL and renders the page that
// posts it to createURL.
func startPage(plan setupPlan, redirectURL, createURL string) ([]byte, error) {
	cfg := plan.config
	cfg.RedirectURL = redirectURL
	m, err := ghapp.Build(cfg)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return ghapp.StartPage(createURL, raw)
}

// writeSecret writes the Secret manifest and tells the person what is left.
func writeSecret(opts *Options, plan setupPlan, deps setupDeps, app *ghapp.App, force bool) error {
	notef(opts.ErrOut, "maul: created GitHub App %q (ID %d) owned by %s: %s\n", app.Name, app.ID, app.Owner,
		app.HTMLURL)
	m, err := ghapp.Build(plan.config)
	if err != nil {
		return err
	}
	// The App's own settings page: its webhook, permissions and keys.
	settings := ghapp.AppsURL(deps.webURL, plan.owner)
	if app.Slug != "" {
		settings = ghapp.SettingsURL(deps.webURL, plan.owner, app.Slug)
	}
	for _, d := range ghapp.Drift(m, app) {
		notef(opts.ErrOut, "maul: warning: %s\n", d)
	}
	if m.HookAttributes != nil && !app.Credentials.HasWebhookSecret() {
		notef(opts.ErrOut, "maul: warning: GitHub issued no webhook secret, and an Integration refuses a Secret "+
			"without %s: set one on the App's webhook at %s and add it to the Secret.\n", ghapp.KeyWebhookSecret,
			settings)
	}
	data, err := ghapp.SecretManifest(app, plan.secretName, plan.namespace)
	if err != nil {
		return err
	}
	lost := fmt.Sprintf("; the App exists, but its private key was not saved: generate a new one under "+
		"\"Private keys\" at %s", settings)
	keys := "appID, privateKey"
	if app.Credentials.HasWebhookSecret() {
		keys += ", webhookSecret"
	}
	if plan.output == "-" {
		if err := writeStdout(opts.Out, data); err != nil {
			return fmt.Errorf("write the Secret to stdout: %w%s", err, lost)
		}
		notef(opts.ErrOut, "maul: wrote Secret %s/%s (%s) to stdout\n", plan.namespace, plan.secretName, keys)
	} else {
		if err := ghapp.WriteFile(plan.output, data, force); err != nil {
			return fmt.Errorf("%w%s", err, lost)
		}
		notef(opts.ErrOut, "maul: wrote Secret %s/%s (%s) to %s, mode 0600. It holds the only copy of the "+
			"private key: create or encrypt it, then delete it.\n", plan.namespace, plan.secretName, keys, plan.output)
	}
	if err := writeForge(opts, plan, app, force); err != nil {
		return err
	}
	next := setupNext{owner: app.Owner, org: plan.owner.Org != "", settingsURL: settings,
		secretFile: plan.output, forgeFile: plan.forgeOutput, namespace: plan.namespace,
		intents: plan.config.Features.Intents, serviceAccount: plan.serviceAccount}
	if app.Slug != "" {
		next.installURL = ghapp.InstallURL(deps.webURL, app.Slug)
	}
	notef(opts.ErrOut, "%s", next.render())
	return nil
}

// writeForge writes the forge file: the Forge for the App's owner and, with
// --intents, the Role and RoleBinding letting intent-controller read its
// Secret. The Secret is already saved, so a failure here loses no key.
func writeForge(opts *Options, plan setupPlan, app *ghapp.App, force bool) error {
	name := plan.forgeName
	if name == "" {
		name = strings.ToLower(app.Owner)
	}
	data, err := ghapp.ForgeFile(name, plan.namespace, plan.secretName, app.Owner, plan.serviceAccount)
	if err == nil {
		err = ghapp.WriteFile(plan.forgeOutput, data, force)
	}
	if err != nil {
		return fmt.Errorf("%w; the Secret was saved, but the forge file was not: write a Forge %s/%s naming "+
			"Secret %s and org %s by hand", err, plan.namespace, name, plan.secretName, app.Owner)
	}
	what := "Forge " + plan.namespace + "/" + name
	if plan.serviceAccount != "" {
		what += " and the Role letting intent-controller read Secret " + plan.secretName
	}
	notef(opts.ErrOut, "maul: wrote %s to %s. It holds no secret: keep it.\n", what, plan.forgeOutput)
	return nil
}

// writeStdout writes data to w with SIGPIPE caught. A write to a stdout
// pipe whose reader has gone (a tool downstream that failed to start)
// would otherwise end the process silently, before it could say that the
// App exists without a saved key; caught, the write fails with EPIPE.
func writeStdout(w io.Writer, data []byte) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGPIPE)
	defer signal.Stop(sig)
	_, err := w.Write(data)
	return err
}
