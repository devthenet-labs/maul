---
name: Bug report
about: Something in maul does not work as documented
title: ""
labels: bug
---

<!--
Security problems go through a private advisory, never an issue:
https://github.com/devthenet-labs/maul/security/advisories/new

Before posting logs, a resource's YAML or an agent transcript, remove secrets, tokens and anything private.
-->

## What happened

<!-- What you did, what you expected, and what happened instead. -->

## How to reproduce

1.
2.
3.

## Versions

- maul release (chart and controllers):
- `maul --version`:
- Kubernetes version and platform (EKS, GKE, AKS, k3d, ...):
- Installed with: Helm / kustomize

## Which part

<!--
For example: an intent (plan, build, revision, preview, evaluator), a finding, the status page, the CLI, the chart.
-->

## Evidence

<!--
What helps most, as text:
- `kubectl get intent,intentrun,preview -n <namespace>` or `kubectl get findings -n <namespace>`
- `kubectl describe` of the resource, its conditions included
- the controller's log lines around the problem
- the output of `maul check project <project>` or `maul check previews`, if it is about a Project or previews
-->
