// internal/routes/tglogin_api.go
//
// Admin Panel — Telegram Userbot Login/Logout API
//
//   GET  /api/admin/tg/status       — userbot chalu aahe ka + account info
//   POST /api/admin/tg/send-code    — phone la OTP pathavto
//   POST /api/admin/tg/verify-code  — OTP + 2FA verify, session save, userbot start
//   POST /api/admin/tg/logout       — userbot stop + session delete

package routes

import (
	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/db"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// LoadTGLoginAPI — reflection-based auto-load (method name "Load" ne suruvat)
func (e *allRoutes) LoadTGLoginAPI(r *Route) {
	api := r.Engine.Group("/api/admin/tg", adminAuthMiddleware())
	api.GET("/status", tgStatus)
	api.POST("/send-code", tgSendCode)
	api.POST("/verify-code", tgVerifyCode)
	api.POST("/logout", tgLogout)
}

// GET /api/admin/tg/status
func tgStatus(c *gin.Context) {
	info := bot.UserBot.Status()
	c.JSON(http.StatusOK, info)
}

// POST /api/admin/tg/send-code
// Body: { "phone": "+919876543210" }
func tgSendCode(c *gin.Context) {
	var req struct {
		Phone string `json:"phone"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Phone) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Phone number required — e.g. +919876543210"})
		return
	}

	phoneHash, err := bot.UserBot.SendCode(strings.TrimSpace(req.Phone))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"ok":         true,
		"phone_hash": phoneHash,
		"message":    "✅ OTP Telegram var pathavla!",
	})
}

// POST /api/admin/tg/verify-code
// Body: { "phone": "+91...", "phone_hash": "...", "code": "12345", "password": "" }
// password field: 2FA saathi — nasel tar empty string pathava
func tgVerifyCode(c *gin.Context) {
	var req struct {
		Phone     string `json:"phone"`
		PhoneHash string `json:"phone_hash"`
		Code      string `json:"code"`
		Password  string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}
	if req.Phone == "" || req.PhoneHash == "" || req.Code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone, phone_hash ani code required aahet"})
		return
	}

	sessionStr, userInfo, err := bot.UserBot.VerifyCode(req.Phone, req.PhoneHash, req.Code, req.Password)
	if err != nil {
		// 2FA required — frontend la signal pathav
		if err.Error() == "SESSION_PASSWORD_NEEDED" {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":     "2FA password required",
				"needs_2fa": true,
			})
			return
		}
		c.JSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
		return
	}

	// MongoDB madhe save karo
	if saveErr := db.SaveUserBotSession(
		sessionStr,
		userInfo.Phone,
		userInfo.FirstName,
		userInfo.LastName,
		userInfo.Username,
		userInfo.UserID,
	); saveErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Session save hovu shakla nahi: " + saveErr.Error()})
		return
	}

	// Live userbot restart
	bot.UserBot.StartFromSession(sessionStr)

	c.JSON(http.StatusOK, gin.H{
		"ok":      true,
		"message": "✅ Telegram Login successful!",
		"user":    userInfo,
	})
}

// POST /api/admin/tg/logout
func tgLogout(c *gin.Context) {
	bot.UserBot.Stop()
	if err := db.DeleteUserBotSession(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Logout error: " + err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "✅ Userbot logout kela!"})
}
