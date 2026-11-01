package coolify

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type encryptionFixture struct {
	Cipher, Key, Plaintext, Ciphertext string
	Serialized                         bool
}

func fixtures(t *testing.T) []encryptionFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/laravel-encryption.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []encryptionFixture
	if err = json.Unmarshal(data, &fixtures); err != nil || len(fixtures) != 8 {
		t.Fatal("invalid Laravel fixture", err)
	}
	return fixtures
}
func TestDecryptActualLaravelFormatsAndPreviousKeys(t *testing.T) {
	for _, f := range fixtures(t) {
		// Decode to determine exact AES key length (base64 padding is significant).
		key, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(f.Key, "base64:"))
		other := "base64:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", len(key))))
		d, err := newDecryptor([]string{other, f.Key}, f.Cipher)
		if err != nil {
			t.Fatal(err)
		}
		got, err := d.decrypt(f.Ciphertext, f.Serialized)
		if err != nil || got != f.Plaintext {
			t.Fatalf("Laravel %s serialized=%v failed: %v", f.Cipher, f.Serialized, err)
		}
	}
}
func TestDecryptRejectsTamperingWrongKeyAndWrongSerialization(t *testing.T) {
	for _, f := range fixtures(t) {
		d, _ := newDecryptor([]string{f.Key}, f.Cipher)
		bytes, _ := base64.StdEncoding.DecodeString(f.Ciphertext)
		var payload map[string]string
		_ = json.Unmarshal(bytes, &payload)
		payload["value"] = "AAAA"
		tampered, _ := json.Marshal(payload)
		for _, text := range []string{"not encrypted", base64.StdEncoding.EncodeToString(tampered)} {
			if got, err := d.decrypt(text, f.Serialized); err == nil || got != "" || strings.Contains(err.Error(), f.Plaintext) {
				t.Fatal("unsafe decryption failure")
			}
		}
		if !f.Serialized {
			if got, err := d.decrypt(f.Ciphertext, true); err == nil || got != "" {
				t.Fatal("raw value accepted as serialized string")
			}
		}
	}
}
