package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// ErrInvalidHash is returned when a stored hash is not a parseable argon2id PHC string.
var ErrInvalidHash = errors.New("auth: invalid password hash")

type argonParams struct {
	memory      uint32 // KiB
	iterations  uint32
	parallelism uint8
	saltLen     uint32
	keyLen      uint32
}

// OWASP Password Storage Cheat Sheet minimum for argon2id.
// https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html
var defaultParams = argonParams{
	memory:      19 * 1024,
	iterations:  2,
	parallelism: 1,
	saltLen:     16,
	keyLen:      32,
}

var b64 = base64.RawStdEncoding

// HashPassword returns an argon2id hash.
func HashPassword(password string) (string, error) {
	p := defaultParams
	salt := make([]byte, p.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.iterations, p.memory, p.parallelism, p.keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memory, p.iterations, p.parallelism,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the encoded argon2id hash.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrInvalidHash
	}

	var p argonParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.iterations, &p.parallelism); err != nil {
		return false, ErrInvalidHash
	}

	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return false, ErrInvalidHash
	}

	other := argon2.IDKey([]byte(password), salt, p.iterations, p.memory, p.parallelism, uint32(len(key)))
	return subtle.ConstantTimeCompare(key, other) == 1, nil
}
