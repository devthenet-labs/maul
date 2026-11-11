// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestParseShard(t *testing.T) {
	for _, tc := range []struct {
		in           string
		index, total int
		wantErr      bool
	}{
		{in: "1/2", index: 1, total: 2},
		{in: "2/2", index: 2, total: 2},
		{in: "3/5", index: 3, total: 5},
		{in: "1/1", index: 1, total: 1},
		{in: "", wantErr: true},
		{in: "2", wantErr: true},
		{in: "0/2", wantErr: true},
		{in: "3/2", wantErr: true},
		{in: "1/0", wantErr: true},
		{in: "-1/2", wantErr: true},
		{in: "a/2", wantErr: true},
		{in: "1/2/3", wantErr: true},
		{in: " 1/2", wantErr: true},
		{in: "1/65", wantErr: true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			index, total, err := parseShard(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseShard(%q) = %d/%d, want an error", tc.in, index, total)
				}
				return
			}
			if err != nil || index != tc.index || total != tc.total {
				t.Fatalf("parseShard(%q) = %d/%d, %v; want %d/%d", tc.in, index, total, err, tc.index, tc.total)
			}
		})
	}
}

func TestTestNames(t *testing.T) {
	// `go test -list . ./...` output: names, then a package line per
	// package, test-less packages as "?" lines.
	list := strings.Join([]string{
		"TestPipeline",
		"TestIntentPlan",
		"ExampleRender",
		"FuzzParse",
		"ok  \tgithub.com/devthenet-labs/maul/e2e\t0.051s",
		"TestPipeline",
		"BenchmarkIgnored",
		"ok  \tgithub.com/devthenet-labs/maul/e2e/fakegithub\t0.010s",
		"?   \tgithub.com/devthenet-labs/maul/e2e/replay\t[no test files]",
		"",
	}, "\n")
	got, err := testNames(strings.NewReader(list))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ExampleRender", "FuzzParse", "TestIntentPlan", "TestPipeline"}
	if !slices.Equal(got, want) {
		t.Fatalf("testNames = %q, want %q", got, want)
	}
}

func TestTestNamesRefusesUnknownLines(t *testing.T) {
	// A build failure or anything else unexpected must fail the shard,
	// never shrink it to the names that happened to parse.
	for _, line := range []string{
		"FAIL\tgithub.com/devthenet-labs/maul/e2e [build failed]",
		"# github.com/devthenet-labs/maul/e2e",
		"Test Pipeline",
		"testPipeline",
	} {
		if _, err := testNames(strings.NewReader("TestA\n" + line + "\n")); err == nil {
			t.Errorf("testNames accepted %q", line)
		}
	}
	if _, err := testNames(strings.NewReader("ok  \tpkg\t0.1s\n")); err == nil {
		t.Error("testNames accepted a listing with no tests")
	}
}

// TestAssignPartitions is the property the split rests on: for every shard
// count, every name lands in exactly one shard, so the shards together run
// the whole suite and no test runs twice.
func TestAssignPartitions(t *testing.T) {
	names := make([]string, 0, 120)
	for i := range 120 {
		names = append(names, fmt.Sprintf("TestCase%03d", i))
	}
	for total := 1; total <= 8; total++ {
		seen := map[string]int{}
		for index := 1; index <= total; index++ {
			for _, name := range assign(names, index, total) {
				seen[name]++
			}
		}
		for _, name := range names {
			if seen[name] != 1 {
				t.Fatalf("total %d: %s is in %d shards, want exactly 1", total, name, seen[name])
			}
		}
		if len(seen) != len(names) {
			t.Fatalf("total %d: shards hold %d names, want %d", total, len(seen), len(names))
		}
	}
}

// TestAssignIsStable pins that a test's shard depends on its name alone:
// adding or removing other tests never moves it, so a shard's history stays
// comparable run to run.
func TestAssignIsStable(t *testing.T) {
	base := []string{"TestA", "TestB", "TestC", "TestD", "TestE", "TestF"}
	grown := append([]string{"TestAA", "TestZZ"}, base...)
	for index := 1; index <= 2; index++ {
		before := assign(base, index, 2)
		after := assign(grown, index, 2)
		for _, name := range before {
			if !slices.Contains(after, name) {
				t.Fatalf("%s moved off shard %d when other tests were added", name, index)
			}
		}
	}
}

func TestPatternMatchesExactlyItsNames(t *testing.T) {
	names := []string{"TestIntentPlan", "TestPipeline"}
	re := regexp.MustCompile(pattern(names))
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"TestIntentPlan", true},
		{"TestPipeline", true},
		{"TestPipelineCodex", false},
		{"XTestPipeline", false},
		{"TestIntent", false},
	} {
		if got := re.MatchString(tc.name); got != tc.want {
			t.Errorf("pattern %q matches %q = %v, want %v", pattern(names), tc.name, got, tc.want)
		}
	}
	if strings.Contains(pattern(names), "/") {
		t.Errorf("pattern %q holds a slash, which would filter subtests", pattern(names))
	}
}
