// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package cli

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devthenet-labs/maul/cmd/maul/internal/ghapp"
	"github.com/devthenet-labs/maul/cmd/maul/internal/projectsetup"
)

// setupProjectFlags are `setup project`'s flags.
type setupProjectFlags struct {
	intentRepo   string
	repos        []string
	approvers    []string
	triggerLabel string
	approveLabel string
	output       string
	force        bool
}

// newSetupProjectCmd writes a Project manifest to apply with kubectl.
func newSetupProjectCmd(opts *Options) *cobra.Command {
	f := &setupProjectFlags{}
	cmd := &cobra.Command{
		Use:   "project <name>",
		Short: "Write a Project manifest, and print the commands that make it Ready",
		Long: "Write the manifest of a Project: the intent repository whose issues become its\n" +
			"intents, the application repositories its intents build in, and the GitHub logins\n" +
			"whose approval counts. It holds no secret: keep it, and apply it with kubectl.\n\n" +
			"A repository is owner/repo (on github.com) or https://host/owner/repo. Each\n" +
			"--repo has a key, its short name in the names of the runs built in it: the\n" +
			"repository's name lowercased, unless written as key=owner/repo. The first --repo\n" +
			"is also where intents are planned. The name, keys, URLs, approvers and labels\n" +
			"are held to the Project schema's own rules, so a written manifest is one the\n" +
			"cluster admits. Every other field is left to its default; edit the file for\n" +
			"more. intent-controller creates the trigger label (maul:<name> unless\n" +
			"--trigger-label) and the approve label (maul:approved unless --approve-label)\n" +
			"on the intent repository.\n\n" +
			"The manifest goes to -o, default <name>.project.yaml, and an existing file is\n" +
			"never replaced without --force; -o - writes it to stdout. To change a Project\n" +
			"later, run the same command with every flag and --force, then apply the file\n" +
			"again. Nothing here talks to a cluster or to GitHub: the command ends with a\n" +
			"numbered next: block on stderr naming the App installation each repository\n" +
			"needs, the kubectl command to apply the file, and the maul check project\n" +
			"command that says whether the Project is Ready.",
		Example: "  maul setup project overdub --intent-repo overdubstudio/intents --repo overdubstudio/app " +
			"--approver alice -n maul\n" +
			"  maul setup project shop --intent-repo acme/intents --repo acme/shop --repo api=acme/backend-api " +
			"--approver alice --approver bob\n" +
			"  maul setup project shop --intent-repo https://github.com/acme/intents --repo acme/shop " +
			"--approver alice -o - | kubectl apply -f -",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: noFileCompletion,
		RunE: func(_ *cobra.Command, args []string) error {
			return runSetupProject(opts, f, args[0])
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.intentRepo, "intent-repo", "",
		"the repository whose issues are the Project's intents: owner/repo or https://host/owner/repo")
	fl.StringArrayVar(&f.repos, "repo", nil,
		"an application repository, [key=]owner/repo or [key=]https://host/owner/repo (repeatable, 1 to 8)")
	fl.StringArrayVar(&f.approvers, "approver", nil, "a GitHub login whose approval counts (repeatable, 1 to 20)")
	fl.StringVar(&f.triggerLabel, "trigger-label", "", "the label that starts an intent (default maul:<name>)")
	fl.StringVar(&f.approveLabel, "approve-label", "", "the label that approves a plan (default maul:approved)")
	// -o names a file here, not an output format, as for setup github-app.
	fl.StringVarP(&f.output, "output", "o", "",
		"file to write the Project manifest to, or - for stdout (default <name>.project.yaml)")
	fl.BoolVar(&f.force, "force", false, "replace an existing output file")
	for _, name := range []string{"intent-repo", "repo", "approver", "trigger-label", "approve-label"} {
		_ = cmd.RegisterFlagCompletionFunc(name, noFileCompletion)
	}
	return cmd
}

// runSetupProject validates the flags, writes the manifest and says what
// is next.
func runSetupProject(opts *Options, f *setupProjectFlags, name string) error {
	ns := opts.Namespace
	if ns == "" {
		ns = defaultSetupNamespace
	}
	p, err := projectsetup.Plan(projectsetup.Input{Name: name, Namespace: ns, IntentRepo: f.intentRepo,
		Repos: f.repos, Approvers: f.approvers, TriggerLabel: f.triggerLabel, ApproveLabel: f.approveLabel})
	if err != nil {
		return errUsage(err)
	}
	data, err := projectsetup.Render(p)
	if err != nil {
		return err
	}
	out := f.output
	if out == "" {
		out = name + ".project.yaml"
	}
	where := out
	if out == "-" {
		where = "stdout"
		if _, err := opts.Out.Write(data); err != nil {
			return err
		}
	} else if err := ghapp.WriteFile(out, data, f.force); err != nil {
		return err
	}
	notef(opts.ErrOut, "maul: wrote Project %s/%s to %s (trigger label %s, approve label %s; intent-controller "+
		"creates both)\n", ns, name, where, p.Trigger(), p.Approve())
	notef(opts.ErrOut, "%s", projectNext(p, out))
	return nil
}

// projectNext is the next: block `setup project` ends with: where the App
// must be installed, who needs write access, and the commands that apply
// the file and check the Project. file is "-" when the manifest went to
// stdout.
func projectNext(p projectsetup.Project, file string) string {
	var b strings.Builder
	b.WriteString("maul: next:\n")
	b.WriteString("  1. The App must be installed on every repository listed. Add any it cannot reach at:\n")
	var hosts []string
	for _, a := range p.Owners() {
		web := "https://" + a.Host
		fmt.Fprintf(&b, "     %s\n", ghapp.InstallationsURL(web, ghapp.Owner{Org: a.Owner}))
		if !slices.Contains(hosts, a.Host) {
			hosts = append(hosts, a.Host)
		}
	}
	for _, h := range hosts {
		fmt.Fprintf(&b, "     (an account that is a person's, not an organization's: %s)\n",
			ghapp.InstallationsURL("https://"+h, ghapp.Owner{}))
	}
	fmt.Fprintf(&b, "  2. Each approver (%s) needs write access to %s\n", strings.Join(p.Approvers, ", "),
		p.IntentRepository.URL)
	if file == "-" {
		b.WriteString("  3. Pipe the Project printed on stdout to kubectl apply -f -\n")
	} else {
		fmt.Fprintf(&b, "  3. kubectl apply -f %s\n", shellQuote(file))
	}
	fmt.Fprintf(&b, "  4. GH_TOKEN=$(gh auth token) maul check project %s -n %s\n", p.Name, p.Namespace)
	for _, a := range p.Owners() {
		// setup github-app creates Apps on github.com only.
		if a.Host == "github.com" {
			fmt.Fprintf(&b, "maul: no Forge for %s yet? run maul setup github-app --org %s --intents first\n",
				a.Owner, a.Owner)
		}
	}
	return b.String()
}
