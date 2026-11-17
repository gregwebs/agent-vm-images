package codexpin

import (
	"crypto/sha256"
	"encoding/hex"
)

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
