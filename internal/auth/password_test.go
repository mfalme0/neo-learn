package auth

import (
	"strings"
	"testing"
)

func TestHashAndVerify(t *testing.T) {
	t.Parallel()

	p := DefaultHashParams(2, 8*1024, 1) // cheap params; this is not a benchmark

	encoded, err := p.Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}

	got, err := p.Verify("correct horse battery staple", encoded)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !got {
		t.Error("Verify() = false for the correct password, want true")
	}

	got, err = p.Verify("wrong password", encoded)
	if err != nil {
		t.Fatalf("Verify(wrong) error = %v", err)
	}
	if got {
		t.Error("Verify() = true for a wrong password, want false")
	}
}

func TestHashIsSalted(t *testing.T) {
	t.Parallel()

	p := DefaultHashParams(2, 8*1024, 1)
	const password = "same input"

	a, err := p.Hash(password)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	b, err := p.Hash(password)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}

	if a == b {
		t.Error("two hashes of the same password are identical; the salt is not random")
	}
}

func TestVerifyRejectsMalformedHash(t *testing.T) {
	t.Parallel()

	p := DefaultHashParams(2, 8*1024, 1)

	for name, encoded := range map[string]string{
		"empty":           "",
		"no leading $":    "argon2id$v=19$m=8192,t=2,p=1$c2FsdA$a2V5",
		"wrong algo":      "$argon2i$v=19$m=8192,t=2,p=1$c2FsdA$a2V5",
		"wrong version":   "$argon2id$v=16$m=8192,t=2,p=1$c2FsdA$a2V5",
		"too few parts":   "$argon2id$v=19$m=8192,t=2,p=1$c2FsdA",
		"bad base64 salt": "$argon2id$v=19$m=8192,t=2,p=1$!!!$a2V5",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := p.Verify("password", encoded); err == nil {
				t.Error("Verify() error = nil for a malformed hash, want an error")
			}
		})
	}
}

// A hash produced under weaker cost parameters must still verify, otherwise
// raising ARGON2_MEMORY would lock out every existing user.
func TestVerifyHonoursStoredParamsNotCurrentOnes(t *testing.T) {
	t.Parallel()

	weak := DefaultHashParams(1, 8*1024, 1)
	encoded, err := weak.Hash("password")
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}

	strong := DefaultHashParams(3, 64*1024, 2)
	got, err := strong.Verify("password", encoded)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if !got {
		t.Error("Verify() = false when the hash used older, weaker params")
	}
	if !strong.NeedsRehash(encoded) {
		t.Error("NeedsRehash() = false for a weak hash under a stronger config")
	}
}

func TestNeedsRehashFalseForCurrentParams(t *testing.T) {
	t.Parallel()

	p := DefaultHashParams(2, 8*1024, 1)
	encoded, err := p.Hash("password")
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	if p.NeedsRehash(encoded) {
		t.Error("NeedsRehash() = true for a hash at current parameters")
	}
}

func TestHashRejectsUnsafeParams(t *testing.T) {
	t.Parallel()

	for name, p := range map[string]HashParams{
		"zero time":    {Time: 0, Memory: 8 * 1024, Threads: 1},
		"low memory":   {Time: 1, Memory: 1, Threads: 1},
		"zero threads": {Time: 1, Memory: 8 * 1024, Threads: 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := p.Hash("password"); err == nil {
				t.Error("Hash() error = nil for unsafe params, want an error")
			}
		})
	}
}

func TestGenerateTokenShape(t *testing.T) {
	t.Parallel()

	raw, hash, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken() error = %v", err)
	}
	if raw == "" || hash == "" {
		t.Fatal("GenerateToken() returned an empty token or hash")
	}
	if strings.ContainsAny(raw, "+/=") {
		t.Errorf("token %q is not URL-safe", raw)
	}
	if hash != HashToken(raw) {
		t.Error("HashToken(raw) does not match the hash returned by GenerateToken")
	}
}

func TestGenerateTokenIsUnique(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, 100)
	for range 100 {
		raw, _, err := GenerateToken()
		if err != nil {
			t.Fatalf("GenerateToken() error = %v", err)
		}
		if seen[raw] {
			t.Fatal("GenerateToken() returned a duplicate token")
		}
		seen[raw] = true
	}
}
