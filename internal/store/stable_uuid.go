package store

import (
	"crypto/sha256"
	"encoding/hex"
)

// stableUUID derives an RFC 4122 canonical UUID from an immutable domain key.
// Driver's current v2 contract validates identity syntax before route projection, so grant IDs
// must be born canonical in Flowbee's durable domain state—not translated by an
// adapter. Version 5 bits denote a name-derived identifier; SHA-256 provides the
// name digest and the first 128 bits carry the UUID value.
func stableUUID(domain, value string) string {
	sum := sha256.Sum256([]byte(domain + "\x00" + value))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
