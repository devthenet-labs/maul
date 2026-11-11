// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Command shard picks one shard of the e2e suite for hack/e2e.sh when
// MAUL_E2E_SHARD is set (CI runs the suite as two parallel jobs, since one
// job ran close to the go test timeout). It reads `go test -list . ./...` on
// stdin and prints the -run pattern matching exactly this shard's top-level
// tests:
//
//	go test -list . ./... | go run ./shard 1/2
//
// A test's shard is a hash of its name alone, so adding or removing other
// tests never moves it, and for any shard count every test is in exactly one
// shard: the shards together run the whole suite, none twice. Subtests run
// with their parent (the pattern holds no slash). A listing line that is
// neither a test name nor a package summary fails the command, so a broken
// build can never shrink a shard to the names that happened to parse.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// maxShards bounds the shard count to something a CI matrix would use.
const maxShards = 64

var (
	// testName is a top-level test, example or fuzz target as -list prints
	// it; benchmarks never run under -run alone, so they are not listed.
	testName = regexp.MustCompile(`^(Test|Example|Fuzz)[A-Za-z0-9_]*$`)
	// summary is the per-package line -list ends each package with.
	summary = regexp.MustCompile(`^(ok  |\?   )\t\S+\t`)
	// benchmark is skipped, not refused: -list prints them too.
	benchmark = regexp.MustCompile(`^Benchmark[A-Za-z0-9_]*$`)
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go test -list . ./... | shard <index>/<total>")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "shard:", err)
		os.Exit(1)
	}
}

func run(spec string, in io.Reader, out io.Writer) error {
	index, total, err := parseShard(spec)
	if err != nil {
		return err
	}
	names, err := testNames(in)
	if err != nil {
		return err
	}
	mine := assign(names, index, total)
	if len(mine) == 0 {
		return fmt.Errorf("shard %d/%d holds none of the %d tests", index, total, len(names))
	}
	fmt.Fprintf(os.Stderr, "shard %d/%d: %d of %d tests\n", index, total, len(mine), len(names))
	_, err = fmt.Fprintln(out, pattern(mine))
	return err
}

// parseShard reads "<index>/<total>", 1 <= index <= total <= maxShards.
func parseShard(spec string) (index, total int, err error) {
	i, t, ok := strings.Cut(spec, "/")
	if !ok {
		return 0, 0, fmt.Errorf("shard %q: want <index>/<total>", spec)
	}
	index, errI := strconv.Atoi(i)
	total, errT := strconv.Atoi(t)
	if errI != nil || errT != nil || strings.HasPrefix(i, "+") || strings.HasPrefix(t, "+") {
		return 0, 0, fmt.Errorf("shard %q: want <index>/<total>", spec)
	}
	if total < 1 || total > maxShards || index < 1 || index > total {
		return 0, 0, fmt.Errorf("shard %q: want 1 <= index <= total <= %d", spec, maxShards)
	}
	return index, total, nil
}

// testNames returns the sorted, distinct test names of a -list listing.
func testNames(in io.Reader) ([]string, error) {
	var names []string
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "", summary.MatchString(line), benchmark.MatchString(line):
		case testName.MatchString(line):
			names = append(names, line)
		default:
			return nil, fmt.Errorf("unexpected line in the test listing: %q", line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, errors.New("the test listing names no tests")
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// assign returns the names whose shard, of total, is index (1-based).
func assign(names []string, index, total int) []string {
	var mine []string
	for _, name := range names {
		h := fnv.New32a()
		_, _ = h.Write([]byte(name))
		if int(h.Sum32()%uint32(total))+1 == index {
			mine = append(mine, name)
		}
	}
	return mine
}

// pattern is the -run expression matching exactly names at the top level.
// Test names hold no regexp metacharacters (testName), so they need no
// quoting.
func pattern(names []string) string {
	return "^(" + strings.Join(names, "|") + ")$"
}
