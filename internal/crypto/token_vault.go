package crypto

import (
	"encoding/base64"
	"errors"
	"os"

	"github.com/arpitmandhotra/api-integrator/internal/security"
)

// getEncryptionKey decodes a base64-encoded 32-byte AES-256 key from the environment.
func getEncryptionKey() ([]byte, error) {
	keyStr := os.Getenv("TOKEN_ENCRYPTION_KEY")
	if keyStr == "" {
		return nil, errors.New("TOKEN_ENCRYPTION_KEY environment variable is not set")
	}
	key, err := base64.StdEncoding.DecodeString(keyStr)
	if err != nil {
		return nil, errors.New("TOKEN_ENCRYPTION_KEY must be a valid base64-encoded string")
	}
	if len(key) != 32 {
		return nil, errors.New("TOKEN_ENCRYPTION_KEY must decode to exactly 32 bytes for AES-256")
	}
	return key, nil
}

// EncryptToken encrypts a plaintext string using AES-256-GCM.
func EncryptToken(plaintext string) (string, error) {
	key, err := getEncryptionKey()
	if err != nil {
		return "", err
	}
	return security.EncryptString(plaintext, key)
}

// DecryptToken decrypts an AES-256-GCM encrypted, base64-encoded ciphertext.
func DecryptToken(ciphertextBase64 string) (string, error) {
	key, err := getEncryptionKey()
	if err != nil {
		return "", err
	}
	return security.DecryptString(ciphertextBase64, key)
}

