// internal/routes/offline.go
//
// POST /api/offline-token
//   Body: { "course_id": "...", "message_id": 42, "channel_id": -100123456 }
//   Returns: { "bot_link": "https://t.me/botname?start=dl_48271", ... }

package routes

import (
	"fmt"
	"net/http"
	"strings"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/db"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func (e *allRoutes) LoadOffline(r *Route) {
	protected := r.Engine.Group("/", auth.RequireAuth)
	protected.POST("/api/offline-token", handleGenerateOfflineToken)
}

func handleGenerateOfflineToken(c *gin.Context) {
	userID := auth.GetUserID(c)

	var req struct {
		CourseID  string `json:"course_id"`
		MessageID int    `json:"message_id"`
		ChannelID int64  `json:"channel_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	courseObjID, err := primitive.ObjectIDFromHex(strings.TrimSpace(req.CourseID))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid course_id"})
		return
	}
	if req.MessageID < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid message_id"})
		return
	}

	hasAccess, err := db.HasAccess(userID, courseObjID)
	if err != nil || !hasAccess {
		c.JSON(http.StatusForbidden, gin.H{"error": "you do not have access to this course"})
		return
	}

	usedToday, err := db.GetDailyUsageCount(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not check daily usage"})
		return
	}
	if usedToday >= 3 {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "daily download limit of 3 reached; resets at midnight IST",
			"used":  usedToday,
			"limit": 3,
		})
		return
	}

	token, err := db.CreateOfflineToken(userID, courseObjID, req.MessageID, req.ChannelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// ── Deep link बनवतो ────────────────────────────────────────────────────────
	// https://t.me/botname?start=dl_48271
	// User click करेल → Telegram उघडेल → /start dl_48271 → direct video येईल
	botLink := fmt.Sprintf("https://t.me/%s?start=dl_%s",
		config.ValueOf.BotUsername, token)

	c.JSON(http.StatusOK, gin.H{
		"bot_link":   botLink,           // ← clickable deep link
		"token":      token,             // (backup साठी)
		"used_today": usedToday,
		"limit":      3,
		"remaining":  3 - usedToday,
	})
}
