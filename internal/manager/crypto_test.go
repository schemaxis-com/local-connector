package manager

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The key is created once and read back at every later start: the saved
// passwords and tokens are readable only under the key they were saved with.
func TestTheKeyIsCreatedOnceAndReused(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "data", ".agent.key")

	first, err := NewCryptoHelper(keyPath)
	if err != nil {
		t.Fatalf("NewCryptoHelper = %v", err)
	}
	sealed, err := first.Encrypt("tiger")
	if err != nil {
		t.Fatalf("Encrypt = %v", err)
	}
	if strings.Contains(sealed, "tiger") {
		t.Fatalf("Encrypt = %q, which carries the plain text", sealed)
	}

	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("the key file was not written: %v", err)
	}
	if info.Size() != 32 {
		t.Fatalf("the key file holds %d bytes, want 32", info.Size())
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the key file's mode is %v, want owner read and write only", info.Mode().Perm())
	}

	restarted, err := NewCryptoHelper(keyPath)
	if err != nil {
		t.Fatalf("NewCryptoHelper on an existing key = %v", err)
	}
	if plain, err := restarted.Decrypt(sealed); err != nil || plain != "tiger" {
		t.Fatalf("Decrypt after a restart = %q, %v, want tiger", plain, err)
	}
}

func TestEachEncryptionUsesAFreshNonce(t *testing.T) {
	c, err := NewCryptoHelper(filepath.Join(t.TempDir(), ".agent.key"))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := c.Encrypt("same")
	b, _ := c.Encrypt("same")
	if a == b {
		t.Fatal("two encryptions of one value are identical, so the nonce is reused")
	}
}

// A value saved under another key, or altered, is refused rather than
// decrypted to something else.
func TestDecryptRefusesAnotherKeyAndAlteredValues(t *testing.T) {
	dir := t.TempDir()
	mine, err := NewCryptoHelper(filepath.Join(dir, "mine.key"))
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := NewCryptoHelper(filepath.Join(dir, "theirs.key"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := theirs.Encrypt("tiger")

	if _, err := mine.Decrypt(sealed); err == nil {
		t.Error("a value sealed under another key was decrypted")
	}
	altered := []byte(sealed)
	altered[len(altered)/2] ^= 0x01
	if _, err := theirs.Decrypt(string(altered)); err == nil {
		t.Error("an altered value was decrypted")
	}
	if _, err := theirs.Decrypt("c2hvcnQ="); err == nil {
		t.Error("a value shorter than a nonce was decrypted")
	}
}

func TestAKeyFileOfTheWrongLengthIsRefused(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), ".agent.key")
	if err := os.WriteFile(keyPath, []byte("too short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCryptoHelper(keyPath); err == nil {
		t.Fatal("a 9-byte key file was accepted")
	}
}

func TestEmptyValuesStayEmpty(t *testing.T) {
	c, err := NewCryptoHelper(filepath.Join(t.TempDir(), ".agent.key"))
	if err != nil {
		t.Fatal(err)
	}
	if sealed, err := c.Encrypt(""); err != nil || sealed != "" {
		t.Fatalf("Encrypt(\"\") = %q, %v", sealed, err)
	}
	if plain, err := c.Decrypt(""); err != nil || plain != "" {
		t.Fatalf("Decrypt(\"\") = %q, %v", plain, err)
	}
}
