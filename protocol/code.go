package protocol

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"strings"
)

const (
	KDFPBKDF2SHA256  = "pbkdf2-sha256"
	PBKDF2Iterations = 600_000
	HashLen          = 32
	SaltLen          = 16
)

const (
	codeDigits  = "0123456789"
	codeLetters = "ABCDEFGHJKLMNPQRSTUVWXYZ" // no I, O
)

// NewCode returns a code like "4821KQ" (display as "4821 KQ").
func NewCode() string {
	b := make([]byte, 6)
	for i := range 4 {
		b[i] = codeDigits[randIndex(len(codeDigits))]
	}
	for i := 4; i < 6; i++ {
		b[i] = codeLetters[randIndex(len(codeLetters))]
	}
	return string(b)
}

func randIndex(n int) int {
	// rejection sampling over one byte; n is small
	limit := 256 - 256%n
	var buf [1]byte
	for {
		_, _ = rand.Read(buf[:])
		if int(buf[0]) < limit {
			return int(buf[0]) % n
		}
	}
}

// NormaliseCode upper-cases and strips spaces and dashes.
func NormaliseCode(code string) string {
	r := strings.NewReplacer(" ", "", "-", "", "\t", "")
	return strings.ToUpper(r.Replace(strings.TrimSpace(code)))
}

// NewSalt returns SaltLen random bytes.
func NewSalt() []byte {
	b := make([]byte, SaltLen)
	_, _ = rand.Read(b)
	return b
}

// HashCode derives the code hash the relay stores and compares.
func HashCode(code string, salt []byte, iterations int) []byte {
	if iterations <= 0 {
		iterations = PBKDF2Iterations
	}
	out, err := pbkdf2.Key(sha256.New, NormaliseCode(code), salt, iterations, HashLen)
	if err != nil {
		panic(err)
	}
	return out
}

// EqualHash compares two hashes in constant time.
func EqualHash(a, b []byte) bool {
	return len(a) == HashLen && len(b) == HashLen && subtle.ConstantTimeCompare(a, b) == 1
}
