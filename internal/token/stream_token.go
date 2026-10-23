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
	"net"
	"os"
	"strings"
	"time"

	// Loads fsb.env before init() below runs (see internal/envinit).
	_ "EverythingSuckz/fsb/internal/envinit"
)

// ─── Key setup ────────────────────────────────────────────────────────────────

var aesKey []byte

func init() {
	secret := os.Getenv("STREAM_SECRET")
	if secret == "" {
		log.Fatal("FATAL: STREAM_SECRET environment variable is not set. " +
			"Set a strong random secret before running in production.")
	}
	// Reject the placeholder from fsb.sample.env and obviously weak values.
	// (A 16 char floor keeps older deployments booting; < 32 only warns.)
	if strings.Contains(strings.ToLower(secret), "change-me") || len(secret) < 16 {
		log.Fatal("FATAL: STREAM_SECRET is a placeholder or too short. " +
			"Generate one with: openssl rand -hex 32 " +
			"(the same value must be configured on every worker).")
	}
	if len(secret) < 32 {
		log.Println("WARNING: STREAM_SECRET is shorter than 32 characters — " +
			"rotate it to `openssl rand -hex 32` (update workers too).")
	}
	sum := sha256.Sum256([]byte(secret))
	aesKey = sum[:]
}

// ─── Domain separation (AAD) ──────────────────────────────────────────────────
// Every token type is AES-256-GCM encrypted with the same key. Without domain
// separation a token minted for one purpose could be replayed for another
// (e.g. an authenticated /watch/ token used on the unauthenticated /stream/
// endpoint, which removes the session / single-device / nonce checks).
// Each type below is therefore bound to its own Additional Authenticated Data:
// a token only decrypts for the purpose it was created for.
//
// NOTE: worker tokens intentionally keep NO AAD — the external worker
// (separate project, token/verify.go) decrypts them with a plain GCM Open.

var (
	aadStream  = []byte("fsb/token/stream/v1")
	aadPublic  = []byte("fsb/token/public/v1")
	aadFileKey = []byte("fsb/token/filekey/v1")
)

// ─── Token TTLs ───────────────────────────────────────────────────────────────

const (
	StreamTokenTTL = 2 * time.Hour
	// Worker token TTL — video duration peksha jast asayla hava.
	// 2-3 hour videos sathi 4 hours safe ahe.
	// Security IP binding + Sec-Fetch ne hote, TTL ne nahi.
	WorkerTokenTTL = 4 * time.Hour
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
	FileType      string `json:"ftype"` // "document" | "photo"
	FileID        int64  `json:"fid"`
	AccessHash    int64  `json:"fah"`
	FileReference []byte `json:"fref"`
	ThumbSize     string `json:"fthumb"` // photo only
	FileSize      int64  `json:"fsz"`
	FileName      string `json:"fname"`
	MimeType      string `json:"fmime"`
	DCID          int    `json:"dc"`

	// Stream identity key — main server generates a unique key per stream.
	// Worker sends this key in every heartbeat so main server can confirm
	// this stream is still the latest active one for this user.
	// If user starts a new stream, main server issues a new key → old key
	// becomes invalid → worker detects it and kills the stream.
	StreamKey string `json:"sk"`

	// ClientIP — request cha IP token madhe embed karto.
	// Worker validate karto: request IP == token IP.
	// URL share kela / different machine varun download → rejected.
	ClientIP string `json:"cip"`

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
//   - streamKey                    — unique key for this stream session (heartbeat validation)
func NewWorkerToken(
	messageID int64, channelID int64, userID string,
	apiID int32, apiHash string, botToken string,
	fileType string,
	fileID int64, accessHash int64, fileRef []byte,
	thumbSize string,
	fileSize int64, fileName string, mimeType string,
	dcID int,
	streamKey string,
	clientIP string,
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
		StreamKey:     streamKey,
		ClientIP:      clientIP,
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
	return sealWithAAD(plaintext, aadStream)
}

// encryptWorker — NO AAD (external worker decrypts with a plain GCM Open).
func encryptWorker(payload WorkerTokenPayload) (string, error) {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return sealWithAAD(plaintext, nil)
}

// sealWithAAD encrypts plaintext with AES-256-GCM, binding it to aad.
func sealWithAAD(plaintext, aad []byte) (string, error) {
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

	ciphertext := gcm.Seal(nonce, nonce, plaintext, aad)
	return base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

// openWithAAD decrypts a token produced by sealWithAAD with the same aad.
// Any failure (bad base64, wrong key, wrong aad, tampering) → ErrTokenInvalid.
func openWithAAD(tokenStr string, aad []byte) ([]byte, error) {
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
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrTokenInvalid
	}
	return plaintext, nil
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
	plaintext, err := openWithAAD(tokenStr, aadStream)
	if err != nil {
		return nil, err
	}

	var payload StreamPayload
	if err = json.Unmarshal(plaintext, &payload); err != nil {
		return nil, ErrTokenInvalid
	}
	return &payload, nil
}

// ─── Public stream token (no auth — anyone with link can stream) ─────────────

const PublicStreamTokenTTL = 3 * time.Hour

type PublicStreamPayload struct {
	MessageID int64     `json:"mid"`
	ExpiresAt time.Time `json:"exp"`
	ChannelID int64     `json:"cid,omitempty"` // 0 = LOG_CHANNEL (default), non-zero = private channel (userbot stream)
}

// NewPublicToken creates a 3-hour encrypted token for open /stream/ links.
// No user identity — anyone who has the URL can stream.
// Streams from LOG_CHANNEL (ChannelID = 0).
func NewPublicToken(messageID int) (string, error) {
	return NewPublicTokenForChannel(messageID, 0)
}

// NewPublicTokenForChannel creates a token for a specific channel (private channels).
// channelID = 0 → LOG_CHANNEL (same as NewPublicToken).
// channelID != 0 → userbot ने tya private channel madhun stream karto.
func NewPublicTokenForChannel(messageID int, channelID int64) (string, error) {
	payload := PublicStreamPayload{
		MessageID: int64(messageID),
		ExpiresAt: time.Now().Add(PublicStreamTokenTTL),
		ChannelID: channelID,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return sealWithAAD(raw, aadPublic)
}

// VerifyPublicToken decrypts and validates a public stream token.
// Returns messageID on success (ChannelID=0 assumed — backward compat).
func VerifyPublicToken(tokenStr string) (int, error) {
	msgID, _, err := VerifyPublicTokenFull(tokenStr)
	return msgID, err
}

// VerifyPublicTokenFull returns (messageID, channelID, error).
// channelID = 0 → LOG_CHANNEL; channelID != 0 → private channel via userbot.
func VerifyPublicTokenFull(tokenStr string) (int, int64, error) {
	plaintext, err := openWithAAD(tokenStr, aadPublic)
	if err != nil {
		return 0, 0, err
	}

	var payload PublicStreamPayload
	if err = json.Unmarshal(plaintext, &payload); err != nil {
		return 0, 0, ErrTokenInvalid
	}
	if time.Now().After(payload.ExpiresAt) {
		return 0, 0, ErrTokenExpired
	}
	return int(payload.MessageID), payload.ChannelID, nil
}

// ─── IP helper (kept for logging only — not used in auth) ────────────────────

// ClientIP returns the host part of remoteAddr (IPv4, IPv6 and host:port safe).
func ClientIP(forwardedFor, remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// ─── File Key (offline download — hides message_id + channel_id) ─────────────
// HTML मध्ये message_id आणि channel_id expose होऊ नये म्हणून
// दोन्ही values encrypt करून एक opaque string बनवतो.
// TTL नाही — page reload झाल्यावर नवीन key generate होतो.

type FileKeyPayload struct {
	MessageID int   `json:"mid"`
	ChannelID int64 `json:"cid"`
}

// NewFileKey encrypts message_id + channel_id into a single opaque string.
func NewFileKey(messageID int, channelID int64) (string, error) {
	payload := FileKeyPayload{
		MessageID: messageID,
		ChannelID: channelID,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return sealWithAAD(raw, aadFileKey)
}

// DecryptFileKey decrypts a file key back to message_id + channel_id.
func DecryptFileKey(fileKey string) (messageID int, channelID int64, err error) {
	plaintext, err := openWithAAD(fileKey, aadFileKey)
	if err != nil {
		return 0, 0, err
	}
	var p FileKeyPayload
	if err = json.Unmarshal(plaintext, &p); err != nil {
		return 0, 0, ErrTokenInvalid
	}
	return p.MessageID, p.ChannelID, nil
}
