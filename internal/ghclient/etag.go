// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

package ghclient

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// ETagCache keeps GitHub's last answer to each GET a client built
// WithETagCache made, with its entity tag, so the next identical GET is
// asked conditionally (If-None-Match). GitHub answers an unchanged resource
// with a bodiless 304, which costs no rate budget, and the client then
// serves the kept answer as GitHub's 200. Every GET still reaches GitHub, so
// a change is seen at once: the cache only saves the budget an unchanged
// answer would have spent (D-B7: idle intents polled every minute spent an
// installation's whole hourly budget). Bounded in bytes, least recently used
// first out; safe for concurrent use; one cache may serve many clients.
type ETagCache struct {
	maxBytes int

	mu      sync.Mutex
	bytes   int
	order   *list.List // of *etagEntry, most recently used first
	entries map[string]*list.Element
}

// etagEntry is one kept answer. Never mutated once kept.
type etagEntry struct {
	key    string
	etag   string
	header http.Header
	body   []byte
	size   int
}

// etagEntryShare is the largest share of the cache one answer may take:
// a bigger one is passed through and not kept.
const etagEntryShare = 16

// NewETagCache returns a cache holding at most maxBytes of kept answers.
func NewETagCache(maxBytes int) *ETagCache {
	return &ETagCache{maxBytes: maxBytes, order: list.New(), entries: map[string]*list.Element{}}
}

// WithETagCache makes the client's GETs conditional on the answers kept in
// c (ETagCache). A nil c is a no-op.
func WithETagCache(c *ETagCache) Option {
	return func(s *settings) { s.etags = c }
}

// get is the answer kept under key, or nil, marked as just used.
func (c *ETagCache) get(key string) *etagEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil
	}
	c.order.MoveToFront(el)
	return el.Value.(*etagEntry)
}

// put keeps e, replacing what key held, and evicts the least recently used
// answers past the byte bound.
func (c *ETagCache) put(e *etagEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(e.key)
	c.entries[e.key] = c.order.PushFront(e)
	c.bytes += e.size
	for c.bytes > c.maxBytes {
		c.dropLocked(c.order.Back().Value.(*etagEntry).key)
	}
}

// drop forgets the answer kept under key.
func (c *ETagCache) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked(key)
}

func (c *ETagCache) dropLocked(key string) {
	el, ok := c.entries[key]
	if !ok {
		return
	}
	c.order.Remove(el)
	delete(c.entries, key)
	c.bytes -= el.Value.(*etagEntry).size
}

// etagTransport asks each GET conditionally on the answer cache holds for
// it. Requests that are not plain GETs, or that carry their own conditional
// or range header (ListIssues and ListBlockedBy keep their own entity tags),
// pass through untouched.
type etagTransport struct {
	next  http.RoundTripper
	cache *ETagCache
}

// RoundTrip implements http.RoundTripper.
func (t *etagTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || req.Header.Get("If-None-Match") != "" ||
		req.Header.Get("If-Modified-Since") != "" || req.Header.Get("Range") != "" {
		return t.next.RoundTrip(req)
	}
	key := etagKey(req)
	held := t.cache.get(key)
	out := req
	if held != nil {
		out = req.Clone(req.Context())
		out.Header.Set("If-None-Match", held.etag)
	}
	resp, err := t.next.RoundTrip(out)
	if err != nil {
		return resp, err
	}
	switch {
	case resp.StatusCode == http.StatusNotModified && held != nil:
		drain(resp)
		return held.response(req, resp), nil
	case resp.StatusCode == http.StatusOK:
		return t.keep(key, req, resp)
	}
	t.cache.drop(key)
	return resp, nil
}

// keep reads a 200's body, keeps it when it carries an entity tag, fits,
// and stands alone (standsAlone), and returns the response with its body
// intact for the caller.
func (t *etagTransport) keep(key string, req *http.Request, resp *http.Response) (*http.Response, error) {
	tag := resp.Header.Get("ETag")
	if tag == "" {
		t.cache.drop(key)
		return resp, nil
	}
	limit := t.cache.maxBytes / etagEntryShare
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		_ = resp.Body.Close()
		t.cache.drop(key)
		return nil, err
	}
	if len(body) > limit {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
		t.cache.drop(key)
		return resp, nil
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if !standsAlone(req.URL, resp.Header, body) {
		t.cache.drop(key)
		return resp, nil
	}
	header := resp.Header.Clone()
	size := len(key) + len(body)
	for k, vs := range header {
		size += len(k)
		for _, v := range vs {
			size += len(v)
		}
	}
	t.cache.put(&etagEntry{key: key, etag: tag, header: header, body: body, size: size})
	return resp, nil
}

// response is the kept answer as GitHub's 200 to req, carrying the 304's
// fresh headers (its rate figures and entity tag) over the kept ones, but
// for those that describe a body, which the 304 has none of.
func (e *etagEntry) response(req *http.Request, fresh *http.Response) *http.Response {
	h := e.header.Clone()
	for k, vs := range fresh.Header {
		switch k {
		case "Content-Length", "Content-Type", "Content-Encoding", "Transfer-Encoding":
			continue
		}
		h[k] = vs
	}
	return &http.Response{
		Status: "200 OK", StatusCode: http.StatusOK,
		Proto: fresh.Proto, ProtoMajor: fresh.ProtoMajor, ProtoMinor: fresh.ProtoMinor,
		Header: h, Body: io.NopCloser(bytes.NewReader(e.body)), ContentLength: int64(len(e.body)),
		Request: req, TLS: fresh.TLS,
	}
}

// standsAlone reports that a 304 to this answer means the whole answer is
// unchanged, so the kept one may be served for it. GitHub's entity tag
// covers the body only: a page of a listing keeps its tag when entries are
// added past it, and its 304 carries no Link header. So a listing page
// stands alone only when it is not full (an entry added would land on it)
// or names its next page (which is asked, and revalidated, on its own); a
// full last page never does, nor a listing whose page size is unknown. An
// object stands alone: GitHub's wrapped listings (check runs, workflow
// runs and jobs) carry a total_count that moves with any entry added.
func standsAlone(u *url.URL, header http.Header, body []byte) bool {
	b := bytes.TrimSpace(body)
	if len(b) == 0 {
		return false
	}
	switch b[0] {
	case '{':
		return true
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(b, &items); err != nil {
			return false
		}
		if hasNextPage(header) {
			return true
		}
		per, err := strconv.Atoi(u.Query().Get("per_page"))
		return err == nil && len(items) < per
	}
	return false
}

// hasNextPage reports a Link header naming a next page.
func hasNextPage(h http.Header) bool {
	for _, v := range h.Values("Link") {
		for link := range strings.SplitSeq(v, ",") {
			if strings.Contains(link, `rel="next"`) {
				return true
			}
		}
	}
	return false
}

// etagKey keys a GET by its credential (hashed, never kept), media type and
// URL: an answer is only ever served again to the credential it was given
// to.
func etagKey(req *http.Request) string {
	sum := sha256.Sum256([]byte(req.Header.Get("Authorization")))
	return hex.EncodeToString(sum[:]) + "\n" + req.Header.Get("Accept") + "\n" + req.URL.String()
}
