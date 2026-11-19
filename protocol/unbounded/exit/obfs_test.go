package exit

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSalamanderPasswordKnownVectors(t *testing.T) {
	// hex(SHA-256("lantern-box unbounded exit salamander v1" || password)), from shasum -a 256.
	assert.Equal(t, "bc25ec9b91fa1685e3a1af239aa8a0008a144e11b4525a5ccd305a384706a82f",
		SalamanderPassword("correct horse battery staple"))
	assert.Equal(t, "4b8566e01d8c3e10f7c7f931300ab3b21e682156055b7ee4331805f62e63d20a",
		SalamanderPassword(""))
}

func TestSalamanderPasswordIsNotTheSessionPassword(t *testing.T) {
	assert.NotEqual(t, "pw", SalamanderPassword("pw"))
	assert.NotEqual(t, SalamanderPassword("a"), SalamanderPassword("b"))
}
