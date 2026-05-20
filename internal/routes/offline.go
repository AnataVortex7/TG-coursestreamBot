// internal/routes/offline.go
//
// POST /api/offline-token
//   Body: { "course_id": "...", "message_id": 42, "channel_id": -100123456 }
//   Returns: { "token": "48271", "expires_at": "...", "used": 2, "limit": 5 }
//
// Called by the "📥 Offline" button on the course detail page (JS fetch).

package routes

import (
	"net/http"
	"strings"

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

	// Access check — user must have course access
	hasAccess, err := db.HasAccess(userID, courseObjID)
	if err != nil || !hasAccess {
		c.JSON(http.StatusForbidden, gin.H{"error": "you do not have access to this course"})
		return
	}

	// Check today's usage before generating
	usedToday, err := db.GetDailyUsageCount(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not check daily usage"})
		return
	}
	if usedToday >= 5 {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": "daily download limit of 5 reached; resets at midnight IST",
			"used":  usedToday,
			"limit": 5,
		})
		return
	}

	// Generate token
	token, err := db.CreateOfflineToken(userID, courseObjID, req.MessageID, req.ChannelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"used_today": usedToday,     // before this token (so user knows remaining)
		"limit":      5,
		"remaining":  5 - usedToday, // remaining AFTER this token will be used
		"note":       "Send /dl " + token + " to the bot in any Telegram chat",
	})
}
