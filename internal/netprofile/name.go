// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package netprofile

import "fmt"

// ValidName reports whether name is a profile name: a DNS label of at most
// MaxNameLen characters that starts with a letter. It is the grammar
// Profile.Validate applies, for a caller that holds a name alone (the jobs
// package, judging the profile a controller asked a Job to carry).
func ValidName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("profile name %q is not a DNS label of at most %d characters starting with a letter",
			name, MaxNameLen)
	}
	return nil
}
