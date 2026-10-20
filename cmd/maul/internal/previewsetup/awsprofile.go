// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package previewsetup

import (
	"bufio"
	"bytes"
	"fmt"
	"slices"
	"strings"
)

// AWSKey is one IAM user's access key, read out of an AWS shared
// credentials file for the route53 solver: the access key ID (not a
// secret, it lands in answers.yaml and the Issuer) and the secret access
// key (a Credential, written only into the Secret manifest).
type AWSKey struct {
	ID     string
	Secret Credential
}

// ParseAWSProfile reads profile's access key out of an AWS shared
// credentials file (~/.aws/credentials: [name] sections of key = value
// lines, # and ; comments). Only that profile is read: the file usually
// holds every account's keys, and the others never leave it. It refuses
// a profile that is missing or given twice, a setting given twice, a
// nested (indented) setting, a session token (temporary credentials
// expire, and cert-manager cannot renew them), and a key ID that is not
// an IAM user's. No error quotes a value; profile names are not secret,
// so a missing profile lists the ones there are.
func ParseAWSProfile(raw []byte, profile string) (AWSKey, error) {
	if len(raw) > maxCredential {
		return AWSKey{}, fmt.Errorf("%w: more than %d bytes", ErrCredential, maxCredential)
	}
	settings, err := awsProfileSettings(raw, profile)
	if err != nil {
		return AWSKey{}, err
	}
	if settings["aws_session_token"] != "" {
		return AWSKey{}, fmt.Errorf("%w: profile %s holds temporary credentials (aws_session_token), which "+
			"expire: give an IAM user's access key", ErrCredential, profile)
	}
	id, secret := settings["aws_access_key_id"], settings["aws_secret_access_key"]
	if id == "" || secret == "" {
		return AWSKey{}, fmt.Errorf("%w: profile %s needs both aws_access_key_id and aws_secret_access_key",
			ErrCredential, profile)
	}
	if !awsKeyIDPattern.MatchString(id) {
		return AWSKey{}, fmt.Errorf("%w: profile %s's aws_access_key_id is not an IAM user's (AKIA...)",
			ErrCredential, profile)
	}
	if strings.ContainsFunc(secret, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return AWSKey{}, fmt.Errorf("%w: profile %s's aws_secret_access_key holds white space or a control "+
			"character", ErrCredential, profile)
	}
	return AWSKey{ID: id, Secret: Credential{value: []byte(secret)}}, nil
}

// awsProfileSettings is profile's settings, keys lowercased, values
// trimmed; every other profile's lines are skipped unread.
func awsProfileSettings(raw []byte, profile string) (map[string]string, error) {
	var (
		names         []string
		found, inside bool
		settings      = map[string]string{}
	)
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 4096), maxCredential)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "" || trimmed[0] == '#' || trimmed[0] == ';':
			continue
		case trimmed[0] == '[':
			if !strings.HasSuffix(trimmed, "]") {
				return nil, fmt.Errorf("%w: line %d is not a [profile] header", ErrCredential, n)
			}
			name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			names = append(names, name)
			inside = name == profile
			if inside && found {
				return nil, fmt.Errorf("%w: profile %s is in the file twice", ErrCredential, profile)
			}
			found = found || inside
			continue
		case !inside:
			continue
		case line[0] == ' ' || line[0] == '\t':
			return nil, fmt.Errorf("%w: line %d of profile %s is a nested setting: give a profile of "+
				"plain key = value lines", ErrCredential, n, profile)
		}
		k, v, ok := strings.Cut(trimmed, "=")
		if !ok {
			return nil, fmt.Errorf("%w: line %d of profile %s is not key = value", ErrCredential, n, profile)
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if _, dup := settings[k]; dup {
			return nil, fmt.Errorf("%w: profile %s sets %s twice", ErrCredential, profile, k)
		}
		settings[k] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCredential, err)
	}
	if !found {
		slices.Sort(names)
		return nil, fmt.Errorf("%w: no profile %s in the file (it has: %s)", ErrCredential, profile,
			strings.Join(slices.Compact(names), ", "))
	}
	return settings, nil
}
