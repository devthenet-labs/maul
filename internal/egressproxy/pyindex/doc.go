// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package pyindex rewrites a PEP 503 simple page for the egress proxy's pip
// route (§2.4 of the step-2 spec). It is pure: the standard library,
// golang.org/x/net/html and netprofile's file-path grammar, nothing else (a
// test pins the import set).
//
// A page is never edited in place. It is tokenized and a new page is
// generated, holding only the links that pass every rule: https, no
// userinfo, no query, a host:port that is one of the allowed origins, an
// escaped path netprofile.PyFilePath admits (under the registry's file
// prefix on origin 0), a distribution file of the page's project when the
// grant has an allowlist, a hash fragment kept only in the
// sha256/sha384/sha512 form, and four data- attributes copied escaped. Each
// link becomes /r/<registry>/f/<k><path>, so the output holds no absolute
// URL and pip resolves every file against the proxy. Judge is the file
// route's stateless check of such a path, the same rules in reverse, so
// what a page can emit and what the route accepts are one set.
package pyindex
