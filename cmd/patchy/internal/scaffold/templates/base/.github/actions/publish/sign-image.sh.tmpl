#!/usr/bin/env bash
# Sign the published agent image with the AWS KMS key AGENT_SIGNING_KEY
# names, so patchy's runner-image verifier (repositoryImages.cosignPublicKey)
# accepts it. It signs exactly IMAGE_DIGEST, the digest validate_oci.py
# printed and copy-image.sh published, never a tag.
#
# The signature is cosign's legacy form, a tag sha256-<hex>.sig beside the
# image: a tag survives an ECR lifecycle rule that expires untagged images,
# where an OCI referrer would not. Nothing is uploaded to the public
# transparency log, so private repository names stay private. The tags are
# immutable, so an image that already verifies is never signed again: a
# re-run, or the first run after AGENT_SIGNING_KEY is set, signs only what
# is not yet signed.
set -euo pipefail

fail() {
  echo "sign-image: $1" >&2
  exit 1
}

[[ "${IMAGE_KIND:-}" == agent ]] || fail "only the agent image is signed"
[[ "${IMAGE_DIGEST:-}" =~ ^sha256:[a-f0-9]{64}$ ]] || fail "IMAGE_DIGEST must be a sha256 digest"
[[ -n "${AGENT_SIGNING_KEY:-}" ]] || fail "AGENT_SIGNING_KEY is unset, so signing is off"
bash "$(dirname -- "${BASH_SOURCE[0]}")/check-config.sh" > /dev/null
image="$ECR_REGISTRY/$IMAGE_REPOSITORY@$IMAGE_DIGEST"

# cosign reads registry credentials from DOCKER_CONFIG: a private one,
# removed on exit.
umask 077
export DOCKER_CONFIG="$RUNNER_TEMP/cosign-docker"
trap 'rm -rf -- "$DOCKER_CONFIG"' EXIT
mkdir -p "$DOCKER_CONFIG"
aws ecr get-login-password | cosign login --username AWS --password-stdin "$ECR_REGISTRY"

verify() {
  cosign verify --key "$AGENT_SIGNING_KEY" --insecure-ignore-tlog=true "$image" > /dev/null 2> "$RUNNER_TEMP/cosign-verify-error"
}

if verify; then
  printf 'Already signed: %s\n' "$image"
  exit 0
fi
cosign sign --key "$AGENT_SIGNING_KEY" --tlog-upload=false --new-bundle-format=false --use-signing-config=false \
  --yes "$image"
verify || { cat "$RUNNER_TEMP/cosign-verify-error" >&2; fail "$image does not verify after signing"; }
printf 'Signed %s\n' "$image"
