// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package podstate reads a tool pod's state the same way for every host of
// the agent evaluator's tool pod (preview-controller's slot, the agents
// host): a container's termination message (TerminationMessage) and the
// image it ran, held to the status field's bound (ImageID), and why a pod
// will not get going: a container crash-looping (CrashLooping, past
// RestartBound), one waiting on its image (ImagePullWaiting,
// ImagePullReason) or a pod the scheduler could not place (Unschedulable).
// Each host keeps its own judgement of what a state means and its own
// writes.
//
// It reads values only: the standard library and the core API types (a
// test pins that).
package podstate
