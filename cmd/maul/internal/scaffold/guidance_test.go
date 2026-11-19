// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package scaffold

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// ghVariableSet is one printed `gh variable set` command for a gate, so it
// pastes into a shell as one command.
var ghVariableSet = regexp.MustCompile(`^       gh variable set ([A-Z_]+) --repo [A-Za-z0-9._/-]+ --body true$`)

// TestNextStepsSetTheVariablesFromTheModule: the configuration comes from
// the reference module's dotenv output in one command, on the repository
// (never the organization), and only the two gates are set by hand: the
// agent's first, the preview's last, once the Project previews the
// repository.
func TestNextStepsSetTheVariablesFromTheModule(t *testing.T) {
	for _, existing := range []bool{false, true} {
		o := testOptions(existing)
		steps := NextSteps(o, Present{})
		var set []string
		for line := range strings.SplitSeq(steps, "\n") {
			if !strings.Contains(line, "gh variable set") || strings.Contains(line, "gh variable set -f -") {
				continue
			}
			m := ghVariableSet.FindStringSubmatch(line)
			if m == nil {
				t.Errorf("not a pasteable gh command: %q", line)
				continue
			}
			set = append(set, m[1])
		}
		if want := []string{AgentPublishEnabled, PreviewPublishEnabled}; !slices.Equal(set, want) {
			t.Errorf("existing=%v: variables set by hand %v, want %v", existing, set, want)
		}
		for _, wantLine := range []string{
			"       terraform output -raw maul_app_hello_web_variables | gh variable set -f - --repo acme/Hello.Web\n",
			// The platform module's root output, keyed by the slug: the
			// standalone module's output does not exist in that root.
			"       terraform output -json github_variables_dotenv | jq -r '.\"hello-web\"' | " +
				"gh variable set -f - --repo acme/Hello.Web\n",
			`       output "maul_app_hello_web_variables" {`,
			"value = module.maul_app_hello_web.github_variables_dotenv",
			"on this repository only: an organization variable reaches every",
			"agent    123456789012.dkr.ecr.eu-west-2.amazonaws.com/maul/app-envs/hello-web",
			"runtime  123456789012.dkr.ecr.eu-west-2.amazonaws.com/maul/previews/hello-web",
			"imageRepository: 123456789012.dkr.ecr.eu-west-2.amazonaws.com/maul/previews/hello-web",
			`gh workflow run "agent image" --repo acme/Hello.Web`,
			"bump the tag in\n.maul/agent.yaml (toolchain-v2, ...)",
		} {
			if !strings.Contains(steps, wantLine) {
				t.Errorf("existing=%v: next steps lack %q:\n%s", existing, wantLine, steps)
			}
		}
		// The dotenv command comes before the agent gate: the publisher
		// fails closed on any variable it reads unset.
		if strings.Index(steps, "gh variable set -f -") > strings.Index(steps, AgentPublishEnabled+" --repo") {
			t.Errorf("existing=%v: the agent gate is set before the configuration:\n%s", existing, steps)
		}
	}
}

// TestNextStepsPinTheModuleToTheRelease: a release CLI names the reference
// module at its own tag, with every value it knows filled in and the gh
// command beside each value only GitHub knows; a development build says
// to pin the ref.
func TestNextStepsPinTheModuleToTheRelease(t *testing.T) {
	o := testOptions(false)
	o.Release = "0.12.16"
	steps := NextSteps(o, Present{})
	for _, want := range []string{
		`       module "maul_app_hello_web" {` + "\n" + `         source = "` + ModuleSource + `?ref=v0.12.16"` + "\n",
		"git::https://github.com/devthenet-labs/maul.git//deploy/terraform/aws/modules/app?ref=v0.12.16",
		`         slug = "hello-web"`,
		`           owner            = "acme"`,
		`           name             = "Hello.Web"`,
		`           repository_id    = "<id>" # gh api repos/acme/Hello.Web --jq .id`,
		`           owner_id         = "<id>" # gh api repos/acme/Hello.Web --jq .owner.id`,
		`           default_branch   = "main"`,
		`# gh api repos/acme/Hello.Web/actions/oidc/customization/sub --jq .sub_claim_prefix`,
		`github_oidc_provider_arn = "arn:aws:iam::123456789012:oidc-provider/token.actions.githubusercontent.com"`,
		"add an apps\n     entry keyed hello-web instead: its github_variables_dotenv output",
	} {
		if !strings.Contains(steps, want) {
			t.Errorf("next steps lack %q:\n%s", want, steps)
		}
	}
	for _, unwanted := range []string{"development build", "agent_path_prefix", "vX.Y.Z"} {
		if strings.Contains(steps, unwanted) {
			t.Errorf("a release's next steps hold %q:\n%s", unwanted, steps)
		}
	}

	o.Release = ""
	o.AgentPrefix = "acme/agents"
	dev := NextSteps(o, Present{})
	for _, want := range []string{"modules/app?ref=vX.Y.Z\"", "development build: pin ref",
		`agent_path_prefix        = "acme/agents"`} {
		if !strings.Contains(dev, want) {
			t.Errorf("a development build's next steps lack %q:\n%s", want, dev)
		}
	}
}

// TestNextStepsModuleNamesAreIdentifiers: the module and output names are
// terraform identifiers whatever the slug, which may start with a digit.
func TestNextStepsModuleNamesAreIdentifiers(t *testing.T) {
	identifier := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	for _, slug := range []string{"hello-web", "3d-viewer", "a"} {
		o := testOptions(false)
		o.ImageName = slug
		if name := moduleName(o); !identifier.MatchString(name) || !identifier.MatchString(name+"_variables") {
			t.Errorf("slug %s: module name %q is not a terraform identifier", slug, name)
		}
	}
}

// TestNextStepsNameTheDeclaredTag: the agent image is published as the tag
// .maul/agent.yaml declares, the first one for a new declaration and the
// kept one when a forced scaffold kept the repository's own.
func TestNextStepsNameTheDeclaredTag(t *testing.T) {
	for _, tc := range []struct {
		present Present
		tag     string
	}{
		{Present{}, ToolchainTag},
		{Present{Toolchain: "toolchain-v3"}, "toolchain-v3"},
	} {
		steps := NextSteps(testOptions(false), tc.present)
		for _, want := range []string{
			tc.tag + ", the tag .maul/agent.yaml declares",
			"--repository-name maul/app-envs/hello-web --image-ids imageTag=" + tc.tag + "\n",
		} {
			if !strings.Contains(steps, want) {
				t.Errorf("next steps for %+v lack %q:\n%s", tc.present, want, steps)
			}
		}
		if tc.tag != ToolchainTag && strings.Contains(steps, ToolchainTag+",") {
			t.Errorf("next steps for a kept %s still name %s:\n%s", tc.tag, ToolchainTag, steps)
		}
	}
}

// TestNextStepsAdaptExisting: --existing ends with what the application
// must be adapted to, which depends on what it already has.
func TestNextStepsAdaptExisting(t *testing.T) {
	app := NextSteps(testOptions(false), Present{})
	if strings.Contains(app, "Adapt the application") || !strings.Contains(app, "port: 8080") {
		t.Errorf("a new application's next steps:\n%s", app)
	}

	o := testOptions(true)
	bare := NextSteps(o, Present{})
	for _, want := range []string{"Adapt the application", "Add ./Dockerfile", "no go.mod at the repository root",
		`named "runtime image"`, "USER 65532:65532", "port: <the one port your Dockerfile EXPOSEs>"} {
		if !strings.Contains(bare, want) {
			t.Errorf("--existing without a Dockerfile or go.mod lacks %q:\n%s", want, bare)
		}
	}
	full := NextSteps(o, Present{Dockerfile: true, GoMod: true})
	for _, want := range []string{"./Dockerfile is the runtime image runtime-image.yml builds", "keep both"} {
		if !strings.Contains(full, want) {
			t.Errorf("--existing with a Dockerfile and go.mod lacks %q:\n%s", want, full)
		}
	}
	if strings.Contains(full, "an earlier init app's") {
		t.Errorf("--existing names an earlier scaffold's ci.yml that is not there:\n%s", full)
	}

	// Over an earlier full scaffold (a template's copy), its ci.yml keeps
	// building a runtime image nothing publishes: say so, and how to undo it.
	over := NextSteps(o, Present{Dockerfile: true, GoMod: true, ScaffoldCI: true})
	for _, want := range []string{".github/workflows/ci.yml is an earlier init app's",
		`nothing publishes
    now (publish-images.yml follows "runtime image")`, "leaves\n    out runtime-image.yml",
		"delete runtime-image.yml and scaffold\n    again without --existing",
		"removing .maul/agent.yaml and .maul/Dockerfile"} {
		if !strings.Contains(over, want) {
			t.Errorf("--existing over an earlier scaffold lacks %q:\n%s", want, over)
		}
	}
}

// TestNextStepsGolden pins the printed next steps, byte for byte, for a new
// application and for --existing (run with -update to accept a change).
func TestNextStepsGolden(t *testing.T) {
	for name, existing := range map[string]bool{"app": false, "existing": true} {
		t.Run(name, func(t *testing.T) {
			got := NextSteps(testOptions(existing), Present{})
			path := filepath.Join("testdata", "nextsteps", name+".txt")
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s (run with -update to create): %v", path, err)
			}
			if got != string(want) {
				t.Errorf("next steps differ from %s (run with -update to accept):\n--- want\n%s\n--- got\n%s",
					path, want, got)
			}
		})
	}
}

// TestNextStepsEndWithTheProject: the last numbered step adds the
// repository to its Project and the App's installation, then checks the
// Project, so a person is never left without the command that comes next.
func TestNextStepsEndWithTheProject(t *testing.T) {
	steps := NextSteps(testOptions(false), Present{})
	want := "  5. Add the repository to its Project:\n" +
		"       maul setup project <name> ... --repo hello-web=acme/Hello.Web\n" +
		"       kubectl apply -f <name>.project.yaml\n" +
		"     add acme/Hello.Web to the GitHub App's installation, then check the Project:\n" +
		"       GH_TOKEN=$(gh auth token) maul check project <name> -n <namespace>\n"
	if !strings.Contains(steps, want) {
		t.Errorf("next steps lack %q:\n%s", want, steps)
	}
	if strings.Index(steps, "  4. ") > strings.Index(steps, "  5. ") {
		t.Errorf("step 5 is not after step 4:\n%s", steps)
	}
}

// TestProjectRepoArg: maul setup project keys a repository by its lowercased
// name, which the Project CRD holds to a DNS label of at most 16 characters;
// a name that is not one is printed with a key that is, never as a command
// the CLI would refuse.
func TestProjectRepoArg(t *testing.T) {
	for _, tt := range []struct{ owner, name, image, want string }{
		{"acme", "app", "app", "acme/app"},
		{"acme", "Shop-API", "shop-api", "acme/Shop-API"},
		{"acme", "Hello.Web", "hello-web", "hello-web=acme/Hello.Web"},
		{"acme", "a-very-long-repository", "a-very-long-repository", "<key>=acme/a-very-long-repository"},
		{"acme", "trailing-", "trailing", "trailing=acme/trailing-"},
		{"acme", "under_score", "under_score", "<key>=acme/under_score"},
	} {
		o := Options{Repo: Repo{Owner: tt.owner, Name: tt.name}, ImageName: tt.image}
		if got := projectRepoArg(o); got != tt.want {
			t.Errorf("projectRepoArg(%s/%s, image %s) = %q, want %q", tt.owner, tt.name, tt.image, got, tt.want)
		}
	}
}
