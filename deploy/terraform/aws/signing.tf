# Copyright 2026 Bitwise Media Group Ltd.
# SPDX-License-Identifier: MIT

# Optional cosign signing of every app's agent image. Either the module
# creates the key (create_agent_signing_key) or the operator names one
# (agent_signing_key_arn); each app's agent publisher, and no other role,
# may sign with it. The private half never leaves KMS. Put the public half
# (output agent_signing.public_key_pem) in the chart's
# agent.repositoryImages.cosignPublicKey yourself, then set allowUnsigned
# false: like every security switch, it stays an explicit line in the
# operator's values, never part of helm_values.

resource "aws_kms_key" "agent_signing" {
  count = var.create_agent_signing_key ? 1 : 0

  description              = "cosign signing key for patchy agent images (${local.name_prefix})"
  customer_master_key_spec = "ECC_NIST_P256"
  key_usage                = "SIGN_VERIFY"
  deletion_window_in_days  = 30
  tags                     = var.tags
}

resource "aws_kms_alias" "agent_signing" {
  count = var.create_agent_signing_key ? 1 : 0

  name          = "alias/${local.name_prefix}-patchy-agent-image-signing"
  target_key_id = aws_kms_key.agent_signing[0].key_id
}

locals {
  # Known at plan time, unlike a created key's ARN, so counts can use it.
  agent_signing         = var.create_agent_signing_key || var.agent_signing_key_arn != null
  agent_signing_key_arn = var.create_agent_signing_key ? aws_kms_key.agent_signing[0].arn : var.agent_signing_key_arn
}

data "aws_kms_public_key" "agent_signing" {
  count  = local.agent_signing ? 1 : 0
  key_id = local.agent_signing_key_arn
}
