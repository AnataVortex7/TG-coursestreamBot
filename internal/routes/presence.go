package routes

import (
	"net/http"

	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/presence"

	"github.com/gin-gonic/gin"
)

// LoadPresence registers the presence ping endpoint.
func (e *allRoutes) LoadPresence(r *Route) {
	r.Engine.POST("/api/presence/ping", auth.RequireAuth, handlePresencePing)
}

// POST /api/presence/ping
// Frontend calls this every 30s while user is on a course page.
func handlePresencePing(c *gin.Context) {
	userID := auth.GetUserID(c)
	presence.MarkActive(userID.Hex())
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
