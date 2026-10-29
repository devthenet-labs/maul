// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package jobs

import (
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/sandboxprobe"
)

// The agent evaluator's and check reviewer's Jobs (Spec.Role). Both are
// intent runs on maul's own runner image with no tree: no artifact fetch, no
// git init, no repository image, no network profile. Both run without a
// resolver whatever Config.DNS says (the broker, and the evaluator's tool
// pod, are pinned in the hosts file), and both carry v1alpha1.LabelRole on
// the Job and its pod, the selector of their own egress policies. The
// evaluator also carries v1alpha1.LabelPreviewVerification, the value its tool
// pod's ingress policy admits, and its prepare ends with the strict sandbox
// probe over every target, the cluster's resolver included.
const (
	// ToolsHostName is the name the evaluator reaches its tool pod by, pinned
	// in its hosts file at the tool pod's address (Spec.ToolHost).
	ToolsHostName = "maul-tools"
	// ToolsPort is the tool pod's API port.
	ToolsPort = 8090
	// ToolsURLEnv names the evaluator's tool pod URL, which maul-tool reads.
	ToolsURLEnv = "MAUL_TOOLS_URL"

	// roleWorkDir is a role pod's workspace and HOME. The agent's file tools
	// are confined to roleWorkDir/scratch (the harness's SandboxEvaluate
	// posture runs there); agent-runner reads its handoff from
	// roleWorkDir/input, which those tools cannot reach.
	roleWorkDir = "/work"
)

// HostAlias pins one name at one address in a pod's hosts file, without
// resolving anything: the evaluator's tool pod, whose address the
// PreviewVerification's status carries.
type HostAlias struct {
	Name string
	IP   string
}

// ErrRoleRefused wraps every refusal of a Spec's Role and the fields only a
// role Job may carry: a contradiction in controller code, never launched.
var ErrRoleRefused = errors.New("jobs: role refused")

// roles are the Spec.Role values a Job may carry.
var roles = []string{v1alpha1.RoleEvaluator, v1alpha1.RoleCheckReview}

// roleRefusal reports why a Spec's role, or the fields that belong to a
// role, cannot be built, or nil when they can (a Job with none of them
// always can).
func roleRefusal(cfg Config, spec Spec) error {
	if spec.Role == "" {
		if spec.ToolHost != nil || spec.Evaluation != "" {
			return fmt.Errorf("%w: a tool host or a verification UID belongs to an evaluator or check-reviewer "+
				"Job, and this Job has no role", ErrRoleRefused)
		}
		return nil
	}
	var errs []error
	refuse := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if !slices.Contains(roles, spec.Role) {
		refuse("role %q is not one of %v", spec.Role, roles)
	}
	if spec.Kind != kindIntent {
		refuse("a %s Job is an intent run, not kind %q", spec.Role, spec.Kind)
	}
	if spec.ArtifactURL != "" || spec.ArtifactDigest != "" || len(spec.Trees) > 0 {
		refuse("a %s Job reads no tree and fetches no artifact", spec.Role)
	}
	if spec.RunnerImage != "" {
		refuse("a %s Job runs maul's runner image, never a repository-declared one (%q)", spec.Role, spec.RunnerImage)
	}
	if spec.NetworkProfile != nil {
		refuse("a %s Job never carries a network profile", spec.Role)
	}
	if spec.Evaluation != "" {
		if e := validation.IsValidLabelValue(spec.Evaluation); len(e) > 0 {
			refuse("verification UID %q is not a label value: %s", spec.Evaluation, strings.Join(e, "; "))
		}
	}
	if spec.Role == v1alpha1.RoleEvaluator {
		errs = append(errs, evaluatorRefusals(cfg, spec)...)
	} else if spec.ToolHost != nil {
		refuse("only an evaluator Job reaches a tool pod")
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrRoleRefused, errors.Join(errs...))
}

// evaluatorRefusals are the evaluator's own requirements: its verification,
// its tool pod and the resolver its strict probe must find closed.
func evaluatorRefusals(cfg Config, spec Spec) []error {
	var errs []error
	refuse := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if spec.Evaluation == "" {
		refuse("an evaluator Job names its PreviewVerification's UID (Evaluation)")
	}
	if h := spec.ToolHost; h == nil {
		refuse("an evaluator Job needs its tool pod's address (ToolHost)")
	} else {
		if e := validation.IsDNS1123Label(h.Name); len(e) > 0 {
			refuse("tool host name %q: %s", h.Name, strings.Join(e, "; "))
		}
		if _, err := netip.ParseAddr(h.IP); err != nil {
			refuse("tool host address %q is not an IP", h.IP)
		}
	}
	if err := validProbeDNS(cfg.SandboxProbeDNS); err != nil {
		refuse("%v", err)
	}
	return errs
}

// validProbeDNS holds the controller's resolver to what the dns probe target
// accepts, <ip>:53, so a misconfiguration is refused here rather than as a
// prepare failure in every evaluator pod.
func validProbeDNS(addr string) error {
	if addr == "" {
		return fmt.Errorf("an evaluator Job's strict probe needs the controller's resolver (Config.SandboxProbeDNS)")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port != "53" || net.ParseIP(host) == nil {
		return fmt.Errorf("the controller's resolver %q is not <ip>:53", addr)
	}
	return nil
}

// roleLabels adds the role, and the verification's UID when there is one,
// to a Job's labels. Like a network profile's label they are on the Job and
// its pod only, never in jobLabels, so the per-Job Secret is unchanged.
func roleLabels(lbls map[string]string, spec Spec) {
	if spec.Role == "" {
		return
	}
	lbls[v1alpha1.LabelRole] = spec.Role
	if spec.Evaluation != "" {
		lbls[v1alpha1.LabelPreviewVerification] = spec.Evaluation
	}
}

// workDir is where a Job's workspace emptyDir is mounted, and its HOME.
func workDir(spec Spec) string {
	if spec.Role != "" {
		return roleWorkDir
	}
	return workspaceDir
}

// toolsURL is the evaluator's MAUL_TOOLS_URL.
func toolsURL(h *HostAlias) string {
	return "http://" + net.JoinHostPort(h.Name, strconv.Itoa(ToolsPort))
}

// roleHandoffScript stages a role Job's handoff where agent-runner reads it
// (its prompt inputs, outside the scratch directory the agent's file tools
// are confined to) and makes that scratch directory. Nothing is fetched.
const roleHandoffScript = `set -eu
mkdir -p ` + roleWorkDir + `/input ` + roleWorkDir + `/scratch
cp ` + inputMount + `/` + secretKeyIssue + ` ` + roleWorkDir + `/input/` + secretKeyIssue + `
if [ -f ` + inputMount + `/` + secretKeyInvestigation + ` ]; then
  cp ` + inputMount + `/` + secretKeyInvestigation + ` ` + roleWorkDir + `/input/` + secretKeyInvestigation + `
fi
`

// evaluatorProbeArgs select the strict probe over every target, the
// cluster's resolver among them: the evaluator holds a broker token and
// reads app output, so open egress of any kind refuses the run (exit 78,
// SandboxUnenforced) rather than narrowing a window as the wait probe does.
var evaluatorProbeArgs = "--mode=" + string(sandboxprobe.ModeStrict) + " --targets=" + strings.Join([]string{
	sandboxprobe.TargetInternet, sandboxprobe.TargetKubernetes, sandboxprobe.TargetMetadata, sandboxprobe.TargetDNS,
}, ",")

// evaluatorProbeScript ends the evaluator's prepare. Under set -e its exit
// status ends the script, so ExitSandboxUnenforced reaches the collectors as
// it does on a repository-image Job.
var evaluatorProbeScript = toolsBinDir + `/` + agentRunnerBin + ` ` + sandboxprobe.Command + ` ` +
	evaluatorProbeArgs + "\n"

// rolePrepareContainer is a role Job's init: the handoff, then the probe
// (strict for the evaluator, the wait probe for the check reviewer). It
// holds no identity, as every prepare init.
func (c *Client) rolePrepareContainer(runner Runner, spec Spec, res corev1.ResourceRequirements) corev1.Container {
	env := []corev1.EnvVar{
		{Name: "HOME", Value: roleWorkDir},
		{Name: sandboxprobe.TimeoutEnv, Value: c.cfg.SandboxProbeTimeout.String()},
	}
	script := roleHandoffScript
	if spec.Role == v1alpha1.RoleEvaluator {
		env = append(env, corev1.EnvVar{Name: sandboxprobe.DNSEnv, Value: c.cfg.SandboxProbeDNS})
		script += evaluatorProbeScript
	} else {
		script += waitProbeScript
	}
	return corev1.Container{
		Name:    initContainerName,
		Image:   runner.Image,
		Command: []string{"/bin/sh", "-c", script},
		Env:     env,
		VolumeMounts: []corev1.VolumeMount{
			{Name: volWorkspace, MountPath: roleWorkDir},
			{Name: volTmp, MountPath: "/tmp"},
			{Name: volInput, MountPath: inputMount, ReadOnly: true},
		},
		SecurityContext: containerSecurity(),
		Resources:       res,
	}
}

// withHostAlias pins h in aliases: beside any name already at its address,
// otherwise as an entry of its own, addresses kept in order so the pod spec
// stays deterministic.
func withHostAlias(aliases []corev1.HostAlias, h *HostAlias) []corev1.HostAlias {
	addr := netip.MustParseAddr(h.IP).Unmap().WithZone("")
	byAddr := map[netip.Addr][]string{addr: {h.Name}}
	for _, a := range aliases {
		ip := netip.MustParseAddr(a.IP)
		for _, n := range a.Hostnames {
			if !slices.Contains(byAddr[ip], n) {
				byAddr[ip] = append(byAddr[ip], n)
			}
		}
	}
	out := make([]corev1.HostAlias, 0, len(byAddr))
	for _, ip := range slices.SortedFunc(maps.Keys(byAddr), netip.Addr.Compare) {
		names := byAddr[ip]
		slices.Sort(names)
		out = append(out, corev1.HostAlias{IP: ip.String(), Hostnames: names})
	}
	return out
}
