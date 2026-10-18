package usagestats

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// UnattributedKeyID is the key_id for usage recorded without a client API key.
const UnattributedKeyID = "unattributed"

const (
	keyIDPrefix    = "k_"
	keyIDHexLength = 12
	maskEllipsis   = "…"
)

// keyIDForRaw returns the stable, non-reversible identifier of a client API key.
// Raw keys are never persisted or returned; only this id, the masked form and
// the key metadata are.
func keyIDForRaw(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return UnattributedKeyID
	}
	sum := sha256.Sum256([]byte(raw))
	return keyIDPrefix + hex.EncodeToString(sum[:])[:keyIDHexLength]
}

// maskKey returns the display form of a client API key: its first and last four
// characters, or only its first two characters when the key has ten characters
// or fewer. Blank keys yield an empty string.
func maskKey(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	runes := []rune(raw)
	if len(runes) <= 10 {
		return string(runes[:min(2, len(runes))]) + maskEllipsis
	}
	return string(runes[:4]) + maskEllipsis + string(runes[len(runes)-4:])
}
