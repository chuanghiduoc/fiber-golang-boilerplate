package core

import (
	"os"
	"strings"
	"testing"
)

func TestPasswordHashUsesArgon2idPHC(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple 2026")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=1024,t=1,p=1$") {
		t.Fatalf("hash = %q, want an argon2id PHC string with the test parameters", hash)
	}
	ok, needsRehash := VerifyPassword(hash, "correct horse battery staple 2026")
	if !ok {
		t.Fatal("the correct password must verify")
	}
	if needsRehash {
		t.Fatal("a hash written with the current parameters must not need a rehash")
	}
	if _, needsRehash := VerifyPassword(hash, "wrong password 2026"); needsRehash {
		t.Fatal("a failed verification must not request a rehash")
	}
}

func TestPasswordHashProductionParameters(t *testing.T) {
	if argon2MemoryKiB != 19456 || argon2Iterations != 2 || argon2Parallelism != 1 {
		t.Fatalf("production parameters = m=%d,t=%d,p=%d, want OWASP 19456/2/1",
			argon2MemoryKiB, argon2Iterations, argon2Parallelism)
	}
}

func TestPasswordHashVerifiesHashesFromOlderParameters(t *testing.T) {
	old := argon2Params{memory: 2048, iterations: 3, parallelism: 2}
	hash := encodeArgon2id([]byte("correct horse battery staple 2026"), []byte("0123456789abcdef"), old)
	ok, needsRehash := VerifyPassword(hash, "correct horse battery staple 2026")
	if !ok {
		t.Fatal("a hash from older parameters must still verify")
	}
	if !needsRehash {
		t.Fatal("a hash from older parameters must ask for a rehash")
	}
}

func TestPasswordHashHasNoByteCeiling(t *testing.T) {
	password := strings.Repeat("pässwörd-", 10) // more than 100 bytes
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if ok, _ := VerifyPassword(hash, password); !ok {
		t.Fatal("a password longer than 72 bytes must verify")
	}
}

func TestPasswordHashNormalizesUnicode(t *testing.T) {
	hash, err := HashPassword("pässwörd-café-2026")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	// NFKC folds the decomposed spelling to the composed one.
	if ok, _ := VerifyPassword(hash, "pa\u0308sswo\u0308rd-cafe\u0301-2026"); !ok {
		t.Fatal("an NFKC-equivalent spelling must verify")
	}
}

func TestPasswordHashUsesAFreshSalt(t *testing.T) {
	first, err := HashPassword("correct horse battery staple 2026")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	second, err := HashPassword("correct horse battery staple 2026")
	if err != nil {
		t.Fatalf("hashPassword: %v", err)
	}
	if first == second {
		t.Fatal("two hashes of the same password must differ")
	}
}

func TestDummyHashUsesCurrentParameters(t *testing.T) {
	if !strings.HasPrefix(dummyPasswordHash, "$argon2id$") {
		t.Fatalf("dummy hash = %q, want argon2id", dummyPasswordHash)
	}
	params, _, _, err := parseArgon2id(dummyPasswordHash)
	if err != nil {
		t.Fatalf("parse dummy hash: %v", err)
	}
	if params != passwordHashParams {
		t.Fatalf("dummy parameters = %+v, want %+v", params, passwordHashParams)
	}
}

func TestMain(m *testing.M) {
	passwordHashParams = testPasswordHashParams
	dummyPasswordHash, _ = HashPassword(strings.Join([]string{"timing", "only", "dummy"}, "-"))
	os.Exit(m.Run())
}
