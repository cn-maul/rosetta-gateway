package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
)

var (
	ErrNoMasterKey = errors.New("ROSETTA_GW_MASTER_KEY environment variable not set")
)

func GetMasterKey(envName string) ([]byte, error) {
	if envName == "" {
		envName = "ROSETTA_GW_MASTER_KEY"
	}
	raw := os.Getenv(envName)
	if raw == "" {
		return nil, ErrNoMasterKey
	}
	key := sha256.Sum256([]byte(raw))
	return key[:], nil
}

func Encrypt(plaintext []byte, masterKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

func Decrypt(ciphertext []byte, masterKey []byte) ([]byte, error) {
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, errors.New("ciphertext too short")
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func MaskKey(plainKey string) string {
	if len(plainKey) <= 8 {
		return "****"
	}
	return plainKey[:4] + "..." + plainKey[len(plainKey)-4:]
}

func GenerateKey() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.URLEncoding.EncodeToString(b)
}
