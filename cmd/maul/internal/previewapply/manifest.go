// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewapply

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	"github.com/devthenet-labs/maul/cmd/maul/internal/previewsetup"
)

// Objects decodes a manifest file, one object per YAML document, empty
// documents skipped. Each must name its apiVersion, kind and name.
func Objects(data []byte) ([]*unstructured.Unstructured, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []*unstructured.Unstructured
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
		if len(m) == 0 {
			continue
		}
		o := &unstructured.Unstructured{Object: m}
		if o.GetAPIVersion() == "" || o.GetKind() == "" || o.GetName() == "" {
			return nil, fmt.Errorf("document %d names no apiVersion, kind or name", len(out)+1)
		}
		out = append(out, o)
	}
	if len(out) == 0 {
		return nil, errors.New("no object in it")
	}
	return out, nil
}

// describe names an object, Kind namespace/name.
func describe(o *unstructured.Unstructured) string {
	if ns := o.GetNamespace(); ns != "" {
		return o.GetKind() + " " + ns + "/" + o.GetName()
	}
	return o.GetKind() + " " + o.GetName()
}

// deleteCommand is the kubectl command that deletes o over conn.
func deleteCommand(conn previewsetup.Connection, o *unstructured.Unstructured) string {
	gvk := o.GroupVersionKind()
	ref := strings.ToLower(gvk.Kind)
	if gvk.Group != "" {
		ref += "." + gvk.Group
	}
	ref += "/" + o.GetName()
	if ns := o.GetNamespace(); ns != "" {
		return conn.Kubectl() + " -n " + ns + " delete " + ref
	}
	return conn.Kubectl() + " delete " + ref
}

// applyError is err for o. An API server's answer about a secret file's
// object is named by its status reason and code only: its message could
// quote what was sent. A transport error carries no object and is shown.
func applyError(o *unstructured.Unstructured, secret bool, err error) error {
	var status apierrors.APIStatus
	if !secret || !errors.As(err, &status) {
		return fmt.Errorf("apply %s: %w", describe(o), err)
	}
	s := status.Status()
	return fmt.Errorf("apply %s: %s (HTTP %d; the API server's message is not shown: the object holds a secret)",
		describe(o), s.Reason, s.Code)
}
