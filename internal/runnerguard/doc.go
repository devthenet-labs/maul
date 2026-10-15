// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package runnerguard is the controller side of repository-declared runner
// images, shared by the two job controllers (investigation, remediation) so
// their launch and collect paths cannot drift apart.
//
// It decides three things. At launch, whether a Job may run the image the
// Repository pinned (Guard.Pin): only with the controller's kill switch on,
// the sandbox breaker untripped and the finding not revived by a human, and
// only an image source-controller actually pinned — a rejected declaration
// carries no image, so its finding runs the default one. Guard.PinFor makes
// the same decision for a launch that requires the image (an intent's build
// or revise run), where no image to copy is a refusal, not a fallback, and
// where no revival applies: an intent brought back by its trigger label is a
// new plan and a new approval. When Pin declines an image the Repository
// did pin, a remediation launch holds the finding for a human
// (HeldCondition, Held) instead of remediating untested on the default
// image, until a human approves exactly that. On a launch that runs the
// accepted image, Guard.Profile requests the operator network profile the
// repository matches, if exactly one does, and only for a remediate or
// intent build Job, under its own kill switch. While a repository-image Job has not
// finished, whether its pod is still within the pull grace, stuck on a
// deterministic pull failure, or worth another look (Pending): a pod in
// ImagePullBackOff never mutates its Job, so the Job watch alone would never
// report it. And whether the trusted prepare init refused
// to hand over because NetworkPolicy is not enforced (SandboxRefused), which
// trips the Breaker for the rest of the process's life.
//
// Every collect-side decision reads the Job's own runner-image-source
// annotation (jobs.Status.RunnerImageSource), never controller configuration,
// so a default-image Job behaves exactly as it did before the feature
// existed, and a kill-switch flip never changes how an already-launched Job
// is judged.
package runnerguard
