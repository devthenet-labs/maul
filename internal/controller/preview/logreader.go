// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// LogReader is the port the verification reconciler reads container logs
// through: the tester's MAUL-VERIFY output and the components' log tails,
// in a slot. The kube adapter (KubeLogReader) is the pods/log endpoint; the
// directory adapter (DirLogReader) stands in where there is none (envtest,
// e2e). A log that does not exist (its Pod or container gone) is a NotFound
// error.
type LogReader interface {
	// ReadLog returns at most opts.LimitBytes of container's log in pod.
	ReadLog(ctx context.Context, namespace, pod, container string, opts LogOptions) ([]byte, error)
}

// LogOptions bound one log read. LimitBytes is required: nothing is ever
// read unbounded.
type LogOptions struct {
	// LimitBytes is the most read.
	LimitBytes int64
	// TailLines, when set, reads only the log's last lines.
	TailLines *int64
	// SinceTime, when set, reads only what was logged from then on.
	SinceTime *time.Time
}

// errNoLimit refuses an unbounded read.
var errNoLimit = errors.New("log read without a byte limit")

// readBounded reads at most limit bytes of r.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}

// DirLogReader reads logs from files, <Dir>/<namespace>/<pod>/<container>.log,
// where a test harness's fake kubelet writes what a container printed.
// Every name must be a DNS label, so a read never leaves Dir.
type DirLogReader struct {
	Dir string
}

// ReadLog reads the file's first opts.LimitBytes; TailLines and SinceTime
// are not applied (a test's logs are short and its own).
func (d DirLogReader) ReadLog(_ context.Context, namespace, pod, container string, opts LogOptions) ([]byte, error) {
	if opts.LimitBytes <= 0 {
		return nil, errNoLimit
	}
	for _, name := range []string{namespace, pod, container} {
		if len(name) > maxObjectName*4 || !namePattern.MatchString(name) {
			return nil, fmt.Errorf("log of %q/%q/%q: not a namespace, pod and container name", namespace, pod,
				container)
		}
	}
	f, err := os.Open(filepath.Join(d.Dir, namespace, pod, container+".log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, kerrors.NewNotFound(schema.GroupResource{Resource: "pods/log"}, pod)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return readBounded(f, opts.LimitBytes)
}
