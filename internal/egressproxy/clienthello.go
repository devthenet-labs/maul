// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/cryptobyte"
)

// MaxHelloBytes bounds the TLS records that carry one ClientHello, record
// headers included (§2.3 step 6 of the step-3 spec): the tunnel never
// buffers more than this before judging the hello.
const MaxHelloBytes = 64 << 10

// TLS wire constants the parser judges (RFC 8446, RFC 6066, RFC 7301 and
// draft-ietf-tls-esni).
const (
	recordHeaderLen    = 5
	maxRecordLen       = 1 << 14 // a plaintext record's payload limit
	recordTypeHS       = 22
	minRecordVersion   = 0x0301
	maxRecordVersion   = 0x0304
	handshakeHeaderLen = 4
	typeClientHello    = 1

	extServerName = 0x0000
	extALPN       = 0x0010
	extECH        = 0xfe0d // encrypted_client_hello
	extESNI       = 0xffce // the legacy encrypted_server_name draft

	sniHostName = 0
)

// The ways a ClientHello is refused. ErrBadHello is every malformation (the
// tunnel's bad_hello); ErrHelloTooLarge wraps it, so errors.Is(err,
// ErrBadHello) holds for both. ErrSNIList is a server_name extension that is
// well-formed but is not exactly one non-empty printable host_name (the
// tunnel's sni_missing). ErrHelloIncomplete is ParseClientHello's "the
// bytes end before the hello does": read more, never forward.
var (
	ErrBadHello        = errors.New("egress-proxy: malformed TLS ClientHello")
	ErrHelloTooLarge   = fmt.Errorf("%w: larger than %d bytes", ErrBadHello, MaxHelloBytes)
	ErrSNIList         = errors.New("egress-proxy: the ClientHello's server_name is not exactly one host_name")
	ErrHelloIncomplete = errors.New("egress-proxy: the ClientHello is incomplete")
)

// Hello is what the tunnel reads from a client's ClientHello. Nothing else
// in the hello is kept: the proxy forwards the bytes verbatim and never
// terminates TLS.
type Hello struct {
	// SNI is the server_name extension's one host_name exactly as sent
	// (printable ASCII, not normalised); "" when the hello has no
	// server_name extension.
	SNI string
	// ALPN is the application_layer_protocol_negotiation list in order;
	// nil when absent. Recorded for the audit line only.
	ALPN []string
	// HasECH reports an encrypted_client_hello (0xfe0d) or legacy ESNI
	// (0xffce) extension: an inner name the proxy cannot see.
	HasECH bool
	// Legacy reports that the ECH signal was the legacy ESNI extension.
	Legacy bool
}

func badHello(what string) error {
	return fmt.Errorf("%w: %s", ErrBadHello, what)
}

// assembler reassembles the handshake message the client's first records
// carry, judging each record header as it arrives, in linear time whatever
// the fragmentation.
type assembler struct {
	records int // record bytes seen, headers included
	hs      []byte
}

// header judges one record header and returns its payload length.
func (a *assembler) header(h []byte) (int, error) {
	if h[0] != recordTypeHS {
		return 0, badHello("not a handshake record")
	}
	if v := int(h[1])<<8 | int(h[2]); v < minRecordVersion || v > maxRecordVersion {
		return 0, badHello("record version out of range")
	}
	n := int(h[3])<<8 | int(h[4])
	if n == 0 || n > maxRecordLen {
		return 0, badHello("record length out of range")
	}
	if a.records+recordHeaderLen+n > MaxHelloBytes {
		return 0, ErrHelloTooLarge
	}
	return n, nil
}

// payload appends one record's payload and reports whether the ClientHello
// is now complete. A record that carries bytes past the hello is refused: a
// client's first flight holds the ClientHello alone.
func (a *assembler) payload(p []byte) (bool, error) {
	a.records += recordHeaderLen + len(p)
	a.hs = grow(a.hs, len(p))
	copy(a.hs[len(a.hs)-len(p):], p)
	if len(a.hs) < handshakeHeaderLen {
		return false, nil
	}
	if a.hs[0] != typeClientHello {
		return false, badHello("first handshake message is not a ClientHello")
	}
	need := handshakeHeaderLen + (int(a.hs[1])<<16 | int(a.hs[2])<<8 | int(a.hs[3]))
	if need > MaxHelloBytes {
		return false, ErrHelloTooLarge
	}
	switch {
	case len(a.hs) < need:
		return false, nil
	case len(a.hs) > need:
		return false, badHello("bytes after the ClientHello in its record")
	}
	return true, nil
}

// ParseClientHello parses a client's first TLS records: one or more
// handshake records (type 22, record version 0x0301-0x0304, non-empty, at
// most 2^14 bytes each) whose payloads concatenate to exactly one
// ClientHello. Bytes after the record that completes the hello are not
// read. It returns ErrHelloIncomplete when b ends first, ErrBadHello (or
// ErrHelloTooLarge) for any malformation and ErrSNIList for an unusable
// server_name list.
func ParseClientHello(b []byte) (Hello, error) {
	var a assembler
	for {
		if len(b) < recordHeaderLen {
			if a.records+recordHeaderLen+1 > MaxHelloBytes {
				return Hello{}, ErrHelloTooLarge // no further record fits
			}
			return Hello{}, ErrHelloIncomplete
		}
		n, err := a.header(b[:recordHeaderLen])
		if err != nil {
			return Hello{}, err
		}
		if len(b) < recordHeaderLen+n {
			return Hello{}, ErrHelloIncomplete
		}
		done, err := a.payload(b[recordHeaderLen : recordHeaderLen+n])
		if err != nil {
			return Hello{}, err
		}
		if done {
			return parseHelloMessage(a.hs)
		}
		b = b[recordHeaderLen+n:]
	}
}

// ReadClientHello reads a client's first TLS records from r one record at a
// time, never past the record that completes the ClientHello and never more
// than MaxHelloBytes, and returns the parsed hello with the exact bytes read
// (to be forwarded upstream verbatim once the hello is judged). A stream
// that ends inside the hello is an error wrapping io.ErrUnexpectedEOF. The
// caller sets the read deadline.
func ReadClientHello(r io.Reader) (Hello, []byte, error) {
	var (
		a   assembler
		raw []byte
		hdr [recordHeaderLen]byte
	)
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return Hello{}, raw, eof(err)
		}
		n, err := a.header(hdr[:])
		if err != nil {
			return Hello{}, append(raw, hdr[:]...), err
		}
		raw = grow(raw, recordHeaderLen+n)
		start := len(raw) - n
		copy(raw[start-recordHeaderLen:], hdr[:])
		if _, err := io.ReadFull(r, raw[start:]); err != nil {
			return Hello{}, raw[:start], eof(err)
		}
		done, err := a.payload(raw[start:])
		if err != nil {
			return Hello{}, raw, err
		}
		if done {
			h, err := parseHelloMessage(a.hs)
			return h, raw, err
		}
	}
}

// grow extends b by n bytes, never to a capacity past MaxHelloBytes (the
// header check has already bounded len(b)+n by it).
func grow(b []byte, n int) []byte {
	if len(b)+n <= cap(b) {
		return b[:len(b)+n]
	}
	c := min(max(2*cap(b), len(b)+n, 512), MaxHelloBytes)
	nb := make([]byte, len(b)+n, c)
	copy(nb, b)
	return nb
}

func eof(err error) error {
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("egress-proxy: the stream ended inside the ClientHello: %w", io.ErrUnexpectedEOF)
	}
	return err
}

// parseHelloMessage parses one complete handshake message, header
// included, that assembler has already checked is a ClientHello of exactly
// this length.
func parseHelloMessage(msg []byte) (Hello, error) {
	s := cryptobyte.String(msg[handshakeHeaderLen:])
	if !skipFixedFields(&s) {
		return Hello{}, badHello("bad fixed fields")
	}
	if s.Empty() {
		return Hello{}, nil // a hello without extensions: no SNI
	}
	var exts cryptobyte.String
	if !s.ReadUint16LengthPrefixed(&exts) || !s.Empty() {
		return Hello{}, badHello("bad extensions block")
	}
	return parseExtensions(exts)
}

// skipFixedFields reads past legacy_version, random, legacy_session_id,
// cipher_suites and legacy_compression_methods, judging their lengths.
func skipFixedFields(s *cryptobyte.String) bool {
	var (
		version                     uint16
		random                      []byte
		sessionID, suites, compress cryptobyte.String
	)
	return s.ReadUint16(&version) && s.ReadBytes(&random, 32) &&
		s.ReadUint8LengthPrefixed(&sessionID) && len(sessionID) <= 32 &&
		s.ReadUint16LengthPrefixed(&suites) && len(suites) > 0 && len(suites)%2 == 0 &&
		s.ReadUint8LengthPrefixed(&compress) && len(compress) > 0
}

// parseExtensions reads the extensions block: each type at most once, the
// ones the tunnel judges parsed strictly, every other one skipped.
func parseExtensions(exts cryptobyte.String) (Hello, error) {
	var (
		h      Hello
		sniErr error
		seen   = make(map[uint16]bool)
	)
	for !exts.Empty() {
		var (
			typ  uint16
			data cryptobyte.String
		)
		if !exts.ReadUint16(&typ) || !exts.ReadUint16LengthPrefixed(&data) {
			return Hello{}, badHello("truncated extension")
		}
		if seen[typ] {
			return Hello{}, badHello("duplicate extension")
		}
		seen[typ] = true
		var err error
		switch typ {
		case extServerName:
			h.SNI, err = parseServerName(data)
			if errors.Is(err, ErrSNIList) {
				sniErr, err = err, nil
			}
		case extALPN:
			h.ALPN, err = parseALPN(data)
		case extECH:
			h.HasECH = true
		case extESNI:
			h.HasECH, h.Legacy = true, true
		}
		if err != nil {
			return Hello{}, err
		}
	}
	if sniErr != nil {
		// Judged after every extension, so a malformation anywhere in the
		// hello wins over an unusable name.
		return Hello{}, sniErr
	}
	return h, nil
}

// parseServerName reads RFC 6066's ServerNameList, which the tunnel
// accepts only as exactly one host_name entry of printable ASCII.
func parseServerName(data cryptobyte.String) (string, error) {
	var list cryptobyte.String
	if !data.ReadUint16LengthPrefixed(&list) || !data.Empty() {
		return "", badHello("bad server_name extension")
	}
	var (
		entries int
		name    string
		usable  = true
	)
	for !list.Empty() {
		var (
			typ uint8
			n   cryptobyte.String
		)
		if !list.ReadUint8(&typ) || !list.ReadUint16LengthPrefixed(&n) {
			return "", badHello("bad server_name entry")
		}
		entries++
		if typ != sniHostName || len(n) == 0 || !printable(n) {
			usable = false
		}
		name = string(n)
	}
	if entries != 1 || !usable {
		return "", ErrSNIList
	}
	return name, nil
}

// parseALPN reads RFC 7301's ProtocolNameList: non-empty, of non-empty
// names.
func parseALPN(data cryptobyte.String) ([]string, error) {
	var list cryptobyte.String
	if !data.ReadUint16LengthPrefixed(&list) || !data.Empty() || list.Empty() {
		return nil, badHello("bad ALPN extension")
	}
	var protos []string
	for !list.Empty() {
		var p cryptobyte.String
		if !list.ReadUint8LengthPrefixed(&p) || len(p) == 0 {
			return nil, badHello("bad ALPN protocol")
		}
		protos = append(protos, string(p))
	}
	return protos, nil
}

func printable(b []byte) bool {
	for _, c := range b {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}
