#!/bin/sh
# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT
#
# Lint both helm charts, then render every component combination the templates
# branch on — a broken combination fails here instead of at install time.
set -eu

helm lint charts/maul
helm template maul charts/maul >/dev/null
helm template maul charts/maul --set agent.networkPolicy.cilium.enabled=true >/dev/null
helm template maul charts/maul --set agent.networkPolicy.istio.enabled=true >/dev/null
helm template maul charts/maul \
  --set agent.networkPolicy.create=false --set agent.createNamespace=false \
  --set sourceController.networkPolicy.create=false \
  --set contextController.networkPolicy.create=false \
  --set remediationController.networkPolicy.create=false >/dev/null
helm template maul charts/maul \
  --set webhook.host=maul.example.com \
  --set webhook.ingress.enabled=true --set webhook.ingress.className=nginx \
  --set-json 'webhook.httpRoute={"enabled":true,"parentRefs":[{"name":"gw","namespace":"gateway-system"}]}' >/dev/null
helm template maul charts/maul --set statusServer.enabled=false >/dev/null

# Empty preview slots are off by default; when enabled all of their guardrails
# and the non-default EKS IngressClass must render together.
helm lint charts/maul -f hack/testdata/chart-render/preview-foundation.yaml
helm template maul charts/maul -f hack/testdata/chart-render/preview-foundation.yaml >/dev/null
helm lint charts/maul -f hack/testdata/chart-render/preview-foundation.yaml \
  -f hack/testdata/chart-render/intent-controller.yaml \
  -f hack/testdata/chart-render/preview-controller.yaml
helm template maul charts/maul -f hack/testdata/chart-render/preview-foundation.yaml \
  -f hack/testdata/chart-render/intent-controller.yaml \
  -f hack/testdata/chart-render/preview-controller.yaml >/dev/null

# Preview sign-in (previewAuth, off by default) in both stages: permit over
# the slots alone, require (confirmed, as a render without a cluster must)
# over the whole preview runtime and the edge class.
helm lint charts/maul -f hack/testdata/chart-render/preview-foundation.yaml \
  -f hack/testdata/chart-render/preview-auth.yaml
helm lint charts/maul -f hack/testdata/chart-render/preview-foundation.yaml \
  -f hack/testdata/chart-render/intent-controller.yaml \
  -f hack/testdata/chart-render/preview-controller.yaml \
  -f hack/testdata/chart-render/preview-auth.yaml \
  --set previewAuth.stage=require --set previewAuth.permitConfirmedGeneration=1
helm template maul charts/maul -f hack/testdata/chart-render/preview-foundation.yaml \
  -f hack/testdata/chart-render/auto-mode.yaml \
  -f hack/testdata/chart-render/intent-controller.yaml \
  -f hack/testdata/chart-render/preview-controller.yaml \
  -f hack/testdata/chart-render/preview-auth.yaml \
  --set previewAuth.stage=require --set previewAuth.permitConfirmedGeneration=1 >/dev/null

# The EKS Auto Mode toggles (each default off): node-local DNS egress, the
# preview NodeClass/NodePool and the edge IngressClass, over the whole stack.
helm lint charts/maul -f hack/testdata/chart-render/preview-foundation.yaml \
  -f hack/testdata/chart-render/auto-mode.yaml
helm template maul charts/maul -f hack/testdata/chart-render/preview-foundation.yaml \
  -f hack/testdata/chart-render/auto-mode.yaml \
  -f hack/testdata/chart-render/intent-controller.yaml \
  -f hack/testdata/chart-render/preview-controller.yaml \
  -f hack/testdata/chart-render/evaluation-controller.yaml >/dev/null

# The egress broker: rendered by default (claude enabled ⇒ broker), for
# codex alone (brokered on the openai route), absent when only non-brokered
# runners are enabled, and one render per provider.
helm template maul charts/maul \
  --set agent.runners.claude.enabled=false \
  --set agent.runners.codex.enabled=true --set egressBroker.openai.apiKey.secret=maul-openai >/dev/null
helm template maul charts/maul \
  --set agent.runners.claude.enabled=false \
  --set agent.runners.copilot.enabled=true >/dev/null
helm template maul charts/maul \
  --set egressBroker.anthropicAuth=token >/dev/null
helm template maul charts/maul \
  --set agent.runners.claude.provider.name=bedrock \
  --set agent.runners.claude.provider.region=us-east-1 \
  --set-json 'egressBroker.serviceAccount.annotations={"eks.amazonaws.com/role-arn":"arn:aws:iam::123456789012:role/maul-broker"}' >/dev/null
helm template maul charts/maul \
  --set agent.runners.claude.provider.name=vertex \
  --set agent.runners.claude.provider.region=europe-west1 \
  --set agent.runners.claude.provider.projectID=example-project \
  --set-json 'egressBroker.serviceAccount.annotations={"iam.gke.io/gcp-service-account":"maul-broker@example-project.iam.gserviceaccount.com"}' >/dev/null
helm template maul charts/maul \
  --set agent.runners.claude.provider.name=foundry \
  --set agent.runners.claude.provider.resource=example-foundry \
  --set-json 'agent.runners.claude.provider.modelMap={"anthropic/claude-sonnet-5":"sonnet-deploy","anthropic/claude-opus-5":"opus-deploy"}' \
  --set egressBroker.foundrySecret=maul-foundry >/dev/null
helm template maul charts/maul \
  --set agent.networkPolicy.cilium.enabled=true \
  --set agent.runners.codex.enabled=true --set egressBroker.openai.apiKey.secret=maul-openai >/dev/null
helm template maul charts/maul \
  --set agent.networkPolicy.dns=none --set agent.networkPolicy.broadEgress=never \
  --set agent.runners.codex.enabled=true --set egressBroker.openai.apiKey.secret=maul-openai >/dev/null
helm template maul charts/maul \
  --set statusServer.host=maul-status.example.com \
  --set statusServer.ingress.enabled=true --set statusServer.ingress.className=nginx \
  --set-json 'statusServer.httpRoute={"enabled":true,"annotations":{},"parentRefs":[{"name":"gw","namespace":"gateway-system"}]}' \
  --set statusServer.rbac.userRoles=true \
  --set-json 'statusServer.auth.config={"mode":"oidc","oidc":{"issuerURL":"https://sso.example.com","clientID":"maul-status","clientSecret":"placeholder"}}' >/dev/null

# The optional evaluation controller (default off): its own template plus
# source-controller's :9791 fragments, under every egress mode, with both
# exposure flavours, and as the only fleet that runs claude.
helm lint charts/maul -f hack/testdata/chart-render/evaluation-controller.yaml
helm template maul charts/maul -f hack/testdata/chart-render/evaluation-controller.yaml >/dev/null
helm template maul charts/maul -f hack/testdata/chart-render/evaluation-controller.yaml \
  --set agent.networkPolicy.mode=cilium >/dev/null
helm template maul charts/maul -f hack/testdata/chart-render/evaluation-controller.yaml \
  --set agent.networkPolicy.mode=gke >/dev/null
helm template maul charts/maul -f hack/testdata/chart-render/evaluation-controller.yaml \
  --set agent.networkPolicy.mode=istio >/dev/null
helm template maul charts/maul -f hack/testdata/chart-render/evaluation-controller.yaml \
  --set evaluationController.host=maul-evals.example.com \
  --set evaluationController.ingress.enabled=true --set evaluationController.ingress.className=nginx \
  --set-json 'evaluationController.httpRoute={"enabled":true,"annotations":{},"parentRefs":[{"name":"gw","namespace":"gateway-system"}]}' \
  --set evaluationController.rbac.userRoles=true >/dev/null
helm template maul charts/maul -f hack/testdata/chart-render/evaluation-controller.yaml \
  --set agent.runners.claude.enabled=false --set agent.runners.copilot.enabled=true \
  --set evaluationController.runners.copilot.enabled=true >/dev/null

# The optional intent controller (default off): its own template under every
# egress mode, with repository images on, beside the evaluation controller,
# and as the only fleet that runs claude (which still deploys the broker).
helm lint charts/maul -f hack/testdata/chart-render/intent-controller.yaml
helm template maul charts/maul -f hack/testdata/chart-render/intent-controller.yaml >/dev/null
helm template maul charts/maul -f hack/testdata/chart-render/intent-controller.yaml \
  --set agent.networkPolicy.mode=cilium >/dev/null
helm template maul charts/maul -f hack/testdata/chart-render/intent-controller.yaml \
  --set agent.networkPolicy.mode=gke >/dev/null
helm template maul charts/maul -f hack/testdata/chart-render/intent-controller.yaml \
  --set agent.networkPolicy.mode=istio >/dev/null
helm lint charts/maul -f hack/testdata/chart-render/intent-controller.yaml \
  -f hack/testdata/chart-render/repository-images.yaml
helm template maul charts/maul -f hack/testdata/chart-render/intent-controller.yaml \
  -f hack/testdata/chart-render/repository-images.yaml >/dev/null
helm template maul charts/maul -f hack/testdata/chart-render/intent-controller.yaml \
  -f hack/testdata/chart-render/evaluation-controller.yaml >/dev/null
helm template maul charts/maul -f hack/testdata/chart-render/intent-controller.yaml \
  --set agent.runners.claude.enabled=false --set agent.runners.codex.enabled=true \
  --set egressBroker.openai.apiKey.secret=maul-openai >/dev/null

# The egress proxy on (two profiles, repository images, dns none), and with
# attribution.
helm lint charts/maul -f hack/testdata/chart-render/intent-controller.yaml \
  -f hack/testdata/chart-render/repository-images.yaml \
  -f hack/testdata/chart-render/egress-proxy.yaml --set agent.networkPolicy.dns=none
helm lint charts/maul -f hack/testdata/chart-render/repository-images.yaml \
  -f hack/testdata/chart-render/egress-proxy.yaml --set egressProxy.attribution.enabled=true \
  --set-json 'egressProxy.attribution.apiServerCIDRs=["10.0.1.10/32"]'
# ...with registries (bearer, basic, internal with a CA, CodeArtifact), under
# mode none and cilium.
helm lint charts/maul -f hack/testdata/chart-render/intent-controller.yaml \
  -f hack/testdata/chart-render/repository-images.yaml \
  -f hack/testdata/chart-render/egress-proxy.yaml -f hack/testdata/chart-render/egress-registries.yaml
helm lint charts/maul -f hack/testdata/chart-render/repository-images.yaml \
  -f hack/testdata/chart-render/egress-proxy.yaml -f hack/testdata/chart-render/egress-registries.yaml \
  --set agent.networkPolicy.mode=cilium

# ...with tunnel hosts (a go profile with hosts, a tunnel-only profile).
helm lint charts/maul -f hack/testdata/chart-render/intent-controller.yaml \
  -f hack/testdata/chart-render/repository-images.yaml \
  -f hack/testdata/chart-render/egress-tunnel.yaml --set agent.networkPolicy.dns=none

# The CR chart: the empty default plus a populated render of every array.
helm lint charts/maul-config
helm template maul-config charts/maul-config >/dev/null
helm template maul-config charts/maul-config \
  --set-json 'integrations=[{"name":"github","spec":{"provider":"github","secretRef":{"name":"maul-github"},"interval":"10m"}}]' \
  --set-json 'forges=[{"name":"github","spec":{"provider":"github","secretRef":{"name":"maul-github"},"interval":"10m"}}]' \
  -f hack/testdata/chart-render/config-projects.yaml >/dev/null
