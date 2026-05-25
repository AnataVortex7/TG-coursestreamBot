// internal/routes/admin_folder_api.go
package routes

import (
	"net/http"
	"strings"

	"EverythingSuckz/fsb/internal/db"

	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// LoadFolderAPI — reflection-based auto-load
// Base path /api/admin/folders (conflict-free — /api/admin/courses/:id already exists)
func (e *allRoutes) LoadFolderAPI(r *Route) {
	api := r.Engine.Group("/api/admin/folders", adminAuthMiddleware())
	api.GET("/:course_id", adminListFolders)
	api.POST("/:course_id", adminCreateFolder)
	api.PUT("/:course_id/:folder_id", adminUpdateFolder)
	api.DELETE("/:course_id/:folder_id", adminDeleteFolder)
	api.POST("/:course_id/reorder", adminReorderFolders)
}

// ── GET /api/admin/folders/:course_id ─────────────────────────────────────────

func adminListFolders(c *gin.Context) {
	courseID, err := parseFolderCourseID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}

	folders, err := db.GetFoldersForCourse(courseID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if folders == nil {
		folders = []db.CourseFolder{}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "folders": folders})
}

// ── POST /api/admin/folders/:course_id ────────────────────────────────────────

func adminCreateFolder(c *gin.Context) {
	courseID, err := parseFolderCourseID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}

	var body struct {
		Name   string           `json:"name"`
		Ranges []db.FolderRange `json:"ranges"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Folder name required"})
		return
	}
	if len(body.Ranges) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "At least one range required"})
		return
	}
	for _, r := range body.Ranges {
		if r.Start <= 0 || r.End <= 0 || r.Start > r.End {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid range: start must be >= 1 and <= end"})
			return
		}
	}

	folder, err := db.CreateFolder(courseID, body.Name, body.Ranges)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "folder": folder})
}

// ── PUT /api/admin/folders/:course_id/:folder_id ──────────────────────────────

func adminUpdateFolder(c *gin.Context) {
	courseID, err := parseFolderCourseID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}
	folderID, err := parseFolderID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid folder ID"})
		return
	}

	existing, err := db.GetFolderByID(folderID)
	if err != nil || existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Folder not found"})
		return
	}
	if existing.CourseID != courseID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Folder does not belong to this course"})
		return
	}

	var body struct {
		Name   string           `json:"name"`
		Ranges []db.FolderRange `json:"ranges"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Folder name required"})
		return
	}
	if len(body.Ranges) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "At least one range required"})
		return
	}
	for _, r := range body.Ranges {
		if r.Start <= 0 || r.End <= 0 || r.Start > r.End {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid range: start must be >= 1 and <= end"})
			return
		}
	}

	if err := db.UpdateFolder(folderID, body.Name, body.Ranges); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── DELETE /api/admin/folders/:course_id/:folder_id ───────────────────────────

func adminDeleteFolder(c *gin.Context) {
	courseID, err := parseFolderCourseID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}
	folderID, err := parseFolderID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid folder ID"})
		return
	}

	existing, err := db.GetFolderByID(folderID)
	if err != nil || existing == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Folder not found"})
		return
	}
	if existing.CourseID != courseID {
		c.JSON(http.StatusForbidden, gin.H{"error": "Folder does not belong to this course"})
		return
	}

	if err := db.DeleteFolder(folderID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ── POST /api/admin/folders/:course_id/reorder ────────────────────────────────

func adminReorderFolders(c *gin.Context) {
	courseID, err := parseFolderCourseID(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid course ID"})
		return
	}

	var body struct {
		Orders map[string]int `json:"orders"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || len(body.Orders) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "orders map required"})
		return
	}

	if err := db.ReorderFolders(courseID, body.Orders); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// ─── Param helpers ────────────────────────────────────────────────────────────

func parseFolderCourseID(c *gin.Context) (primitive.ObjectID, error) {
	return primitive.ObjectIDFromHex(c.Param("course_id"))
}

func parseFolderID(c *gin.Context) (primitive.ObjectID, error) {
	return primitive.ObjectIDFromHex(c.Param("folder_id"))
}
