package routes

import (
	"EverythingSuckz/fsb/internal/bot"
	"EverythingSuckz/fsb/internal/stream"
	"EverythingSuckz/fsb/internal/types"
	"EverythingSuckz/fsb/internal/utils"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/gotd/td/tg"
	range_parser "github.com/quantumsheep/range-parser"
	"go.uber.org/zap"

	"github.com/gin-gonic/gin"
)

var log *zap.Logger

func (e *allRoutes) LoadHome(r *Route) {
	log = e.log.Named("Stream")
	defer log.Info("Loaded stream route")
	r.Engine.GET("/stream/:messageID", getStreamRoute)
}

func getStreamRoute(ctx *gin.Context) {
	w := ctx.Writer
	r := ctx.Request

	messageIDParm := ctx.Param("messageID")
	messageID, err := strconv.Atoi(messageIDParm)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	worker := bot.GetNextWorker()

	file, err := utils.TimeFuncWithResult(log, "FileFromMessage", func() (*types.File, error) {
		return utils.FileFromMessage(ctx, worker.Client, messageID)
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// for photo messages
	if file.FileSize == 0 {
		data, err := downloadPhotoChunked(ctx, worker.Client.API(), file.Location)
		if err != nil {
			log.Error("photo download failed", zap.Error(err))
			http.Error(w, "failed to load photo: "+err.Error(), http.StatusInternalServerError)
			return
		}
		ctx.Header("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", file.FileName))
		ctx.Header("Content-Length", strconv.Itoa(len(data)))
		if r.Method != "HEAD" {
			ctx.Data(http.StatusOK, file.MimeType, data)
		}
		return
	}

	ctx.Header("Accept-Ranges", "bytes")
	var start, end int64
	rangeHeader := r.Header.Get("Range")

	if rangeHeader == "" {
		start = 0
		end = file.FileSize - 1
		w.WriteHeader(http.StatusOK)
	} else {
		ranges, err := range_parser.Parse(file.FileSize, r.Header.Get("Range"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		start = ranges[0].Start
		end = ranges[0].End
		ctx.Header("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, file.FileSize))
		log.Info("Content-Range", zap.Int64("start", start), zap.Int64("end", end), zap.Int64("fileSize", file.FileSize))
		w.WriteHeader(http.StatusPartialContent)
	}

	contentLength := end - start + 1
	mimeType := file.MimeType

	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	ctx.Header("Content-Type", mimeType)
	ctx.Header("Content-Length", strconv.FormatInt(contentLength, 10))

	disposition := "inline"

	if ctx.Query("d") == "true" {
		disposition = "attachment"
	}

	ctx.Header("Content-Disposition", fmt.Sprintf("%s; filename=\"%s\"", disposition, file.FileName))

	if r.Method != "HEAD" {
		pipe, err := stream.NewStreamPipe(ctx, worker.Client, file.Location, start, end, log)
		if err != nil {
			log.Error("Failed to create stream pipe", zap.Error(err))
			return
		}
		defer pipe.Close()
		if _, err := io.CopyN(w, pipe, contentLength); err != nil {
			if !utils.IsClientDisconnectError(err) {
				log.Error("Error while copying stream", zap.Error(err))
			}
		}
	}
}

// ── Photo download helpers ────────────────────────────────────────────────────

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
