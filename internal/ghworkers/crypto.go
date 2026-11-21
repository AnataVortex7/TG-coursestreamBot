package ghworkers

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"

	"EverythingSuckz/fsb/config"
)

// Token encryption key: GH_TOKEN_KEY (असेल तर) नाहीतर COLAB_SECRET_KEY + BOT_TOKEN वरून derive.
// टीप: key बदलला (किंवा BOT_TOKEN बदलला) तर साठवलेले tokens decrypt होणार नाहीत — पुन्हा add करा.
func deriveKey() []byte {
	seed := os.Getenv("GH_TOKEN_KEY")
	if seed == "" {
		seed = os.Getenv("COLAB_SECRET_KEY") + "|" + config.ValueOf.BotToken
	}
	h := sha256.Sum256([]byte("fsb-ghworkers|" + seed))
	return h[:]
}

func encryptToken(plain string) (string, error) {
	block, err := aes.NewCipher(deriveKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, []byte(plain), nil)), nil
}

func decryptToken(enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(deriveKey())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	pt, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
