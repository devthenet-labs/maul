// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package pyindex

import (
	"bytes"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"

	xhtml "golang.org/x/net/html"

	"github.com/bitwise-media-group/patchy/internal/netprofile"
)

// MaxPageBytes is the largest upstream page Rewrite takes.
const MaxPageBytes = 16 << 20

// maxAttrBytes bounds a copied data- attribute's value.
const maxAttrBytes = 256

// ErrPageTooLarge is a page over MaxPageBytes.
var ErrPageTooLarge = errors.New("pyindex: page over the size cap")

// ErrBadHref is a file-route path no page could have emitted.
var ErrBadHref = errors.New("pyindex: not a file path of this route")

// copiedAttrs are the only attributes a link keeps (PEP 503, 592, 658,
// 714), in the order they are emitted.
var copiedAttrs = []string{"data-requires-python", "data-core-metadata", "data-dist-info-metadata", "data-yanked"}

// hashFragment is the only fragment a link keeps.
var hashFragment = regexp.MustCompile(`^(sha256|sha384|sha512)=[0-9a-f]{64,128}$`)

// Origin is a host the page may link files to. Origin 0 is the registry's
// own (the credential is sent there): its files must sit under Prefix.
type Origin struct {
	// Host is the URL host, lowercase, with a port only when it is not
	// 443.
	Host string
	// Prefix is the decoded path a file must be at or below on a segment
	// boundary; "" admits any path.
	Prefix string
}

// Stats counts a rewrite's links.
type Stats struct {
	Rewritten, Dropped int
}

// canonicalHost is u's host, lowercase, without a default https port.
func canonicalHost(u *url.URL) string {
	h := strings.ToLower(u.Host)
	return strings.TrimSuffix(h, ":443")
}

// Rewrite generates a new simple page for project from the upstream page
// fetched at pageURL. Every link is resolved against the page (or its
// <base href>) and kept only if it passes the rules of the package doc;
// allow, when not nil, must admit the file's project. Kept links point at
// routeBase/f/<origin index><escaped path>.
func Rewrite(page []byte, pageURL *url.URL, origins []Origin, routeBase, project string,
	allow func(project string) bool,
) ([]byte, Stats, error) {
	var st Stats
	if len(page) > MaxPageBytes {
		return nil, st, ErrPageTooLarge
	}
	var out bytes.Buffer
	p := html.EscapeString(project)
	out.WriteString("<!DOCTYPE html>\n<html><head><meta name=\"pypi:repository-version\" content=\"1.0\">" +
		"<title>Links for " + p + "</title></head>\n<body><h1>Links for " + p + "</h1>\n")
	base := pageURL
	sawBase := false
	z := xhtml.NewTokenizer(bytes.NewReader(page))
	z.SetMaxBuf(MaxPageBytes)
	for {
		tt := z.Next()
		if tt == xhtml.ErrorToken {
			if err := z.Err(); err != io.EOF {
				return nil, st, fmt.Errorf("pyindex: %w", err)
			}
			break
		}
		if tt != xhtml.StartTagToken && tt != xhtml.SelfClosingTagToken {
			continue
		}
		tok := z.Token()
		switch tok.Data {
		case "base":
			if href, ok := attr(tok, "href"); ok && !sawBase {
				sawBase = true
				if b, err := url.Parse(strings.TrimSpace(href)); err == nil {
					base = pageURL.ResolveReference(b)
				}
			}
		case "a":
			href, ok := attr(tok, "href")
			if !ok {
				continue
			}
			line, ok := link(tok, href, base, origins, routeBase, allow)
			if !ok {
				st.Dropped++
				continue
			}
			st.Rewritten++
			out.WriteString(line)
		}
	}
	out.WriteString("</body></html>\n")
	return out.Bytes(), st, nil
}

// attr is the tag's first attribute called key.
func attr(tok xhtml.Token, key string) (string, bool) {
	for _, a := range tok.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// link is the generated anchor for one upstream link, or false to drop it.
func link(tok xhtml.Token, href string, base *url.URL, origins []Origin, routeBase string,
	allow func(string) bool,
) (string, bool) {
	ref, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return "", false
	}
	u := base.ResolveReference(ref)
	if u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Opaque != "" {
		return "", false
	}
	k := -1
	for i, o := range origins {
		if canonicalHost(u) == o.Host {
			k = i
			break
		}
	}
	if k < 0 {
		return "", false
	}
	raw := u.EscapedPath()
	f, _, err := Judge(k, raw, origins)
	if err != nil {
		return "", false
	}
	if allow != nil && !allow(f.Dist.Project) {
		return "", false
	}
	out := routeBase + "/f/" + strconv.Itoa(k) + raw
	if hashFragment.MatchString(u.Fragment) {
		out += "#" + u.Fragment
	}
	var b strings.Builder
	b.WriteString(`<a href="` + html.EscapeString(out) + `"`)
	for _, key := range copiedAttrs {
		if v, ok := attr(tok, key); ok && len(v) <= maxAttrBytes {
			b.WriteString(" " + key + `="` + html.EscapeString(v) + `"`)
		}
	}
	b.WriteString(">" + html.EscapeString(path.Base(f.Decoded)) + "</a><br>\n")
	return b.String(), true
}

// Judge is the file route's check of origin k and the escaped path below
// it: a known origin, a path netprofile.PyFilePath admits, under the
// origin's prefix. It returns the parsed file and the upstream URL, whose
// RawPath is raw exactly.
func Judge(k int, raw string, origins []Origin) (netprofile.PyFile, *url.URL, error) {
	if k < 0 || k >= len(origins) {
		return netprofile.PyFile{}, nil, fmt.Errorf("%w: no origin %d", ErrBadHref, k)
	}
	f, err := netprofile.PyFilePath(raw)
	if err != nil {
		return netprofile.PyFile{}, nil, err
	}
	o := origins[k]
	if o.Prefix != "" && !netprofile.PathUnder(f.Decoded, o.Prefix) {
		return netprofile.PyFile{}, nil, fmt.Errorf("%w: %s is not under %s", ErrBadHref, f.Decoded, o.Prefix)
	}
	u := &url.URL{Scheme: "https", Host: o.Host, Path: f.Decoded, RawPath: raw}
	if u.EscapedPath() != raw {
		return netprofile.PyFile{}, nil, fmt.Errorf("%w: %s does not leave as written", ErrBadHref, raw)
	}
	return f, u, nil
}

// ParseFileRoute splits a file route's escaped path below routeBase,
// "/f/<k><path>", into the origin index and the escaped path. k is decimal
// with no leading zero.
func ParseFileRoute(escaped string) (int, string, error) {
	rest, ok := strings.CutPrefix(escaped, "/f/")
	if !ok {
		return 0, "", ErrBadHref
	}
	ks, p, ok := strings.Cut(rest, "/")
	if !ok || ks == "" || len(ks) > 2 || (len(ks) > 1 && ks[0] == '0') {
		return 0, "", ErrBadHref
	}
	k := 0
	for i := 0; i < len(ks); i++ {
		if ks[i] < '0' || ks[i] > '9' {
			return 0, "", ErrBadHref
		}
		k = k*10 + int(ks[i]-'0')
	}
	return k, "/" + p, nil
}
