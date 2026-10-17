// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewcheck

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The labels a controller's ConfigMap carries: the chart's
// app.kubernetes.io/name is the controller, and both the chart and kustomize
// set part-of. Kustomize's one shared ConfigMap (patchy-config) is named for
// the whole stack instead, and a controller reads it first and its own after.
const (
	LabelName   = "app.kubernetes.io/name"
	labelPartOf = "app.kubernetes.io/part-of"
	partOf      = "patchy"
	sharedName  = "patchy"
)

// The controllers whose settings the checks read.
const (
	SourceController  = "source-controller"
	IntentController  = "intent-controller"
	PreviewController = "preview-controller"
	// PreviewAuth is the preview sign-in relay, chart-only: it never reads
	// kustomize's shared ConfigMap.
	PreviewAuth = "preview-auth"
)

// ControllerConfig is one controller's settings as its ConfigMaps hold them.
type ControllerConfig struct {
	// From names the ConfigMaps read, for reasons; empty when none exists.
	From string
	// Name and Annotations are the controller's own ConfigMap's, empty
	// when it has none (the shared one's are never read).
	Name        string
	Annotations map[string]string
	Data        map[string]string
	// Err is why the ConfigMaps could not be listed.
	Err error
}

// Found reports whether any ConfigMap configures the controller.
func (c ControllerConfig) Found() bool { return c.Err == nil && c.From != "" }

// Bool reads a boolean setting; unset or unparsable is false, as a flag's
// default would be.
func (c ControllerConfig) Bool(key string) bool {
	v, err := strconv.ParseBool(strings.TrimSpace(c.Data[key]))
	return err == nil && v
}

// List reads a comma-separated setting the way a controller's StringList
// does: entries trimmed, empty ones dropped.
func (c ControllerConfig) List(key string) []string {
	var out []string
	for e := range strings.SplitSeq(c.Data[key], ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// Settings are the controllers' configuration, read once per run. Never a
// Secret: only ConfigMaps.
type Settings struct {
	Source, Intent, Preview ControllerConfig
	// Auth is the preview sign-in relay's own settings, found only when the
	// chart's previewAuth is on.
	Auth ControllerConfig
	// ConfigMaps are the patchy ConfigMaps listed, sorted by name, for a
	// caller that reads a controller's other data (check project's cosign
	// key); nil when the list failed.
	ConfigMaps []corev1.ConfigMap
}

// LoadSettings lists the namespace's patchy ConfigMaps once and assigns
// each controller its own over the shared one. With several of one
// controller's (two releases in a namespace), the first by name is read.
func LoadSettings(ctx context.Context, r client.Reader, namespace string) Settings {
	var list corev1.ConfigMapList
	err := r.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabels{labelPartOf: partOf})
	if err != nil {
		err = fmt.Errorf("list the patchy ConfigMaps in namespace %s: %w", namespace, err)
		failed := ControllerConfig{Err: err}
		return Settings{Source: failed, Intent: failed, Preview: failed, Auth: failed}
	}
	items := list.Items
	slices.SortFunc(items, func(a, b corev1.ConfigMap) int { return strings.Compare(a.Name, b.Name) })
	shared := pick(items, sharedName)
	return Settings{
		Source:     overlay(shared, pick(items, SourceController)),
		Intent:     overlay(shared, pick(items, IntentController)),
		Preview:    overlay(shared, pick(items, PreviewController)),
		Auth:       overlay(nil, pick(items, PreviewAuth)),
		ConfigMaps: items,
	}
}

// pick returns the first ConfigMap labelled for name that holds settings.
func pick(items []corev1.ConfigMap, name string) *corev1.ConfigMap {
	for i := range items {
		if items[i].Labels[LabelName] != name {
			continue
		}
		for k := range items[i].Data {
			if strings.HasPrefix(k, "PATCHY_") {
				return &items[i]
			}
		}
	}
	return nil
}

// overlay is a controller's settings: the shared ConfigMap's data, then its
// own, which wins, as the Deployment's envFrom order has it.
func overlay(shared, own *corev1.ConfigMap) ControllerConfig {
	var c ControllerConfig
	var from []string
	c.Data = map[string]string{}
	for _, cm := range []*corev1.ConfigMap{shared, own} {
		if cm == nil {
			continue
		}
		maps.Copy(c.Data, cm.Data)
		from = append(from, cm.Namespace+"/"+cm.Name)
	}
	c.From = strings.Join(from, " and ")
	if own != nil {
		c.Name, c.Annotations = own.Name, own.Annotations
	}
	return c
}
