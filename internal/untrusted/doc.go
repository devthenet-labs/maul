// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package untrusted bounds and makes legible text that an outsider wrote (a
// provider's check output, a pull request comment, an acceptance evidence
// field) before it reaches a human or a model. It is pure: the standard
// library only, a test pins that.
//
// Visible is the one pass a field takes: cut to its byte bound on a rune
// boundary, every invisible or invalid byte shown as a printed escape, and
// cut again to the bound, so escaping can never grow a field without limit.
package untrusted
