// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/devthenet-labs/maul/internal/broker"
	"github.com/devthenet-labs/maul/internal/netprofile"
)

// brokerURLSources are the files that spell the broker's upstream hosts:
// its default base URLs (internal/broker/hosts.go, which serve.go uses),
// the Foundry Entra scope, and serve.go itself, so a literal added there
// beside the constants is still caught.
var brokerURLSources = []string{"serve.go", "../../internal/broker/hosts.go", "../../internal/broker/foundry.go"}

// TestModelHostsDenied pins the egress proxy's deny table to the broker:
// every https URL literal the broker builds a route from, each template
// instantiated with sample regions and resources, is a host no registry,
// file host or redirect target may be, and no tunnel may open to (step 3).
// A new broker route therefore cannot ship without its deny entry
// (netprofile.ModelAPIHosts).
func TestModelHostsDenied(t *testing.T) {
	samples := []string{"us-east-1", "eu-west-2", "us-east5", "europe-west4", "myresource", "acme-ai"}
	var literals []string
	fset := token.NewFileSet()
	for _, src := range brokerURLSources {
		f, err := parser.ParseFile(fset, src, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err == nil && strings.HasPrefix(s, "https://") {
				literals = append(literals, s)
			}
			return true
		})
	}
	if len(literals) < 6 {
		t.Fatalf("found %d https literals (%v); the broker's base URLs moved", len(literals), literals)
	}
	// Every default base URL is a literal the loop below judges; the
	// OpenAI one is named explicitly so the Responses route cannot lose
	// its deny entry by moving.
	for _, def := range []string{broker.DefaultAnthropicBaseURL, broker.DefaultOpenAIBaseURL} {
		if !slices.Contains(literals, def) {
			t.Errorf("%s is not among the broker's https literals %v", def, literals)
		}
	}
	for _, lit := range literals {
		n := strings.Count(lit, "%s")
		for _, sample := range samples {
			args := make([]any, n)
			for i := range args {
				args[i] = sample
			}
			raw := lit
			if n > 0 {
				raw = fmt.Sprintf(lit, args...)
			}
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("%q: %v", raw, err)
			}
			if err := netprofile.DeniedHost(u.Hostname(), netprofile.DenyContext{}); err == nil {
				t.Errorf("the broker reaches %s (from %q), which netprofile.DeniedHost admits", u.Hostname(), lit)
			}
			if err := netprofile.DeniedTunnelHost(u.Hostname(), netprofile.DenyContext{}); err == nil {
				t.Errorf("the broker reaches %s (from %q), which netprofile.DeniedTunnelHost admits",
					u.Hostname(), lit)
			}
		}
	}
}
