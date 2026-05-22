// internal/routes/purchase.go
// POST /api/purchase — user gift card submit karto, admin la email pathavto
package routes

import (
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/email"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ─── Register purchase route ──────────────────────────────────────────────────

func (e *allRoutes) LoadPurchase(r *Route) {
	r.Engine.POST("/api/purchase", auth.RequireAuth, handlePurchase)
}

// ─── Handler ──────────────────────────────────────────────────────────────────

func handlePurchase(c *gin.Context) {
	var req struct {
		CourseID   string `json:"course_id"`
		CardNumber string `json:"card_number"`
		CardPIN    string `json:"card_pin"`
		Message    string `json:"message"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	// Validation
	req.CardNumber = strings.TrimSpace(req.CardNumber)
	req.CardPIN = strings.TrimSpace(req.CardPIN)
	req.Message = strings.TrimSpace(req.Message)

	if req.CourseID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Course ID required"})
		return
	}
	if req.CardNumber == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Gift Card Number required aahe"})
		return
	}
	if req.CardPIN == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Gift Card PIN required aahe"})
		return
	}

	// Course validate karo
	courseID, err := primitive.ObjectIDFromHex(req.CourseID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}
	course, err := db.GetCourseByID(courseID)
	if err != nil || course == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Course not found"})
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
		adminEmail = "" // default
	}

	// Email pathavto
	info := email.PurchaseInfo{
		UserFullName: user.FullName,
		UserMobile:   user.Mobile,
		UserEmail:    user.Email,
		Username:     user.Username,
		CourseTitle:  course.Title,
		CoursePrice:  course.Price,
		CourseID:     course.ID.Hex(),
		CardNumber:   req.CardNumber,
		CardPIN:      req.CardPIN,
		Message:      req.Message,
	}

	if err := email.SendPurchaseNotification(adminEmail, info); err != nil {
		// Email failed — log karo pan user la generic error dakhavto
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Submit hovu shakla nahi. Thoda vel wait karin punha try kara.",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}
