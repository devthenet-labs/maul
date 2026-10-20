// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package projectcheck

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"strconv"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/devthenet-labs/maul/cmd/maul/internal/previewcheck"
	"github.com/devthenet-labs/maul/internal/runnerimage"
	"github.com/devthenet-labs/maul/internal/runnerimage/resolve"
)

// The configuration keys the check reads; each is a MAUL_* environment
// variable a controller binds to its flag of the same name.
const (
	keyRepositoryImages      = "MAUL_REPOSITORY_IMAGES"
	keyImageRegistries       = "MAUL_REPOSITORY_IMAGE_REGISTRIES"
	keyImageDenied           = "MAUL_REPOSITORY_IMAGE_DENIED_REGISTRIES"
	keyImageMaxBytes         = "MAUL_REPOSITORY_IMAGE_MAX_BYTES"
	keyImageAllowUnsigned    = "MAUL_REPOSITORY_IMAGE_ALLOW_UNSIGNED"
	keyImageCosignKeyFile    = "MAUL_REPOSITORY_IMAGE_COSIGN_KEY_FILE"
	keyIntentPreviewsEnabled = "MAUL_INTENT_PREVIEWS_ENABLED"
	keyPreviewImagePrefix    = "MAUL_PREVIEW_IMAGE_PREFIX"
	keyPreviewHostSuffix     = "MAUL_PREVIEW_HOST_SUFFIX"
	// cosignKeyData is the key of the chart's repository-image-key
	// ConfigMap, which source-controller mounts as its cosign key file.
	cosignKeyData = "cosign.pub"
)

// settings are the controllers' configuration (previewcheck.LoadSettings,
// never a Secret) and source-controller's cosign key.
type settings struct {
	previewcheck.Settings
	// cosignKey is the PEM of source-controller's cosign key ConfigMap,
	// empty when there is none.
	cosignKey string
}

// loadSettings reads the namespace's maul ConfigMaps once; the cosign key
// is the first by name of source-controller's that holds one.
func loadSettings(ctx context.Context, r client.Reader, namespace string) settings {
	s := settings{Settings: previewcheck.LoadSettings(ctx, r, namespace)}
	for i := range s.ConfigMaps {
		cm := &s.ConfigMaps[i]
		if cm.Labels[previewcheck.LabelName] == previewcheck.SourceController && cm.Data[cosignKeyData] != "" {
			s.cosignKey = cm.Data[cosignKeyData]
			break
		}
	}
	return s
}

// imagePolicy is source-controller's policy for repository-declared images,
// as its ConfigMaps configure it.
type imagePolicy struct {
	from     string
	enabled  bool
	policy   *runnerimage.Policy
	maxBytes int64
	// key is the operator's cosign key, set when source-controller
	// requires a signature and its key ConfigMap was readable.
	key *ecdsa.PublicKey
	// signature says how the signature is judged, for the reason.
	signature string
	// unverified is why a signature source-controller requires cannot be
	// verified here (its key was not found); empty when it can be, or when
	// none is required. An image that passes every other check is then a
	// SKIP, never a PASS.
	unverified string
}

// policy parses source-controller's settings. An error is a configuration
// source-controller itself would refuse to start with.
func (s settings) policy() (imagePolicy, error) {
	c := s.Source
	p := imagePolicy{from: c.From, enabled: c.Bool(keyRepositoryImages), maxBytes: resolve.DefaultMaxBytes}
	if !p.enabled {
		return p, nil
	}
	policy, err := runnerimage.NewPolicy(c.List(keyImageRegistries))
	if err != nil {
		return p, fmt.Errorf("%s: %s: %w", c.From, keyImageRegistries, err)
	}
	// The registry paths source-controller refuses declared images from
	// whatever the allowlist says: the chart's preview image prefix.
	if policy, err = policy.Deny(c.List(keyImageDenied)); err != nil {
		return p, fmt.Errorf("%s: %s: %w", c.From, keyImageDenied, err)
	}
	p.policy = &policy
	if v := strings.TrimSpace(c.Data[keyImageMaxBytes]); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return p, fmt.Errorf("%s: %s is %q, not a positive byte count", c.From, keyImageMaxBytes, v)
		}
		p.maxBytes = n
	}
	switch {
	case c.Bool(keyImageAllowUnsigned):
		p.signature = "unsigned images allowed"
	case strings.TrimSpace(c.Data[keyImageCosignKeyFile]) == "":
		return p, fmt.Errorf("%s: neither %s nor %s is set", c.From, keyImageAllowUnsigned, keyImageCosignKeyFile)
	case s.cosignKey == "":
		p.unverified = fmt.Sprintf("source-controller requires a signature (%s is set), but no ConfigMap "+
			"labelled %s=%s holds its key as %s, so it cannot be verified here", keyImageCosignKeyFile, previewcheck.LabelName,
			previewcheck.SourceController, cosignKeyData)
	default:
		key, err := resolve.ParsePublicKey([]byte(s.cosignKey))
		if err != nil {
			return p, fmt.Errorf("source-controller's cosign key ConfigMap: %w", err)
		}
		p.key = key
		p.signature = "signature verified with source-controller's cosign key"
	}
	return p, nil
}
