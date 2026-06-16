// Package spec holds the spec-flow content-addressing (DESIGN §11): the BLAKE3
// spec_content_hash of a committed spec.md, plus the spec sign-off record the
// gate mints. The hash is the spec-flow analogue of the build flow's
// (head_sha, base_sha) binding — any edit to the spec changes the hash and
// SUPERSEDES the prior sign-off (§11.5).
//
// This package is NOT part of the deterministic core (archcheck does not cover
// it). The hash is computed by Flowbee when it commits the author's spec_doc —
// never by the worker (the author must not self-address its own artifact, §11.1)
// — and the RESOLVED hash is passed into the pure engine as a value.
package spec

import (
	"encoding/hex"

	"github.com/zeebo/blake3"
)

// ContentHash is the BLAKE3 tree-hash of the spec.md bytes, prefixed "blake3:"
// (§11.5). PURE and deterministic: same bytes -> same hash, always. It is what a
// spec sign-off binds to; a single byte change yields a different hash and voids
// the sign-off (§11.5 supersession).
func ContentHash(specMD []byte) string {
	sum := blake3.Sum256(specMD)
	return "blake3:" + hex.EncodeToString(sum[:])
}
