package crypto

import (
	"bytes"
	"testing"
)

func TestEncryptDecrypt(t *testing.T) {
	passphrase := "rayma-super-secret-passphrase-123456"
	plaintext := []byte("Hello, this is a secret backup chunk payload representing a critical file.")

	encrypted, err := EncryptChunk(plaintext, passphrase)
	if err != nil {
		t.Fatalf("EncryptChunk failed: %v", err)
	}

	if bytes.Equal(encrypted, plaintext) {
		t.Fatal("Encrypted data is identical to plaintext!")
	}

	decrypted, err := DecryptChunk(encrypted, passphrase)
	if err != nil {
		t.Fatalf("DecryptChunk failed: %v", err)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("Decrypted data mismatch. Got %s, want %s", string(decrypted), string(plaintext))
	}

	// Test incorrect password
	_, err = DecryptChunk(encrypted, "wrong-password")
	if err == nil {
		t.Fatal("Expected error with wrong password, but got nil")
	}

	// Test corrupted header
	corrupted := append([]byte(nil), encrypted...)
	corrupted[0] = 'X'
	_, err = DecryptChunk(corrupted, passphrase)
	if err == nil {
		t.Fatal("Expected error with corrupted header, but got nil")
	}
}
