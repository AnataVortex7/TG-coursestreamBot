// internal/routes/messages_api.go
// User-facing "Your Messages" — admin ne pathवलेले messages (email सोबत
// in-app pn disतात). List ughadli ki sagle unread "read" mhanun mark hotat.
package routes

import (
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/db"
	"net/http"

	"github.com/gin-gonic/gin"
)

func (e *allRoutes) LoadMessagesAPI(r *Route) {
	r.Engine.GET("/api/my-messages", auth.RequireAuth, handleMyMessages)
	r.Engine.GET("/api/my-messages/unread-count", auth.RequireAuth, handleMyMessagesUnreadCount)
}

// GET /api/my-messages
// Sagle messages (newest first) return karto, ANI tyacha nantar sagle unread
// "read" mhanun mark karто (list ughadne = baghitले, simple rule). Mhanun ya
// response madhle is_unread flags he "tya veळेची" sthiti dakhवतात — pudhchya
// vela parत call kela tar sagle already-read disतील (navin message yeईparyant).
func handleMyMessages(c *gin.Context) {
	userID := auth.GetUserID(c)

	msgs, err := db.ListUserMessages(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Response madhe is_unread (current state, mark-as-read hoण्या आधीची)
	type msgOut struct {
		ID       string `json:"id"`
		Subject  string `json:"subject"`
		Message  string `json:"message"`
		SentAt   string `json:"sent_at"`
		IsUnread bool   `json:"is_unread"`
	}
	out := make([]msgOut, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, msgOut{
			ID:       m.ID.Hex(),
			Subject:  m.Subject,
			Message:  m.Message,
			SentAt:   m.SentAt.Format("02 Jan 2006, 3:04 PM"),
			IsUnread: m.ReadAt == nil,
		})
	}

	// Ata sagle "read" mhanun mark — list baghitlाच, mhanje vाचलाच (simple rule)
	_ = db.MarkAllUserMessagesRead(userID)

	c.JSON(http.StatusOK, out)
}

// GET /api/my-messages/unread-count
// Nav bar cha red badge sathi — fakt count, kahihi mark-as-read karत nahi.
func handleMyMessagesUnreadCount(c *gin.Context) {
	userID := auth.GetUserID(c)
	count, err := db.CountUnreadUserMessages(userID)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"unread_count": 0})
		return
	}
	c.JSON(http.StatusOK, gin.H{"unread_count": count})
}
