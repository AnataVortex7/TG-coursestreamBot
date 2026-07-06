package auth

// internal/auth/device.go
//
// Device-lock system:
//   - Login var JS kade OS (user-agent varun), screen resolution (min x max),
//     aani timezone pathavla jato as hidden form fields (fp_res, fp_tz).
//   - Server tyanchya + User-Agent varun SHA-256 fingerprint kadhto.
//   - First login → fingerprint DB madhe save.
//   - Next login → same fingerprint → allow; vegla fingerprint → blocked page.
//   - User "5 din madhe delete" request karu shakto; 5 din nantar nava device allow.
//   - Admin /kill <mobile> → turant ClearUserDevice.

import (
	"EverythingSuckz/fsb/internal/db"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ─── OS extraction ────────────────────────────────────────────────────────────

func extractOS(ua string) string {
	switch {
	case strings.Contains(ua, "iPhone") || strings.Contains(ua, "iPad"):
		return "iOS"
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "Windows"):
		return "Windows"
	case strings.Contains(ua, "Mac OS X"):
		return "macOS"
	case strings.Contains(ua, "Linux"):
		return "Linux"
	default:
		return "Unknown"
	}
}

// ─── Fingerprint ──────────────────────────────────────────────────────────────
// resolution — JS kade min(w)xmax(h) format (e.g. "390x844")
// timezone   — Intl.DateTimeFormat().resolvedOptions().timeZone (e.g. "Asia/Kolkata")

func computeFingerprint(userAgent, resolution, timezone string) string {
	os := extractOS(userAgent)
	raw := fmt.Sprintf("%s:%s:%s", os, resolution, timezone)
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

// ─── Short-lived device-change token ─────────────────────────────────────────
// Device blocked झाल्यावर URL madhe pathavayacha signed token.
// 30 min valid — fakt /device-blocked page aani /api/request-device-change
// sathi vaapartaat.

type deviceChangeClaims struct {
	Purpose string `json:"purpose"`
	jwt.RegisteredClaims
}

func generateDeviceChangeToken(userID primitive.ObjectID) (string, error) {
	claims := deviceChangeClaims{
		Purpose: "device_change",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.Hex(),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(30 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString(jwtSecret)
}

// ValidateDeviceChangeToken — routes package madhe /api/request-device-change
// handler madhe vaapartaat (exported).
func ValidateDeviceChangeToken(tokenStr string) (primitive.ObjectID, bool) {
	t, err := jwt.ParseWithClaims(tokenStr, &deviceChangeClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return jwtSecret, nil
	})
	if err != nil || !t.Valid {
		return primitive.NilObjectID, false
	}
	c, ok := t.Claims.(*deviceChangeClaims)
	if !ok || c.Purpose != "device_change" {
		return primitive.NilObjectID, false
	}
	id, err := primitive.ObjectIDFromHex(c.Subject)
	if err != nil {
		return primitive.NilObjectID, false
	}
	return id, true
}

// CheckAndSetFingerprint — credentials validated zhalyavar, JWT issue
// karnyaapurvi call kartat. Returns true jar user blocked asel (response
// already sent).
// resolution, timezone — c.PostForm("fp_res"), c.PostForm("fp_tz") varun yeto.
// isAjax — true jar caller fetch()/XHR varun call hot असेल (JSON response
// dyaycha, redirect nahi — fetch() Accept header automatically set karat
// nahi, mhanun header-sniffing ऐवजी explicit param vaaparla, nahitar fetch()
// 302 follow karto ani HTML page la JSON manun parse karayla jato → "Network
// error"). DoLogin (legacy <form> POST) sathi false, VerifyOTPHandler ani
// DoPasswordLogin (donhi fetch() based) sathi true.
func CheckAndSetFingerprint(c *gin.Context, user *db.User, resolution, timezone string, isAjax bool) bool {
	fp := computeFingerprint(c.GetHeader("User-Agent"), resolution, timezone)

	// First login — save fingerprint, proceed
	if user.DeviceFingerprint == "" {
		_ = db.SetUserFingerprint(user.ID, fp)
		return false
	}

	// Same device — proceed
	if user.DeviceFingerprint == fp {
		return false
	}

	// Different device — check if 5-day delete window passed
	if user.SessionDeleteAt != nil && time.Now().After(*user.SessionDeleteAt) {
		_ = db.SetUserFingerprint(user.ID, fp)
		return false // allow new device
	}

	// ── Blocked ────────────────────────────────────────────────────────────────
	tok, err := generateDeviceChangeToken(user.ID)
	if err != nil {
		// Rare: if token generation fails, just block without redirect token
		tok = ""
	}

	blockURL := "/device-blocked?t=" + url.QueryEscape(tok)
	if user.SessionDeleteAt != nil {
		blockURL += "&pending=1&until=" + url.QueryEscape(
			user.SessionDeleteAt.In(ist).Format("02 Jan 2006, 3:04 PM IST"),
		)
	}

	if isAjax {
		c.JSON(403, gin.H{"blocked": true, "redirect": blockURL})
	} else {
		c.Redirect(302, blockURL)
	}
	return true
}