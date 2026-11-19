package exit

import (
	"crypto/sha256"
	"encoding/hex"
)

const salamanderLabel = "lantern-box unbounded exit salamander v1"

// SalamanderPassword is the Salamander obfuscation key for a direct exit session whose
// Hysteria2 password is password: hex(SHA-256(label || password)). Both ends derive it from the
// password they already share, the exit from the one it minted and the consumer from its
// ExitPath, under a label of its own so the key is never the auth password itself.
func SalamanderPassword(password string) string {
	sum := sha256.Sum256([]byte(salamanderLabel + password))
	return hex.EncodeToString(sum[:])
}
