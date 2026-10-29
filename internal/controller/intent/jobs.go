// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package intent

import (
	"context"
	"log/slog"
	"maps"
	"strconv"

	"k8s.io/client-go/kubernetes"

	v1alpha1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/jobs"
)

// JobRunner is the jobs surface the run scheduler needs. Create takes the
// run's stage configuration: each intent stage reads its Finding
// counterpart's MAUL_* keys, and only turns and tokens have a per-Job
// channel, so the stage's wall clock and ceilings reach the pod through the
// Env of a jobs Client built for that one launch.
type JobRunner interface {
	Create(ctx context.Context, spec jobs.Spec, env map[string]string) (string, v1alpha1.RunnerImageRef, error)
	Result(ctx context.Context, jobName string) (jobs.RunOutput, error)
	Status(ctx context.Context, jobName string) (jobs.Status, error)
	Delete(ctx context.Context, jobName string) error
}

// jobLauncher is the production JobRunner: one base jobs Config, and for
// each launch a Client built from it plus that launch's stage env (jobs.New
// only wraps the clientset). Observing and deleting Jobs needs no stage env.
type jobLauncher struct {
	cs   kubernetes.Interface
	base jobs.Config
	log  *slog.Logger
	*jobs.Client
}

// NewJobRunner builds the production JobRunner over cs and the base Config.
func NewJobRunner(cs kubernetes.Interface, base jobs.Config, log *slog.Logger) JobRunner {
	return &jobLauncher{cs: cs, base: base, log: log, Client: jobs.New(cs, base, log)}
}

// Create builds a Client with env over the base Config's and creates the Job.
func (l *jobLauncher) Create(ctx context.Context, spec jobs.Spec, env map[string]string) (
	string, v1alpha1.RunnerImageRef, error) {
	cfg := l.base
	cfg.Env = maps.Clone(l.base.Env)
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	maps.Copy(cfg.Env, env)
	return jobs.New(l.cs, cfg, l.log).Create(ctx, spec)
}

// stageEnv is a launch's stage configuration. A plan run reads the
// investigate stage's keys: its wall clock, its idle limit and its ceilings
// (the grant rides the per-Job channel and may only lower them). It also
// states the most its build can be granted, which agent-runner reads from the
// build stage's own ceiling, MAUL_REMEDIATE_MANUAL_*: set to the grant the
// Project's build receives, with the automated budget no higher, as
// agent-runner requires. A build run reads the remediate stage's keys: its
// wall clock, its idle limit, and its manual ceiling (the automated budget,
// which applies only to a Job with no grant, no higher); every intent run is
// granted. An evaluate or checkreview run (a role run) reads the investigate
// stage's keys, as agent-runner runs both role stages on the investigate
// stage's configuration: its wall clock, idle limit and ceilings, and
// nothing of a build's. An unknown stage has none at all (errUnknownStage).
func stageEnv(stage v1alpha1.IntentStage, s Settings, build v1alpha1.IntentRunGrant) (map[string]string, error) {
	n := func(v int64) string { return strconv.FormatInt(v, 10) }
	var ceiling StageCeiling
	switch stage {
	case v1alpha1.IntentStageEvaluate, v1alpha1.IntentStageCheckReview:
		role := s.ceiling(stage)
		return map[string]string{
			"MAUL_INVESTIGATE_TIMEOUT":      role.Timeout.String(),
			"MAUL_INVESTIGATE_IDLE_TIMEOUT": role.IdleTimeout.String(),
			"MAUL_INVESTIGATE_MAX_TURNS":    n(int64(role.MaxTurns)),
			"MAUL_INVESTIGATE_TOKEN_BUDGET": n(role.TokenBudget),
		}, nil
	case v1alpha1.IntentStagePlan:
		return map[string]string{
			"MAUL_INVESTIGATE_TIMEOUT":           s.Plan.Timeout.String(),
			"MAUL_INVESTIGATE_IDLE_TIMEOUT":      s.Plan.IdleTimeout.String(),
			"MAUL_INVESTIGATE_MAX_TURNS":         n(int64(s.Plan.MaxTurns)),
			"MAUL_INVESTIGATE_TOKEN_BUDGET":      n(s.Plan.TokenBudget),
			"MAUL_REMEDIATE_MANUAL_MAX_TURNS":    n(int64(build.MaxTurns)),
			"MAUL_REMEDIATE_MANUAL_TOKEN_BUDGET": n(build.TokenBudget),
			"MAUL_REMEDIATE_AUTO_MAX_TURNS":      n(int64(build.MaxTurns)),
			"MAUL_REMEDIATE_AUTO_TOKEN_BUDGET":   n(build.TokenBudget),
		}, nil
	case v1alpha1.IntentStageBuild:
		ceiling = s.Build
	case v1alpha1.IntentStageRevise:
		ceiling = s.Revise
	default:
		return nil, launchEnabled(stage)
	}
	return map[string]string{
		"MAUL_REMEDIATE_TIMEOUT":             ceiling.Timeout.String(),
		"MAUL_REMEDIATE_IDLE_TIMEOUT":        ceiling.IdleTimeout.String(),
		"MAUL_REMEDIATE_MANUAL_MAX_TURNS":    n(int64(ceiling.MaxTurns)),
		"MAUL_REMEDIATE_MANUAL_TOKEN_BUDGET": n(ceiling.TokenBudget),
		"MAUL_REMEDIATE_AUTO_MAX_TURNS":      n(int64(ceiling.MaxTurns)),
		"MAUL_REMEDIATE_AUTO_TOKEN_BUDGET":   n(ceiling.TokenBudget),
	}, nil
}

// stageTimeoutKey is the MAUL_* key carrying stage's wall clock to
// agent-runner: a plan, evaluate or checkreview run reads the investigate
// stage's, a build or revise run the remediate stage's; an unknown stage
// none at all (errUnknownStage).
func stageTimeoutKey(stage v1alpha1.IntentStage) (string, error) {
	switch stage {
	case v1alpha1.IntentStagePlan, v1alpha1.IntentStageEvaluate, v1alpha1.IntentStageCheckReview:
		return "MAUL_INVESTIGATE_TIMEOUT", nil
	case v1alpha1.IntentStageBuild, v1alpha1.IntentStageRevise:
		return "MAUL_REMEDIATE_TIMEOUT", nil
	}
	return "", launchEnabled(stage)
}
