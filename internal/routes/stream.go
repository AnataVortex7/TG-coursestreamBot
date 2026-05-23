package routes

// /stream/:messageID route REMOVED for security.
// Admin साठी Telegram bot command वापरा: /adminstream <messageID>
//
// downloadPhotoChunked and downloadFromCDN are kept here because
// internal/routes/watch.go uses them for photo streaming.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"

	"github.com/gotd/td/tg"
	"go.uber.org/zap"

	"github.com/gin-gonic/gin"
)

var log *zap.Logger

// downloadPhotoChunked downloads a photo chunk by chunk,
// handling both upload.file and upload.fileCdnRedirect responses.
func downloadPhotoChunked(ctx *gin.Context, api *tg.Client, location tg.InputFileLocationClass) ([]byte, error) {
	const chunkSize = 512 * 1024 // 512 KB
	var buf bytes.Buffer
	var offset int64

	for {
		res, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: location,
			Offset:   offset,
			Limit:    chunkSize,
			Precise:  true,
		})
		if err != nil {
			return nil, fmt.Errorf("UploadGetFile offset=%d: %w", offset, err)
		}

		switch r := res.(type) {
		case *tg.UploadFile:
			chunk := r.GetBytes()
			if len(chunk) == 0 {
				return buf.Bytes(), nil
			}
			buf.Write(chunk)
			offset += int64(len(chunk))
			if len(chunk) < chunkSize {
				return buf.Bytes(), nil
			}

		case *tg.UploadFileCDNRedirect:
			return downloadFromCDN(r)

		default:
			return nil, fmt.Errorf("unexpected UploadGetFile response type %T", res)
		}
	}
}

// downloadFromCDN fetches photo data from Telegram CDN.
func downloadFromCDN(redirect *tg.UploadFileCDNRedirect) ([]byte, error) {
	url := fmt.Sprintf("https://cdn%d.telegram.org/file/%s",
		redirect.DCID,
		string(redirect.FileToken),
	)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("CDN fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CDN returned status %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("CDN read: %w", err)
	}
	return data, nil
}
