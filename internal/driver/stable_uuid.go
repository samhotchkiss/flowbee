package driver

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// driverGrantUUID is canonical at birth and unique for every authorized
// delivery attempt. Driver's current v2 contract never reuses a grant_id, even when an action is
// retried at a higher epoch, so the epoch is part of the immutable name.
func driverGrantUUID(actionID string, actionEpoch int64) string {
	sum := sha256.Sum256([]byte("flowbee-driver-grant/v1\x00" + actionID + "\x00" + strconv.FormatInt(actionEpoch, 10)))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
