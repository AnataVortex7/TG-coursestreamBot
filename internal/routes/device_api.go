// internal/routes/device_api.go
//
// User ekach device varun login karu shakto (fingerprint-based, internal/auth/device.go
// madhe). Vegla device detect zhala tar /device-blocked page var pathavla jato.
// Tya page var 2 options: Admin la Telegram/email notify kara, kiwa "5 divsat
// session delete kara" request submit kara (5 din nantar nava device automatically allow).
package routes

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/email"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

func (e *allRoutes) LoadDeviceAPI(r *Route) {
	r.Engine.GET("/device-blocked", showDeviceBlocked)
	r.Engine.POST("/api/device/request-delete", handleRequestDeviceDelete)
	r.Engine.POST("/api/device/notify-admin", handleNotifyAdminDeviceIssue)
}

// GET /device-blocked?t=<token>&pending=1&until=...
func showDeviceBlocked(c *gin.Context) {
	tok := c.Query("t")
	pending := c.Query("pending") == "1"
	until := c.Query("until")

	_, valid := auth.ValidateDeviceChangeToken(tok)
	if !valid {
		c.HTML(http.StatusOK, "device_blocked.html", gin.H{
			"validToken": false,
		})
		return
	}

	c.HTML(http.StatusOK, "device_blocked.html", gin.H{
		"validToken": true,
		"token":      tok,
		"pending":    pending,
		"until":      until,
	})
}

// POST /api/device/request-delete  Body: { "token": "..." }
// User cha pending 5-day delete window suru karto.
func handleRequestDeviceDelete(c *gin.Context) {
	var req struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	userID, valid := auth.ValidateDeviceChangeToken(req.Token)
	if !valid {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Link expired. Please try logging in again."})
		return
	}

	user, err := db.GetUserByID(userID)
	if err != nil || user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	// Already pending — same window dakhav, naveen suru karu nako
	if user.SessionDeleteAt != nil && time.Now().Before(*user.SessionDeleteAt) {
		c.JSON(http.StatusOK, gin.H{
			"ok":    true,
			"until": user.SessionDeleteAt.Format(time.RFC3339),
		})
		return
	}

	deleteAt := time.Now().Add(5 * 24 * time.Hour)
	if err := db.RequestSessionDelete(userID, deleteAt); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Server error. Try again."})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true, "until": deleteAt.Format(time.RFC3339)})
}

// POST /api/device/notify-admin  Body: { "token": "..." }
// Allowed admins la Telegram DM + email — "ya user cha device-login issue aahe".
func handleNotifyAdminDeviceIssue(c *gin.Context) {
	var req struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Token == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	userID, valid := auth.ValidateDeviceChangeToken(req.Token)
	if !valid {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Link expired. Please try logging in again."})
		return
	}

	user, err := db.GetUserByID(userID)
	if err != nil || user == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "User not found"})
		return
	}

	if !allowOnce("devnotify:"+userID.Hex(), 5*time.Minute) {
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "Admin la already notify kela ahe. Thoda vel wait kara."})
		return
	}

	msg := fmt.Sprintf(
		"🔒 Device Login Issue\n\nUser: %s\nMobile: %s\nEmail: %s\n\nYa user cha account doosrya device var aadhich login aahe ani navin device varun login karaycha try kartoy. Fresh login allow karaycha asel tar:\n/kill %s",
		user.FullName, user.Mobile, user.Email, user.Mobile,
	)

	go bot.NotifyAllowedUsers(msg)

	go func() {
		if config.ValueOf.AdminEmail == "" {
			return
		}
		_ = email.SendUserContactToAdmin(config.ValueOf.AdminEmail, email.UserContactInfo{
			UserFullName: user.FullName,
			UserMobile:   user.Mobile,
			UserEmail:    user.Email,
			Username:     user.Username,
			Subject:      "Device Login Issue",
			Message:      "User navin device varun login karaycha try kartoy pan account aadhich dusrya device var active aahe. /kill " + user.Mobile + " vapra fresh login sathi.",
		})
	}()

	c.JSON(http.StatusOK, gin.H{"ok": true})
}