// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewtools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	maulv1 "github.com/devthenet-labs/maul/api/v1alpha1"
	"github.com/devthenet-labs/maul/internal/checks"
	"github.com/devthenet-labs/maul/internal/toolwire"
)

// The tool ConfigMap's keys, as preview-controller writes them and the
// tools container reads them under its mount (DefaultConfigDir).
const (
	// ConfigFile holds the Config, JSON.
	ConfigFile = "tools.json"
	// EventsFile holds the slot's pod events and container states,
	// refreshed by preview-controller on each poll (toolwire's key, which
	// preview-controller writes).
	EventsFile = toolwire.CaptureEventsKey
	// LogFilePrefix starts each component's captured log tail:
	// "logs.<component key>" (toolwire's).
	LogFilePrefix = toolwire.CaptureLogPrefix
)

// DefaultConfigDir is where the tool ConfigMap is mounted.
const DefaultConfigDir = "/etc/maul-tools"

// maxConfigBytes bounds the config file: the checks (MaxSavedChecks of at
// most MaxSavedCheckBytes each) with room for the rest.
const maxConfigBytes = maulv1.MaxSavedChecks*(maulv1.MaxSavedCheckBytes+256) + 64<<10

// maxComponents bounds the components: a Preview has at most four; eight
// leaves room without leaving the grammar unbounded.
const maxComponents = 8

// Config is the tools container's one input: what preview-controller
// copied from the PreviewVerification and the Preview into the tool
// ConfigMap. It names every address the relay may reach; nothing an agent
// sends chooses a host.
type Config struct {
	// Components are the previewed components: each one's key, the path
	// prefix it serves on the preview host and its Service address
	// "<ClusterIP>:<port>" (the tool pod resolves no name).
	Components []Component `json:"components"`
	// Host is the preview host ("<label>.<suffix>"), sent as Host and
	// X-Forwarded-Host as the gateway sends them. Empty sends the
	// Service address.
	Host string `json:"host,omitempty"`
	// Viewer is the pairwise X-Maul-Viewer value the gateway would send for
	// the evaluator.
	Viewer string `json:"viewer"`
	// Checks are the saved checks this evaluation replays first (SPEC Q1:
	// inline in the PreviewVerification, copied here).
	Checks []StoredCheck `json:"checks,omitempty"`
	// MaxToolCalls bounds the agent's calls.
	MaxToolCalls int `json:"maxToolCalls"`
	// TimeoutSeconds is the evaluation's wall clock from the container's
	// start: at it the relay finishes on its own.
	TimeoutSeconds int `json:"timeoutSeconds"`
	// ReplayOnly is a replay round (rung replay, SPEC Q17): no model, the
	// relay finishes once the replay is recorded.
	ReplayOnly bool `json:"replayOnly,omitempty"`
	// Browser and Sandbox say which sidecars the pod runs.
	Browser bool `json:"browser,omitempty"`
	Sandbox bool `json:"sandbox,omitempty"`
}

// Component is one previewed component.
type Component struct {
	Key  string `json:"key"`
	Path string `json:"path"`
	Addr string `json:"addr"`
}

// StoredCheck is one saved check: its id, its digest
// ("sha256:<hex>", internal/checks.Digest) and its canonical JSON.
type StoredCheck struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
	Body   string `json:"body"`
}

// ErrConfig is a config the relay refuses.
var ErrConfig = errors.New("previewtools: config refused")

var (
	keyPattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

// LoadConfig reads and parses the config file in dir.
func LoadConfig(dir string) (Config, error) {
	f, err := os.Open(filepath.Join(dir, ConfigFile))
	if err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	return ParseConfig(b)
}

// ParseConfig decodes a config strictly (unknown fields refused) and
// validates it.
func ParseConfig(b []byte) (Config, error) {
	if len(b) > maxConfigBytes {
		return Config{}, fmt.Errorf("%w: over %d bytes", ErrConfig, maxConfigBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if dec.More() {
		return Config{}, fmt.Errorf("%w: trailing data", ErrConfig)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	return c, nil
}

// Validate holds a config to what the relay runs.
func (c Config) Validate() error {
	if err := validateComponents(c.Components); err != nil {
		return err
	}
	switch {
	case c.Host != "" && !hostPattern.MatchString(c.Host):
		return fmt.Errorf("host %q is not a host name", c.Host)
	case c.Viewer == "" || len(c.Viewer) > 128 || !printableASCII(c.Viewer):
		return errors.New("viewer must be 1 to 128 printable characters")
	case c.MaxToolCalls < maulv1.MinToolCalls || c.MaxToolCalls > maulv1.MaxToolCallsLimit:
		return fmt.Errorf("maxToolCalls must be %d to %d", maulv1.MinToolCalls, maulv1.MaxToolCallsLimit)
	case c.TimeoutSeconds < 1 || c.TimeoutSeconds > 3600:
		return errors.New("timeoutSeconds must be 1 to 3600")
	case c.ReplayOnly && len(c.Checks) == 0:
		return errors.New("a replay round needs at least one check")
	}
	return validateChecks(c.Checks)
}

func validateComponents(cs []Component) error {
	if len(cs) == 0 || len(cs) > maxComponents {
		return fmt.Errorf("components must number 1 to %d", maxComponents)
	}
	seen := map[string]bool{}
	for _, comp := range cs {
		if !keyPattern.MatchString(comp.Key) || seen[comp.Key] {
			return fmt.Errorf("component key %q is not a new DNS label", comp.Key)
		}
		seen[comp.Key] = true
		if !strings.HasPrefix(comp.Path, "/") || len(comp.Path) > 256 || !printableASCII(comp.Path) {
			return fmt.Errorf("component %s: path %q is not an absolute path prefix", comp.Key, comp.Path)
		}
		host, port, err := net.SplitHostPort(comp.Addr)
		if err != nil || net.ParseIP(host) == nil || port == "" {
			return fmt.Errorf("component %s: address %q is not <ip>:<port>", comp.Key, comp.Addr)
		}
	}
	return nil
}

func validateChecks(cs []StoredCheck) error {
	if len(cs) > maulv1.MaxSavedChecks {
		return fmt.Errorf("more than %d checks", maulv1.MaxSavedChecks)
	}
	seen := map[string]bool{}
	for _, s := range cs {
		if len(s.Body) > maulv1.MaxSavedCheckBytes {
			return fmt.Errorf("check %q is over %d bytes", s.ID, maulv1.MaxSavedCheckBytes)
		}
		c, err := checks.Parse([]byte(s.Body))
		if err != nil {
			return fmt.Errorf("check %q: %v", s.ID, err)
		}
		if c.ID != s.ID || checks.Digest(c) != s.Digest {
			return fmt.Errorf("check %q: its id or digest is not its body's", s.ID)
		}
		if seen[s.Digest] {
			return fmt.Errorf("check %q is named twice", s.ID)
		}
		seen[s.Digest] = true
	}
	return nil
}

// Timeout is the evaluation's wall clock.
func (c Config) Timeout() time.Duration { return time.Duration(c.TimeoutSeconds) * time.Second }

// paths are the components' path prefixes, in order.
func (c Config) paths() []string {
	out := make([]string, len(c.Components))
	for i, comp := range c.Components {
		out[i] = comp.Path
	}
	return out
}

// keys are the components' keys, in order.
func (c Config) keys() []string {
	out := make([]string, len(c.Components))
	for i, comp := range c.Components {
		out[i] = comp.Key
	}
	return out
}

// component returns the component with key.
func (c Config) component(key string) (Component, bool) {
	for _, comp := range c.Components {
		if comp.Key == key {
			return comp, true
		}
	}
	return Component{}, false
}

// targets maps each component key to its address.
func (c Config) targets() map[string]string {
	out := make(map[string]string, len(c.Components))
	for _, comp := range c.Components {
		out[comp.Key] = comp.Addr
	}
	return out
}

func printableASCII(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) || r == ' ' {
			return false
		}
	}
	return true
}
