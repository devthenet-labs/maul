// Copyright 2026 Bitwise Media Group Ltd.
// SPDX-License-Identifier: MIT

// Package prepare is the engine of the agents-namespace tool pod's trusted
// inits (SPEC S2-09; DESIGN 2.4, 4.2), `preview-tools prepare` and
// `preview-tools seed`, which run maul's preview-tools image before any PR
// code does.
//
// prepare fetches the head and base tarballs from source-controller's
// artifact server (Fetcher), verifies each against its digest before a byte
// of it is unpacked, extracts it (Extract) into its pristine trees volume
// and makes the contents readable by every side (Readable), then installs
// tool-exec into the maul-bin volume (Install). seed copies one side's
// pristine tree into that side's work volume under the side's own UID
// (Seed): the only writable copy, the one its sandbox builds.
//
// Path hygiene is the point. Every tree is written through an os.Root, so
// no entry, link or race reaches outside its volume; and a tree is refused
// (ErrRefused), never repaired, when an entry would land outside it (an
// absolute or ".." path, a second top-level directory), when it holds
// anything but directories, regular files and symbolic links, when a link
// points or resolves outside the tree, when an entry is reached through a
// link, when a path appears twice, or when it is larger than its volume.
// A tree is only ever written into an empty directory, never merged.
//
// Standard library only (a test pins that).
package prepare
