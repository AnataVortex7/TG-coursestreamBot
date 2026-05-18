// internal/routes/contact.go
// POST /api/contact — logged-in user admin la direct message pathavto
// Message admin chya ADMIN_EMAIL address var jato.
package routes

import (
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/email"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// ─── Register contact route ───────────────────────────────────────────────────

func (e *allRoutes) LoadContact(r *Route) {
	r.Engine.POST("/api/contact", auth.RequireAuth, handleUserContact)
}

// ─── Handler ──────────────────────────────────────────────────────────────────

func handleUserContact(c *gin.Context) {
	var req struct {
		Subject string `json:"subject"`
		Message string `json:"message"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	req.Subject = strings.TrimSpace(req.Subject)
	req.Message = strings.TrimSpace(req.Message)

	if req.Subject == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Subject bharave lagel"})
		return
	}
	if req.Message == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Message bharave lagel"})
		return
	}
	if len(req.Message) > 2000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Message khup motha ahe (max 2000 characters)"})
		return
	}

	// Logged-in user info
	userID := auth.GetUserID(c)
	user, err := db.GetUserByID(userID)
	if err != nil || user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "User not found"})
		return
	}

	// Admin email — env madhuun gheto
	adminEmail := os.Getenv("ADMIN_EMAIL")
	if adminEmail == "" {
		adminEmail = "meratakshay22@gmail.com" // default fallback
	}

	info := email.UserContactInfo{
		UserFullName: user.FullName,
		UserMobile:   user.Mobile,
		UserEmail:    user.Email,
		Username:     user.Username,
		Subject:      req.Subject,
		Message:      req.Message,
	}

	if err := email.SendUserContactToAdmin(adminEmail, info); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Message pathavta ala nahi. Thoda vel wait karin punha try kara.",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true, "message": "Tumcha message admin la pathavla gela!"})
}
