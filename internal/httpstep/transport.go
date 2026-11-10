// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package httpstep

import (
	"maps"
	"slices"
)

// The observation's bounds. The PreviewVerification CRD repeats them as
// maxLength/maxItems.
const (
	// MaxResponseBytes is the most of a response body the tester reads;
	// past it the step is TransportTooLarge.
	MaxResponseBytes = 64 << 10
	// MaxCaptureBytes bounds a captured value.
	MaxCaptureBytes = 128
	// MaxRunBytes bounds a {{run}} token, as a try records it.
	MaxRunBytes = 64
)

// TooLarge stands for an observed value over MaxValueBytes. It is not JSON,
// so it can never equal an expected value; only $present judges it.
const TooLarge = "!too-large"

// Transport classifies how a request failed before a response was judged.
type Transport string

// Transport classes. Anything but TransportNone fails the step.
const (
	TransportNone           Transport = "none"
	TransportRefused        Transport = "refused"
	TransportTimeout        Transport = "timeout"
	TransportReset          Transport = "reset"
	TransportDNS            Transport = "dns"
	TransportTLS            Transport = "tls"
	TransportTooLarge       Transport = "too-large"
	TransportCaptureMissing Transport = "capture-missing"
)

var transports = map[Transport]bool{
	TransportNone: true, TransportRefused: true, TransportTimeout: true, TransportReset: true,
	TransportDNS: true, TransportTLS: true, TransportTooLarge: true, TransportCaptureMissing: true,
}

// Known reports whether t is one of the transport classes.
func (t Transport) Known() bool { return transports[t] }

// Transports lists the transport classes, in no particular order.
func Transports() []Transport { return slices.Collect(maps.Keys(transports)) }
