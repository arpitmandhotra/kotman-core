package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
)

// EncryptString encrypts a plaintext string using AES-256-GCM.
// The randomly generated nonce is prepended to the ciphertext before base64 encoding.
func EncryptString(plaintext string, masterKey []byte) (string, error) {
	if len(masterKey) != 32 {
		return "", errors.New("master key must be exactly 32 bytes for AES-256")
	}

	block, err := aes.NewCipher(masterKey)
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

	// Seal appends the ciphertext to the prefix (nonce), storing both together
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// DecryptString decrypts a base64-encoded AES-256-GCM ciphertext.
// Returns an error if the key is invalid or if the ciphertext has been tampered with.
func DecryptString(ciphertextBase64 string, masterKey []byte) (string, error) {
	if len(masterKey) != 32 {
		return "", errors.New("master key must be exactly 32 bytes for AES-256")
	}

	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextBase64)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", errors.New("ciphertext is too short")
	}

	nonce, encryptedMsg := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, encryptedMsg, nil)
	if err != nil {
		return "", err
	}

	return string(plaintext), nil
}

// ValidateHMAC validates a message body signature using HMAC-SHA256.
// Uses subtle.ConstantTimeCompare to eliminate timing attacks.
func ValidateHMAC(body []byte, expectedHmac string, secret []byte) bool {
	// Try standard base64 decoding first
	expectedBytes, err := base64.StdEncoding.DecodeString(expectedHmac)
	if err != nil {
		// Fallback to hex decoding if base64 fails (standard for many webhooks like Shiprocket/WooCommerce)
		expectedBytes, err = hex.DecodeString(expectedHmac)
		if err != nil {
			return false
		}
	}

	if len(expectedBytes) == 0 {
		return false
	}

	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	computedBytes := mac.Sum(nil)

	return subtle.ConstantTimeCompare(computedBytes, expectedBytes) == 1
}
