// internal/routes/offline.go

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

	// Access check
	hasAccess, err := db.HasAccess(userID, courseObjID)
	if err != nil || !hasAccess {
		c.JSON(http.StatusForbidden, gin.H{"error": "you do not have access to this course"})
		return
	}

	// ── Course-specific limit fetch ───────────────────────────────────────────
	course, err := db.GetCourseByID(courseObjID)
	if err != nil || course == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "course not found"})
		return
	}
	limit := course.DownloadLimit
	if limit <= 0 {
		limit = 3 // default
	}

	// आज किती downloads झाले (या course साठी)
	usedToday, err := db.GetDailyUsageCount(userID, courseObjID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not check daily usage"})
		return
	}
	if usedToday >= limit {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error": fmt.Sprintf("daily download limit of %d reached for this course; resets at midnight IST", limit),
			"used":  usedToday,
			"limit": limit,
		})
		return
	}

	token, err := db.CreateOfflineToken(userID, courseObjID, req.MessageID, req.ChannelID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Deep link: https://t.me/botname?start=dl_48271
	botLink := fmt.Sprintf("https://t.me/%s?start=dl_%s",
		config.ValueOf.BotUsername, token)

	c.JSON(http.StatusOK, gin.H{
		"bot_link":   botLink,
		"token":      token,
		"used_today": usedToday,
		"limit":      limit,
		"remaining":  limit - usedToday,
	})
}
