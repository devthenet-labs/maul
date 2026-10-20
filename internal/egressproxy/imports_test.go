// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoKubernetesInTheCore keeps the engine free of Kubernetes: the pod
// index that attributes source IPs is an adapter (podindex) behind the
// PodLookup port, and the engine never reaches the API server itself.
func TestNoKubernetesInTheCore(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(path, "k8s.io/") || strings.HasPrefix(path, "sigs.k8s.io/") ||
				strings.Contains(path, "/maul/api/") || strings.Contains(path, "/maul/internal/kube") {
				t.Errorf("%s imports %s: the egress proxy core imports no Kubernetes package", name, path)
			}
		}
	}
}
