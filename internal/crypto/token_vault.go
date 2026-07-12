package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
)

// getEncryptionKey decodes a base64-encoded 32-byte AES-256 key from the environment.
//
// M19 FIX: The key MUST be a base64-encoded random 32-byte value, not a raw
// UTF-8 string. ASCII-printable passphrases have < 256-bit entropy (max ~210 bits
// for a 32-char ASCII string), defeating the security of AES-256.
//
// To generate a valid key:  openssl rand -base64 32
func getEncryptionKey() ([]byte, error) {
	keyStr := os.Getenv("TOKEN_ENCRYPTION_KEY")
	if keyStr == "" {
		return nil, errors.New("TOKEN_ENCRYPTION_KEY environment variable is not set")
	}
	key, err := base64.StdEncoding.DecodeString(keyStr)
	if err != nil {
		return nil, errors.New("TOKEN_ENCRYPTION_KEY must be a valid base64-encoded string (generate with: openssl rand -base64 32)")
	}
	if len(key) != 32 {
		return nil, errors.New("TOKEN_ENCRYPTION_KEY must decode to exactly 32 bytes for AES-256")
	}
	return key, nil
}

// EncryptToken encrypts a plaintext string using AES-256-GCM.
// Nonce is randomly generated and prepended to the ciphertext.
func EncryptToken(plaintext string) (string, error) {
	key, err := getEncryptionKey()
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// DecryptToken decrypts an AES-256-GCM encrypted, base64-encoded ciphertext.
// Returns an error on tampered or invalid ciphertext instead of panicking.
func DecryptToken(ciphertextBase64 string) (string, error) {
	key, err := getEncryptionKey()
	if err != nil {
		return "", err
	}

	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextBase64)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", errors.New("ciphertext too short")
	}

	nonce, encryptedMessage := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, encryptedMessage, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}
