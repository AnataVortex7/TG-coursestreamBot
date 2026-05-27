package routes

// /stream/:token — public streaming endpoint.
// कोणत्याही login शिवाय access होतो.
// Token 3 तासांसाठी valid असतो (bot generate करतो).

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"path/filepath"
	"strings"

	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/stream"
	"EverythingSuckz/fsb/internal/token"
	"EverythingSuckz/fsb/internal/utils"

	range_parser "github.com/quantumsheep/range-parser"
	"github.com/gin-gonic/gin"
)

func (r *Route) StreamRoutes(route *Route) {
	r.Engine.GET("/stream/:token", handlePublicStream)
}

// handlePublicStream — /stream/:token
// Token verify करतो, file Telegram वरून stream करतो.
// Auth नाही — URL असलेला कोणीही access करू शकतो.
func handlePublicStream(c *gin.Context) {
	w := c.Writer
	req := c.Request

	// 1. Token verify
	tokenStr := c.Param("token")
	messageID, err := token.VerifyPublicToken(tokenStr)
	if err != nil {
		switch err {
		case token.ErrTokenExpired:
			c.JSON(http.StatusForbidden, gin.H{"error": "Stream link has expired (3 hour limit)."})
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid stream link."})
		}
		return
	}

	// 2. Telegram वरून file info आण
	worker := bot.GetNextWorker()
	file, err := utils.FileFromMessage(c, worker.Client, messageID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("Could not load file: %s", err.Error())})
		return
	}

	// 3. Photo / small file (no size info — CDN download)
	if file.FileSize == 0 {
		data, err := downloadPhotoChunked(c, worker.Client.API(), file.Location)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load photo."})
			return
		}
		c.Header("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", file.FileName))
		c.Header("Content-Length", strconv.Itoa(len(data)))
		if req.Method != "HEAD" {
			c.Data(http.StatusOK, file.MimeType, data)
		}
		return
	}

	// 4. Range-aware streaming
	mimeType := normalizePublicMime(file.MimeType, file.FileName)
	c.Header("Accept-Ranges", "bytes")
	c.Header("Content-Type", mimeType)
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", file.FileName))

	var start, end int64
	rangeHeader := req.Header.Get("Range")

	if rangeHeader == "" {
		start = 0
		end = file.FileSize - 1
		c.Header("Content-Length", strconv.FormatInt(file.FileSize, 10))
		w.WriteHeader(http.StatusOK)
	} else {
		ranges, err := range_parser.Parse(file.FileSize, rangeHeader)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		start = ranges[0].Start
		end = ranges[0].End
		contentLength := end - start + 1
		c.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, file.FileSize))
		c.Header("Content-Length", strconv.FormatInt(contentLength, 10))
		w.WriteHeader(http.StatusPartialContent)
	}

	if req.Method == "HEAD" {
		return
	}

	contentLength := end - start + 1
	pipe, err := stream.NewStreamPipe(c, worker.Client, file.Location, start, end, log)
	if err != nil {
		log.Sugar().Errorf("failed to create stream pipe: %v", err)
		return
	}
	defer pipe.Close()

	if _, err := io.CopyN(w, pipe, contentLength); err != nil {
		if !utils.IsClientDisconnectError(err) {
			log.Sugar().Errorf("stream copy error: %v", err)
		}
	}
}

// normalizePublicMime — browser-safe Content-Type.
func normalizePublicMime(mime, fileName string) string {
	switch strings.ToLower(mime) {
	case "video/x-matroska", "video/mkv", "video/x-msvideo",
		"video/avi", "video/quicktime":
		return "video/mp4"
	case "video/mp4", "video/webm", "video/ogg":
		return mime
	case "":
		ext := strings.ToLower(filepath.Ext(fileName))
		switch ext {
		case ".webm":
			return "video/webm"
		case ".mp4", ".m4v", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".3gp":
			return "video/mp4"
		case ".mp3":
			return "audio/mpeg"
		case ".pdf":
			return "application/pdf"
		}
		return "application/octet-stream"
	default:
		if strings.HasPrefix(strings.ToLower(mime), "video/") {
			return "video/mp4"
		}
		return mime
	}
}

// downloadPhotoChunked आणि downloadFromCDN watch.go मध्ये define आहेत —
// same package असल्यामुळे इथे directly वापरता येतात.
