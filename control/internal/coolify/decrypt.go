// Package coolify adapts the legacy Coolify database to Fabric's environment model.
package coolify

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

var errCiphertext = errors.New("Coolify encrypted value unavailable")

type decryptor struct {
	keys   [][]byte
	cipher string
}

func newDecryptor(keys []string, algorithm string) (*decryptor, error) {
	algorithm = strings.ToLower(algorithm)
	length := 32
	switch algorithm {
	case "aes-128-cbc", "aes-128-gcm":
		length = 16
	case "aes-256-cbc", "aes-256-gcm":
	default:
		return nil, errCiphertext
	}
	d := &decryptor{cipher: algorithm}
	for _, k := range keys {
		b := []byte(k)
		var err error
		if strings.HasPrefix(k, "base64:") {
			b, err = base64.StdEncoding.DecodeString(strings.TrimPrefix(k, "base64:"))
		}
		if err != nil || len(b) != length {
			return nil, errCiphertext
		}
		d.keys = append(d.keys, b)
	}
	if len(d.keys) == 0 {
		return nil, errCiphertext
	}
	return d, nil
}

// Laravel environment_variables uses encrypt() (serialized PHP string), while
// shared_environment_variables uses the encrypted Eloquent cast (raw string).
func (d *decryptor) decrypt(encoded string, serialized bool) (string, error) {
	if len(encoded) > 12*1024*1024 {
		return "", errCiphertext
	}
	outer, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", errCiphertext
	}
	var payload struct{ IV, Value, MAC, Tag string }
	if json.Unmarshal(outer, &payload) != nil {
		return "", errCiphertext
	}
	iv, e1 := base64.StdEncoding.DecodeString(payload.IV)
	value, e2 := base64.StdEncoding.DecodeString(payload.Value)
	if e1 != nil || e2 != nil {
		return "", errCiphertext
	}
	for _, key := range d.keys {
		block, _ := aes.NewCipher(key)
		var plain []byte
		if strings.HasSuffix(d.cipher, "-gcm") {
			aead, _ := cipher.NewGCM(block)
			tag, e := base64.StdEncoding.DecodeString(payload.Tag)
			if e != nil || len(tag) != aead.Overhead() || len(iv) != aead.NonceSize() {
				continue
			}
			plain, e = aead.Open(nil, iv, append(append([]byte{}, value...), tag...), nil)
			if e != nil {
				continue
			}
		} else {
			mac := hmac.New(sha256.New, key)
			mac.Write([]byte(payload.IV + payload.Value))
			provided, e := hex.DecodeString(payload.MAC)
			if e != nil || !hmac.Equal(mac.Sum(nil), provided) || payload.Tag != "" || len(iv) != aes.BlockSize || len(value) == 0 || len(value)%aes.BlockSize != 0 {
				continue
			}
			plain = make([]byte, len(value))
			cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, value)
			padding := int(plain[len(plain)-1])
			if padding < 1 || padding > aes.BlockSize {
				continue
			}
			valid := true
			for _, b := range plain[len(plain)-padding:] {
				if int(b) != padding {
					valid = false
				}
			}
			if !valid {
				continue
			}
			plain = plain[:len(plain)-padding]
		}
		if !serialized {
			return string(plain), nil
		}
		// Parse strings only. Never invoke a general PHP object deserializer.
		text := string(plain)
		if !strings.HasPrefix(text, "s:") {
			return "", errCiphertext
		}
		end := strings.Index(text[2:], ":") + 2
		if end < 2 {
			return "", errCiphertext
		}
		n, e := strconv.Atoi(text[2:end])
		start := end + 2
		if e != nil || n < 0 || start > len(text) || text[end:end+2] != `:"` || n > len(text)-start || text[start+n:] != `";` {
			return "", errCiphertext
		}
		return text[start : start+n], nil
	}
	return "", errCiphertext
}
