package token

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"time"
)

// ─── Key setup ────────────────────────────────────────────────────────────────

var aesKey []byte

func init() {
	secret := os.Getenv("STREAM_SECRET")
	if secret == "" {
		log.Fatal("FATAL: STREAM_SECRET environment variable is not set. " +
			"Set a strong random secret before running in production.")
	}
	sum := sha256.Sum256([]byte(secret))
	aesKey = sum[:]
}

// ─── Token TTLs ───────────────────────────────────────────────────────────────

const (
	StreamTokenTTL = 2 * time.Hour
	// Worker tokens are short-lived — they carry no session data,
	// just enough to authorize one stream on the worker.
	WorkerTokenTTL = 80 * time.Minute
)

// ─── Main stream token payload ───────────────────────────────────────────────
// Worker च्या verify.go मधील streamPayload शी exactly match केलेलं असणं आवश्यक.

type StreamPayload struct {
	MessageID    int64     `json:"mid"`
	ChannelID    int64     `json:"cid"`
	UserID       string    `json:"uid"`
	SessionToken string    `json:"sid"` // "" for worker tokens
	ExpiresAt    time.Time `json:"exp"`
}

// ─── Worker token payload ─────────────────────────────────────────────────────
// Must match worker's token/verify.go WorkerTokenPayload exactly.
// Main server encrypts this; worker only decrypts + uses it.

type WorkerTokenPayload struct {
	// Identity
	MessageID int64  `json:"mid"`
	ChannelID int64  `json:"cid"`
	UserID    string `json:"uid"`

	// Bot credentials — from main server bot pool
	// Worker connects to Telegram using these directly
	ApiID    int32  `json:"api_id"`
	ApiHash  string `json:"api_hash"`
	BotToken string `json:"bot_token"`

	// File location — pre-fetched by main server
	// Worker does NOT need to re-query Telegram
	FileType      string `json:"ftype"`  // "document" | "photo"
	FileID        int64  `json:"fid"`
	AccessHash    int64  `json:"fah"`
	FileReference []byte `json:"fref"`
	ThumbSize     string `json:"fthumb"` // photo only
	FileSize      int64  `json:"fsz"`
	FileName      string `json:"fname"`
	MimeType      string `json:"fmime"`
	DCID          int    `json:"dc"`

	// Expiry
	ExpiresAt time.Time `json:"exp"`
}

// ─── Main stream token (user → main server) ──────────────────────────────────

// New creates an encrypted stream token for the player page.
// sessionToken = JWT cookie value from auth.GetSessionToken(c).
func New(messageID int, channelID int64, userID string, sessionToken string) (string, error) {
	payload := StreamPayload{
		MessageID:    int64(messageID),
		ChannelID:    channelID,
		UserID:       userID,
		SessionToken: sessionToken,
		ExpiresAt:    time.Now().Add(StreamTokenTTL),
	}
	return encryptStream(payload)
}

// ─── Worker token (main server → worker) ─────────────────────────────────────

// NewWorkerToken creates a short-lived token carrying full file info + bot credentials.
// The worker uses this to stream the file directly without querying Telegram.
//
// Parameters:
//   - messageID, channelID, userID — identity (for logging / audit)
//   - apiID, apiHash, botToken     — Telegram bot credentials (from bot pool)
//   - fileType                     — "document" | "photo"
//   - fileID, accessHash, fileRef  — Telegram file location fields
//   - thumbSize                    — photo thumb type (empty for documents)
//   - fileSize, fileName, mimeType — file metadata
//   - dcID                         — Telegram DC where the file lives
func NewWorkerToken(
	messageID int64, channelID int64, userID string,
	apiID int32, apiHash string, botToken string,
	fileType string,
	fileID int64, accessHash int64, fileRef []byte,
	thumbSize string,
	fileSize int64, fileName string, mimeType string,
	dcID int,
) (string, error) {
	payload := WorkerTokenPayload{
		MessageID:     messageID,
		ChannelID:     channelID,
		UserID:        userID,
		ApiID:         apiID,
		ApiHash:       apiHash,
		BotToken:      botToken,
		FileType:      fileType,
		FileID:        fileID,
		AccessHash:    accessHash,
		FileReference: fileRef,
		ThumbSize:     thumbSize,
		FileSize:      fileSize,
		FileName:      fileName,
		MimeType:      mimeType,
		DCID:          dcID,
		ExpiresAt:     time.Now().Add(WorkerTokenTTL),
	}
	return encryptWorker(payload)
}

// ─── Encrypt ─────────────────────────────────────────────────────────────────

func encryptStream(payload StreamPayload) (string, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return encryptBytes(plaintext)
}

func encryptWorker(payload WorkerTokenPayload) (string, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return encryptBytes(plaintext)
}

func encryptBytes(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

// ─── Verify (main stream token — sessionToken must NOT be empty) ──────────────

var (
	ErrTokenInvalid = errors.New("stream token is invalid")
	ErrTokenExpired = errors.New("stream token has expired")
)

type VerifyResult struct {
	MessageID    int
	ChannelID    int64
	UserID       string
	SessionToken string
}

// Verify decrypts the token and validates expiry.
// The caller is responsible for session / access / nonce checks.
func Verify(tokenStr string) (*VerifyResult, error) {
	payload, err := decryptPayload(tokenStr)
	if err != nil {
		return nil, err
	}

	if time.Now().After(payload.ExpiresAt) {
		return nil, ErrTokenExpired
	}

	return &VerifyResult{
		MessageID:    int(payload.MessageID),
		ChannelID:    payload.ChannelID,
		UserID:       payload.UserID,
		SessionToken: payload.SessionToken,
	}, nil
}

func decryptPayload(tokenStr string) (*StreamPayload, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tokenStr)
	if err != nil {
		return nil, ErrTokenInvalid
	}

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, ErrTokenInvalid
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrTokenInvalid
	}

	nonceSize := gcm.NonceSize()
	if len(raw) < nonceSize {
		return nil, ErrTokenInvalid
	}

	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrTokenInvalid
	}

	var payload StreamPayload
	if err = json.Unmarshal(plaintext, &payload); err != nil {
		return nil, ErrTokenInvalid
	}
	return &payload, nil
}

// ─── IP helper (kept for logging only — not used in auth) ────────────────────

func ClientIP(forwardedFor, remoteAddr string) string {
	if idx := lastIndex(remoteAddr, ":"); idx != -1 {
		return remoteAddr[:idx]
	}
	return remoteAddr
}

func lastIndex(s, substr string) int {
	idx := -1
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			idx = i
		}
	}
	return idx
}
