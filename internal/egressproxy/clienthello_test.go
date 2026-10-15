// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package egressproxy

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"math/rand"
	"net"
	"reflect"
	"slices"
	"testing"
	"testing/iotest"
	"testing/quick"
	"time"

	"golang.org/x/crypto/cryptobyte"
)

// recordConn is the client half of a handshake that never gets an answer:
// it records what the client writes and ends the stream on the first read.
type recordConn struct {
	out bytes.Buffer
}

func (c *recordConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *recordConn) Write(b []byte) (int, error)      { return c.out.Write(b) }
func (c *recordConn) Close() error                     { return nil }
func (c *recordConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *recordConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *recordConn) SetDeadline(time.Time) error      { return nil }
func (c *recordConn) SetReadDeadline(time.Time) error  { return nil }
func (c *recordConn) SetWriteDeadline(time.Time) error { return nil }

// replayConn is the server half: it replays recorded client bytes and
// discards what the server writes back.
type replayConn struct {
	recordConn
	in io.Reader
}

func (c *replayConn) Read(b []byte) (int, error)  { return c.in.Read(b) }
func (c *replayConn) Write(b []byte) (int, error) { return len(b), nil }

// stdlibHello records the first flight a real crypto/tls client sends.
func stdlibHello(t *testing.T, cfg *tls.Config) []byte {
	t.Helper()
	c := &recordConn{}
	if err := tls.Client(c, cfg).Handshake(); err == nil {
		t.Fatal("handshake against a silent peer succeeded")
	}
	return c.out.Bytes()
}

var errStopHandshake = errors.New("stop")

// stdlibSees is what a crypto/tls server's GetConfigForClient is shown for
// the same bytes; ok is false when the server refuses them before that.
func stdlibSees(b []byte) (*tls.ClientHelloInfo, bool) {
	var seen *tls.ClientHelloInfo
	srv := tls.Server(&replayConn{in: bytes.NewReader(b)}, &tls.Config{
		GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
			seen = chi
			return nil, errStopHandshake
		},
	})
	_ = srv.Handshake()
	return seen, seen != nil
}

// handshakeBytes concatenates the payloads of well-formed records.
func handshakeBytes(t *testing.T, records []byte) []byte {
	t.Helper()
	var hs []byte
	for len(records) > 0 {
		if len(records) < recordHeaderLen {
			t.Fatalf("trailing %d bytes", len(records))
		}
		n := int(records[3])<<8 | int(records[4])
		hs = append(hs, records[recordHeaderLen:recordHeaderLen+n]...)
		records = records[recordHeaderLen+n:]
	}
	return hs
}

// fragment wraps hs in handshake records whose payload sizes are taken in
// turn from sizes (the last size repeats), each at most maxRecordLen.
func fragment(hs []byte, sizes ...int) []byte {
	var out []byte
	for i := 0; len(hs) > 0; i++ {
		n := sizes[min(i, len(sizes)-1)]
		n = min(max(n, 1), maxRecordLen, len(hs))
		out = append(out, recordTypeHS, 0x03, 0x01, byte(n>>8), byte(n))
		out = append(out, hs[:n]...)
		hs = hs[n:]
	}
	return out
}

// chunkReader returns at most the next size in turn per Read, so a hello
// arrives in the TCP reads a property draws.
type chunkReader struct {
	r     io.Reader
	sizes []int
	i     int
}

func (c *chunkReader) Read(b []byte) (int, error) {
	n := c.sizes[c.i%len(c.sizes)]
	c.i++
	return c.r.Read(b[:min(len(b), max(n, 1))])
}

// genHelloCase is one client configuration and one way its hello is cut
// into records and TCP reads.
type genHelloCase struct {
	Host    string
	ALPN    []string
	TLS13   bool
	Tickets bool
	Records []int
	Reads   []int
}

func (genHelloCase) Generate(r *rand.Rand, _ int) reflect.Value {
	c := genHelloCase{
		Host:    string(genHost("").Generate(r, 0).Interface().(genHost)),
		TLS13:   r.Intn(2) == 0,
		Tickets: r.Intn(2) == 0,
	}
	for range r.Intn(4) {
		switch r.Intn(3) {
		case 0:
			c.ALPN = append(c.ALPN, "h2")
		case 1:
			c.ALPN = append(c.ALPN, "http/1.1")
		default:
			c.ALPN = append(c.ALPN, randString(r, lowerDigits+"/.-_", 1, 20))
		}
	}
	for range 1 + r.Intn(4) {
		c.Records = append(c.Records, 1+r.Intn(600))
		c.Reads = append(c.Reads, 1+r.Intn(700))
	}
	return reflect.ValueOf(c)
}

// TestPropertyHelloMatchesStdlib (§8.2's PropHelloMatchesStdlib): for
// hellos a real crypto/tls client writes (TLS 1.2 and 1.3, random SNI and
// ALPN, session tickets on and off), cut into random records and random
// TCP reads, ParseClientHello and ReadClientHello see the SNI and ALPN a
// crypto/tls server's GetConfigForClient sees for the same bytes, no ECH,
// and ReadClientHello consumes exactly those bytes.
func TestPropertyHelloMatchesStdlib(t *testing.T) {
	prop := func(c genHelloCase) bool {
		cfg := &tls.Config{
			ServerName:             c.Host,
			NextProtos:             c.ALPN,
			MinVersion:             tls.VersionTLS12,
			MaxVersion:             tls.VersionTLS12,
			SessionTicketsDisabled: !c.Tickets,
		}
		if c.TLS13 {
			cfg.MaxVersion = tls.VersionTLS13
		}
		wire := fragment(handshakeBytes(t, stdlibHello(t, cfg)), c.Records...)
		seen, ok := stdlibSees(wire)
		if !ok {
			t.Logf("crypto/tls refused the refragmented hello for %+v", c)
			return false
		}
		parsed, err := ParseClientHello(wire)
		if err != nil {
			t.Logf("ParseClientHello: %v for %+v", err, c)
			return false
		}
		read, raw, err := ReadClientHello(&chunkReader{r: bytes.NewReader(wire), sizes: c.Reads})
		if err != nil || !bytes.Equal(raw, wire) || !reflect.DeepEqual(read, parsed) {
			t.Logf("ReadClientHello: %+v, %d of %d bytes, %v", read, len(raw), len(wire), err)
			return false
		}
		if parsed.SNI != seen.ServerName || parsed.SNI != c.Host || parsed.HasECH ||
			!slices.Equal(parsed.ALPN, seen.SupportedProtos) {
			t.Logf("parsed %+v, crypto/tls saw ServerName=%q ALPN=%q", parsed, seen.ServerName, seen.SupportedProtos)
			return false
		}
		return true
	}
	if err := quick.Check(prop, quickConfig(300)); err != nil {
		t.Error(err)
	}
}

// ext is one raw extension for buildHello.
type ext struct {
	typ  uint16
	data []byte
}

// buildHello writes a ClientHello handshake message (header included) with
// exactly the given extensions; noExts omits the extensions block.
func buildHello(noExts bool, exts ...ext) []byte {
	var b cryptobyte.Builder
	b.AddUint8(typeClientHello)
	b.AddUint24LengthPrefixed(func(b *cryptobyte.Builder) {
		b.AddUint16(0x0303)
		b.AddBytes(make([]byte, 32))
		b.AddUint8LengthPrefixed(func(*cryptobyte.Builder) {})
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) { b.AddUint16(0x1301) })
		b.AddUint8LengthPrefixed(func(b *cryptobyte.Builder) { b.AddUint8(0) })
		if noExts {
			return
		}
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
			for _, e := range exts {
				b.AddUint16(e.typ)
				b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes(e.data) })
			}
		})
	})
	return b.BytesOrPanic()
}

type sniEntry struct {
	typ  uint8
	name string
}

func sniExt(entries ...sniEntry) ext {
	var b cryptobyte.Builder
	b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
		for _, e := range entries {
			b.AddUint8(e.typ)
			b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes([]byte(e.name)) })
		}
	})
	return ext{extServerName, b.BytesOrPanic()}
}

func host(name string) ext { return sniExt(sniEntry{sniHostName, name}) }

func alpnExt(protos ...string) ext {
	var b cryptobyte.Builder
	b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
		for _, p := range protos {
			b.AddUint8LengthPrefixed(func(b *cryptobyte.Builder) { b.AddBytes([]byte(p)) })
		}
	})
	return ext{extALPN, b.BytesOrPanic()}
}

func record(typ byte, version uint16, payload []byte) []byte {
	return append([]byte{typ, byte(version >> 8), byte(version), byte(len(payload) >> 8), byte(len(payload))},
		payload...)
}

// sizedHello is a one-SNI hello padded (extension 21) so its records,
// cut at maxRecordLen and headers included, total exactly n bytes.
func sizedHello(t *testing.T, n int) []byte {
	t.Helper()
	base := len(buildHello(false, host("pypi.org"), ext{21, nil}))
	for records := 1; records <= 8; records++ {
		hs := n - records*recordHeaderLen
		if (hs+maxRecordLen-1)/maxRecordLen != records || hs < base {
			continue
		}
		wire := fragment(buildHello(false, host("pypi.org"), ext{21, make([]byte, hs-base)}), maxRecordLen)
		if len(wire) != n {
			t.Fatalf("sizedHello(%d) built %d bytes", n, len(wire))
		}
		return wire
	}
	t.Fatalf("no padding reaches %d bytes", n)
	return nil
}

// nearCap is an unfinished ClientHello declaring a 65516-byte body (within
// the cap), with present handshake bytes cut into maxRecordLen records: the
// records end within a record header of MaxHelloBytes, so no further
// record fits.
func nearCap(present int) []byte {
	declared := 65516
	hs := make([]byte, present)
	hs[0], hs[1], hs[2], hs[3] = typeClientHello, byte(declared>>16), byte(declared>>8), byte(declared)
	return fragment(hs, maxRecordLen)
}

// truncated is hello with its last byte dropped and its handshake length
// adjusted, so the message is complete but its extensions block is not.
func truncated(hello []byte) []byte {
	m := slices.Clone(hello[:len(hello)-1])
	n := len(m) - handshakeHeaderLen
	m[1], m[2], m[3] = byte(n>>16), byte(n>>8), byte(n)
	return m
}

func TestParseClientHelloExamples(t *testing.T) {
	one := func(exts ...ext) []byte { return fragment(buildHello(false, exts...), maxRecordLen) }
	twoRecords := fragment(buildHello(false, host("pypi.org"), alpnExt("h2", "http/1.1")), 7, maxRecordLen)
	hello := buildHello(false, host("pypi.org"))
	sslv2 := []byte{0x80, 0x2e, 0x01, 0x00, 0x02, 0x00, 0x15, 0x00, 0x00, 0x00, 0x10}
	serverHello := append([]byte{2}, hello[1:]...)

	cases := []struct {
		name string
		wire []byte
		want Hello
		err  error
	}{
		{"sni and alpn", one(host("pypi.org"), alpnExt("h2", "http/1.1")),
			Hello{SNI: "pypi.org", ALPN: []string{"h2", "http/1.1"}}, nil},
		{"unknown extensions ignored", one(ext{0x000a, []byte{0, 2, 0, 0x1d}}, host("pypi.org"), ext{0x4469, nil}),
			Hello{SNI: "pypi.org"}, nil},
		{"no sni", one(alpnExt("h2")), Hello{ALPN: []string{"h2"}}, nil},
		{"no extensions block", fragment(buildHello(true), maxRecordLen), Hello{}, nil},
		{"sni case kept", one(host("PyPI.org")), Hello{SNI: "PyPI.org"}, nil},
		{"sni trailing dot kept", one(host("pypi.org.")), Hello{SNI: "pypi.org."}, nil},
		{"ip in sni kept", one(host("1.2.3.4")), Hello{SNI: "1.2.3.4"}, nil},
		{"ech", one(host("pypi.org"), ext{extECH, []byte{0, 1, 2}}), Hello{SNI: "pypi.org", HasECH: true}, nil},
		{"legacy esni", one(host("pypi.org"), ext{extESNI, []byte{0}}),
			Hello{SNI: "pypi.org", HasECH: true, Legacy: true}, nil},
		{"two records", twoRecords, Hello{SNI: "pypi.org", ALPN: []string{"h2", "http/1.1"}}, nil},
		{"one-byte records", fragment(hello, 1), Hello{SNI: "pypi.org"}, nil},
		{"record version 0x0303", record(recordTypeHS, 0x0303, hello), Hello{SNI: "pypi.org"}, nil},
		{"record version 0x0304", record(recordTypeHS, 0x0304, hello), Hello{SNI: "pypi.org"}, nil},
		{"exactly the cap", sizedHello(t, MaxHelloBytes), Hello{SNI: "pypi.org"}, nil},

		{"two sni entries", one(sniExt(sniEntry{0, "pypi.org"}, sniEntry{0, "evil.example"})), Hello{}, ErrSNIList},
		{"non host_name type", one(sniExt(sniEntry{1, "pypi.org"})), Hello{}, ErrSNIList},
		{"host_name beside another type", one(sniExt(sniEntry{0, "pypi.org"}, sniEntry{1, "x"})), Hello{}, ErrSNIList},
		{"empty sni list", one(sniExt()), Hello{}, ErrSNIList},
		{"empty sni name", one(host("")), Hello{}, ErrSNIList},
		{"control byte in sni", one(host("pypi.org\r\nx")), Hello{}, ErrSNIList},
		{"space in sni", one(host("pypi .org")), Hello{}, ErrSNIList},
		{"non-ascii sni", one(host("pyp\xc3\xaf.org")), Hello{}, ErrSNIList},
		{"malformation beats a bad name", one(sniExt(), ext{extALPN, []byte{0, 0}}), Hello{}, ErrBadHello},

		{"64 KiB + 1", sizedHello(t, MaxHelloBytes+1), Hello{}, ErrHelloTooLarge},
		// Pinned boundary inputs: the records seen end within a record
		// header of the cap and the hello is unfinished, so no further
		// record fits. Both functions refuse there and read nothing more.
		{"no further record fits, stream ends", nearCap(65511), Hello{}, ErrHelloTooLarge},
		{"no further record fits, a record follows",
			append(nearCap(65513), record(recordTypeHS, 0x0301, []byte{0})...), Hello{}, ErrHelloTooLarge},
		{"declared length over the cap", record(recordTypeHS, 0x0301, []byte{1, 0x01, 0x00, 0x00}),
			Hello{}, ErrHelloTooLarge},
		{"application data first", record(23, 0x0303, hello), Hello{}, ErrBadHello},
		{"alert first", record(21, 0x0303, []byte{2, 40}), Hello{}, ErrBadHello},
		{"sslv2 hello", sslv2, Hello{}, ErrBadHello},
		{"handshake type 2", fragment(serverHello, maxRecordLen), Hello{}, ErrBadHello},
		{"record version 0x0300", record(recordTypeHS, 0x0300, hello), Hello{}, ErrBadHello},
		{"record version 0x0305", record(recordTypeHS, 0x0305, hello), Hello{}, ErrBadHello},
		{"empty record", append(record(recordTypeHS, 0x0301, nil), fragment(hello, maxRecordLen)...),
			Hello{}, ErrBadHello},
		{"record over 2^14", record(recordTypeHS, 0x0301, append(slices.Clone(hello), make([]byte, maxRecordLen)...)),
			Hello{}, ErrBadHello},
		{"alert between records", append(append(fragment(hello[:10], maxRecordLen), record(21, 0x0303, []byte{1, 0})...),
			fragment(hello[10:], maxRecordLen)...), Hello{}, ErrBadHello},
		{"bytes after the hello in its record", record(recordTypeHS, 0x0301, append(slices.Clone(hello), 0)),
			Hello{}, ErrBadHello},
		{"duplicate extension", one(host("pypi.org"), host("pypi.org")), Hello{}, ErrBadHello},
		{"duplicate alpn", one(alpnExt("h2"), alpnExt("h2")), Hello{}, ErrBadHello},
		{"empty alpn list", one(alpnExt()), Hello{}, ErrBadHello},
		{"empty alpn protocol", one(alpnExt("")), Hello{}, ErrBadHello},
		{"alpn trailing bytes", one(ext{extALPN, []byte{0, 3, 2, 'h', '2', 0}}), Hello{}, ErrBadHello},
		{"sni trailing bytes", one(ext{extServerName, append(sniExt(sniEntry{0, "a.b"}).data, 0)}), Hello{}, ErrBadHello},
		{"sni truncated entry", one(ext{extServerName, []byte{0, 3, 0, 0, 9}}), Hello{}, ErrBadHello},
		{"truncated extension", fragment(truncated(hello), maxRecordLen),
			Hello{}, ErrBadHello},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseClientHello(c.wire)
			if !errors.Is(err, c.err) || (c.err == nil && err != nil) || !reflect.DeepEqual(got, c.want) {
				t.Errorf("ParseClientHello = %+v, %v; want %+v, %v", got, err, c.want, c.err)
			}
			read, raw, rerr := ReadClientHello(iotest.OneByteReader(bytes.NewReader(c.wire)))
			if !errors.Is(rerr, c.err) || (c.err == nil && rerr != nil) || !reflect.DeepEqual(read, c.want) {
				t.Errorf("ReadClientHello = %+v, %v; want %+v, %v", read, rerr, c.want, c.err)
			}
			if c.err == nil && !bytes.Equal(raw, c.wire) {
				t.Errorf("ReadClientHello consumed %d bytes, want all %d", len(raw), len(c.wire))
			}
			if len(raw) > MaxHelloBytes || cap(raw) > MaxHelloBytes {
				t.Errorf("ReadClientHello buffered %d (cap %d) bytes, over the cap", len(raw), cap(raw))
			}
		})
	}
}

// TestParseClientHelloIncomplete: every proper prefix of a hello is
// incomplete to ParseClientHello and an unexpected EOF to ReadClientHello.
func TestParseClientHelloIncomplete(t *testing.T) {
	wire := fragment(buildHello(false, host("pypi.org"), alpnExt("h2")), 9)
	for n := range len(wire) {
		if _, err := ParseClientHello(wire[:n]); !errors.Is(err, ErrHelloIncomplete) {
			t.Errorf("ParseClientHello(first %d bytes) = %v, want ErrHelloIncomplete", n, err)
		}
		if _, _, err := ReadClientHello(bytes.NewReader(wire[:n])); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("ReadClientHello(first %d bytes) = %v, want io.ErrUnexpectedEOF", n, err)
		}
	}
}

// TestReadClientHelloStopsAtTheHello: bytes the client sends after its
// hello (0-RTT data, a pipelined record) stay unread for the splice.
func TestReadClientHelloStopsAtTheHello(t *testing.T) {
	wire := fragment(buildHello(false, host("pypi.org")), 11)
	after := record(23, 0x0303, []byte("early data"))
	r := bytes.NewReader(append(slices.Clone(wire), after...))
	h, raw, err := ReadClientHello(r)
	if err != nil || h.SNI != "pypi.org" || !bytes.Equal(raw, wire) {
		t.Fatalf("ReadClientHello = %+v, %d bytes, %v", h, len(raw), err)
	}
	rest, _ := io.ReadAll(r)
	if !bytes.Equal(rest, after) {
		t.Errorf("left %q unread, want %q", rest, after)
	}
	if h2, err := ParseClientHello(append(slices.Clone(wire), after...)); err != nil || h2.SNI != "pypi.org" {
		t.Errorf("ParseClientHello with trailing records = %+v, %v", h2, err)
	}
}

// TestReadClientHelloReadError: a read error that is not EOF (a deadline)
// comes back as itself.
func TestReadClientHelloReadError(t *testing.T) {
	boom := errors.New("i/o timeout")
	wire := fragment(buildHello(false, host("pypi.org")), maxRecordLen)
	r := io.MultiReader(bytes.NewReader(wire[:12]), iotest.ErrReader(boom))
	if _, _, err := ReadClientHello(r); !errors.Is(err, boom) {
		t.Errorf("ReadClientHello = %v, want %v", err, boom)
	}
}

// countingReader counts the bytes a reader hands out.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += n
	return n, err
}

func FuzzParseClientHello(f *testing.F) {
	f.Add(fragment(buildHello(false, host("pypi.org"), alpnExt("h2", "http/1.1")), maxRecordLen))
	f.Add(fragment(buildHello(false, host("pypi.org")), 1))
	f.Add(fragment(buildHello(true), 3))
	f.Add(fragment(buildHello(false, sniExt(sniEntry{0, "a.b"}, sniEntry{0, "c.d"})), maxRecordLen))
	f.Add(fragment(buildHello(false, sniExt(sniEntry{1, "a.b"})), maxRecordLen))
	f.Add(fragment(buildHello(false, host("pypi.org"), ext{extECH, []byte{1}}), maxRecordLen))
	f.Add(fragment(buildHello(false, host("pypi.org"), ext{extESNI, nil}), maxRecordLen))
	f.Add(fragment(buildHello(false, host("1.2.3.4.")), maxRecordLen))
	f.Add(record(23, 0x0303, []byte("x")))
	f.Add([]byte{0x80, 0x2e, 0x01, 0x00, 0x02})
	f.Add(record(recordTypeHS, 0x0301, []byte{1, 0x01, 0x00, 0x00}))
	f.Add([]byte{})
	f.Add(nearCap(65511))
	f.Add(append(nearCap(65513), record(recordTypeHS, 0x0301, []byte{0})...))
	f.Fuzz(func(t *testing.T, data []byte) {
		parsed, perr := ParseClientHello(data) // must not panic on any input
		cr := &countingReader{r: bytes.NewReader(data)}
		read, raw, rerr := ReadClientHello(cr)
		if cr.n > MaxHelloBytes || len(raw) > MaxHelloBytes || cap(raw) > MaxHelloBytes {
			t.Fatalf("read %d bytes, buffered %d (cap %d): over the %d-byte cap", cr.n, len(raw), cap(raw), MaxHelloBytes)
		}
		if !bytes.Equal(raw, data[:len(raw)]) {
			t.Fatal("ReadClientHello returned bytes it did not read")
		}
		if (perr == nil) != (rerr == nil) {
			t.Fatalf("ParseClientHello err %v, ReadClientHello err %v", perr, rerr)
		}
		if perr != nil {
			if errors.Is(perr, ErrHelloIncomplete) != errors.Is(rerr, io.ErrUnexpectedEOF) {
				t.Fatalf("incomplete disagrees: %v vs %v", perr, rerr)
			}
			return
		}
		if !reflect.DeepEqual(parsed, read) {
			t.Fatalf("ParseClientHello %+v, ReadClientHello %+v", parsed, read)
		}
		if again, err := ParseClientHello(raw); err != nil || !reflect.DeepEqual(again, parsed) {
			t.Fatalf("the consumed bytes reparse as %+v, %v", again, err)
		}
		if len(parsed.SNI) > len(data) || (parsed.Legacy && !parsed.HasECH) || !printable([]byte(parsed.SNI)) {
			t.Fatalf("implausible hello %+v", parsed)
		}
	})
}

// TestReadClientHelloStopsAtTheCap: once the records seen leave no room
// for another, ReadClientHello refuses without reading the next header, so
// it never reads or buffers past MaxHelloBytes and never waits on a client
// whose hello can no longer fit.
func TestReadClientHelloStopsAtTheCap(t *testing.T) {
	wire := nearCap(65513)
	if len(wire) != MaxHelloBytes-3 {
		t.Fatalf("nearCap built %d bytes, want %d", len(wire), MaxHelloBytes-3)
	}
	after := record(recordTypeHS, 0x0301, []byte{0})
	cr := &countingReader{r: bytes.NewReader(append(slices.Clone(wire), after...))}
	_, raw, err := ReadClientHello(cr)
	if !errors.Is(err, ErrHelloTooLarge) {
		t.Errorf("ReadClientHello = %v, want ErrHelloTooLarge", err)
	}
	if cr.n != len(wire) || !bytes.Equal(raw, wire) {
		t.Errorf("ReadClientHello read %d bytes and returned %d, want %d", cr.n, len(raw), len(wire))
	}
	if cap(raw) > MaxHelloBytes {
		t.Errorf("ReadClientHello buffered cap %d, over the cap", cap(raw))
	}
}
