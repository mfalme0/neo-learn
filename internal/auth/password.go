// Package auth implements password hashing and opaque session management.
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

// ErrInvalidHash is returned when a stored hash cannot be parsed.
var ErrInvalidHash = errors.New("auth: invalid password hash")

// HashParams are the argon2id cost parameters for a single hash.
type HashParams struct {
	Time    uint32
	Memory  uint32 // KiB
	Threads uint8
	// SaltLen and KeyLen are fixed. Varying them per-hash would complicate
	// verification for no security gain.
	SaltLen int
	KeyLen  int
}

// DefaultHashParams matches the argon2id recommendations for interactive
// logins. The 64 MiB memory cost is the main defense against GPU cracking.
func DefaultHashParams(time uint32, memory uint32, threads uint8) HashParams {
	return HashParams{
		Time:    time,
		Memory:  memory,
		Threads: threads,
		SaltLen: 16,
		KeyLen:  32,
	}
}

// Hash derives an argon2id hash and returns it in the standard encoded form:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>
//
// Parameters are embedded so raising the cost later does not invalidate
// existing credentials: verification uses the stored values, not current
// defaults.
func (p HashParams) Hash(password string) (string, error) {
	if p.Time == 0 || p.Memory < 8*1024 || p.Threads == 0 {
		return "", fmt.Errorf("auth: invalid hash params %+v", p)
	}

	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, uint32(p.KeyLen))

	return fmt.Sprintf(
			"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
			argon2.Version,
			p.Memory, p.Time, p.Threads,
			base64.RawStdEncoding.EncodeToString(salt),
			base64.RawStdEncoding.EncodeToString(key),
		),
		nil
}

// Verify checks a password against an encoded hash.
//
// It returns a boolean rather than an error for a wrong password: that is the
// common case, not an exceptional one. A malformed hash is a genuine error.
func (p HashParams) Verify(password, encoded string) (bool, error) {
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}

	got := argon2.IDKey([]byte(password), salt, params.Time, params.Memory, params.Threads, uint32(len(want)))

	// Constant-time compare: a timing side channel here leaks the hash
	// byte-by-byte.
	if subtle.ConstantTimeCompare(got, want) == 1 {
		return true, nil
	}
	return false, nil
}

// NeedsRehash reports whether a stored hash was produced with weaker
// parameters than the current configuration. Callers should transparently
// re-hash on next successful login.
func (p HashParams) NeedsRehash(encoded string) bool {
	params, _, _, err := decodeHash(encoded)
	if err != nil {
		// Unparseable: force a rehash by returning true.
		return true
	}
	return params.Time < p.Time || params.Memory < p.Memory
}

func decodeHash(encoded string) (HashParams, []byte, []byte, error) {
	var params HashParams

	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, key
	if len(parts) != 6 || parts[0] != "" {
		return params, nil, nil, ErrInvalidHash
	}
	if parts[1] != "argon2id" {
		return params, nil, nil, fmt.Errorf("%w: unsupported algorithm %q", ErrInvalidHash, parts[1])
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return params, nil, nil, fmt.Errorf("%w: %v", ErrInvalidHash, err)
	}
	if version != argon2.Version {
		return params, nil, nil, fmt.Errorf("%w: unsupported version %d", ErrInvalidHash, version)
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.Memory, &params.Time, &params.Threads); err != nil {
		return params, nil, nil, fmt.Errorf("%w: %v", ErrInvalidHash, err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return params, nil, nil, fmt.Errorf("%w: salt: %v", ErrInvalidHash, err)
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return params, nil, nil, fmt.Errorf("%w: key: %v", ErrInvalidHash, err)
	}

	params.SaltLen = len(salt)
	params.KeyLen = len(key)
	return params, salt, key, nil
}
