// internal/commands/watermark_deliver.go
//
// Offline-download (student) videos साठी — direct copy-forward ऐवजी, video
// download करून त्यावर user चा mobile number FFmpeg ने burn-in watermark
// म्हणून लावतो, मग तोच watermarked file user ला पाठवतो. यामुळे content
// screen-record / re-share झाला तरी user ची identity video मध्येच राहते.
//
// Colab worker online असेल तर (KOYEB_URL set + wmqueue.IsColabOnline())
// त्याच्याकडे job पाठवला जातो — जड FFmpeg काम मुख्य server वर येत नाही,
// Colab स्वतःच्या Telethon session ने थेट student ला video पाठवतो. Colab
// offline/fail/timeout असेल तर local (main server) FFmpeg fallback वापरतो —
// हा नेहमी काम करतो, कशावरही अवलंबून नाही.

package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"EverythingSuckz/fsb/internal/utils"
	"EverythingSuckz/fsb/internal/wmqueue"

	"github.com/celestix/gotgproto"
	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
)

// deliverWatermarkedVideo — channelID/messageID वरून **ज्या client ने download
// करायचा त्याच client ने** fresh file_reference fetch करतो (bot ने fetch केलेला
// reference userbot ला — किंवा उलट — चालत नाही, त्यामुळे FILE_REFERENCE_EXPIRED
// येतो; हेच टाळण्यासाठी हा refetch), मग FFmpeg ने watermarkText burn करतो, मग
// bot client ने upload करून tg.InputFileClass परत देतो (caller ने पुढे
// MessagesSendMedia ने पाठवायचं).
//
// User ला दिसणारे टप्पे मुद्दाम फक्त 2 च ठेवले आहेत:
//   📥 Downloading   — खरा byte-progress
//   📤 Uploading     — यात watermark burn (0-80%) आणि actual upload (80-100%)
//                      दोन्ही लपवून एकाच bar मध्ये दाखवले जातात — "watermark
//                      लावतोय" असं वेगळं कधीच दिसत नाही.
func deliverWatermarkedVideo(
	botClient *gotgproto.Client,
	channelID int64,
	messageID int,
	fileName string,
	watermarkText string,
	editProgress func(string),
) (tg.InputFileClass, error) {

	startTime := time.Now()
	elapsed := func() string {
		d := time.Since(startTime)
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm %ds", m, s)
	}

	dlClient := botClient
	if GetLiveUserbotClient != nil {
		if uc := GetLiveUserbotClient(); uc != nil {
			dlClient = uc
		}
	}

	// याच dlClient ने fresh file_reference मिळवतो — दुसऱ्या client ने fetch
	// केलेला reference वापरला तर Telegram FILE_REFERENCE_EXPIRED देतो.
	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 30*time.Second)
	file, err := utils.FileFromMessageInChannel(fetchCtx, dlClient, messageID, channelID)
	fetchCancel()
	if err != nil {
		return nil, fmt.Errorf("file reference fetch failed: %w", err)
	}
	if fileName == "" {
		fileName = file.FileName
	}

	fext := filepath.Ext(fileName)
	if fext == "" {
		fext = ".mp4"
	}
	tmpIn, err := os.CreateTemp("", "wmdl-in-*"+fext)
	if err != nil {
		return nil, fmt.Errorf("temp file create failed: %w", err)
	}
	inPath := tmpIn.Name()
	defer os.Remove(inPath)

	// ── 📥 Downloading — खरा byte % ──────────────────────────────────────
	dlWriter := &progressWriter{
		w:     tmpIn,
		total: file.FileSize,
		onProgress: func(written, total int64) {
			pct := 0
			if total > 0 {
				pct = int(written * 100 / total)
			}
			editProgress(fmt.Sprintf(
				"📥 Downloading...\n%s %d%%\n💾 %s / %s\n⏱ %s",
				progressBar(pct), pct, formatSize(written), formatSize(total), elapsed(),
			))
		},
	}
	editProgress(fmt.Sprintf("📥 Downloading...\n%s 0%%\n💾 0 B / %s\n⏱ %s", progressBar(0), formatSize(file.FileSize), elapsed()))

	dlCtx, dlCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer dlCancel()

	d := downloader.NewDownloader()
	_, err = d.Download(dlClient.API(), file.Location).Stream(dlCtx, dlWriter)
	tmpIn.Close()
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}

	// ── 📤 Uploading — watermark burn (0-80%) + actual upload (80-100%),
	// एकाच progress bar मध्ये. "watermark" शब्द इथे कुठेच user ला दिसत नाही.
	outPath := inPath + "_wm.mp4"
	defer os.Remove(outPath)

	renderUploading := func(pct int) string {
		if pct > 100 {
			pct = 100
		}
		if pct < 0 {
			pct = 0
		}
		return fmt.Sprintf("📤 Uploading...\n%s %d%%\n⏱ %s", progressBar(pct), pct, elapsed())
	}
	editProgress(renderUploading(0))

	onFFmpegProgress := func(fracDone float64) {
		editProgress(renderUploading(int(fracDone * 80)))
	}

	if err := burnMobileWatermark(inPath, outPath, watermarkText, onFFmpegProgress); err != nil {
		return nil, fmt.Errorf("watermark failed: %w", err)
	}
	editProgress(renderUploading(80))

	outFile, err := os.Open(outPath)
	if err != nil {
		return nil, fmt.Errorf("watermarked file open error: %w", err)
	}
	defer outFile.Close()

	var outSize int64
	if info, statErr := outFile.Stat(); statErr == nil {
		outSize = info.Size()
	}

	upReader := &progressWriter{
		w:     outFile,
		total: outSize,
		onProgress: func(written, total int64) {
			frac := 0.0
			if total > 0 {
				frac = float64(written) / float64(total)
			}
			editProgress(renderUploading(80 + int(frac*20)))
		},
	}

	upCtx, upCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer upCancel()

	up := uploader.NewUploader(botClient.API()).WithPartSize(256 * 1024)
	uploaded, err := up.FromReader(upCtx, fileName, upReader)
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	editProgress(renderUploading(100))
	return uploaded, nil
}

// burnMobileWatermark — FFmpeg ने video वर 5 ठिकाणी (4 कोपरे + मध्यभागी)
// अर्ध-पारदर्शक mobile number burn करतो — फक्त एक कोपरा crop करून काढता येऊ नये
// म्हणून संपूर्ण frame वर पसरवलेला. onProgress(fracDone 0.0-1.0) ला ffmpeg च्या
// "-progress pipe:1" output वरून live कळवत राहतो (ffprobe ने आधी duration हवा,
// नाहीतर progress callback अजिबात call होत नाही — फक्त शेवटी burn पूर्ण होतो).
func burnMobileWatermark(inPath, outPath, text string, onProgress func(fracDone float64)) error {
	if strings.TrimSpace(text) == "" {
		text = "PROTECTED"
	}
	esc := escapeDrawtext(text)

	fontArg := ""
	if font := resolveWatermarkFont(); font != "" {
		fontArg = fmt.Sprintf(":fontfile=%s", escapeDrawtextPath(font))
	}

	positions := []string{
		"x=10:y=10",
		"x=w-tw-10:y=10",
		"x=10:y=h-th-10",
		"x=w-tw-10:y=h-th-10",
		"x=(w-tw)/2:y=(h-th)/2",
	}
	filters := make([]string, 0, len(positions))
	for _, pos := range positions {
		filters = append(filters, fmt.Sprintf(
			"drawtext=text='%s'%s:fontcolor=white@0.45:box=1:boxcolor=black@0.25:boxborderw=4:fontsize=26:%s",
			esc, fontArg, pos,
		))
	}
	vf := strings.Join(filters, ",")

	durationSec := probeDurationSeconds(inPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",
		"-i", inPath,
		"-vf", vf,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", "26",
		"-c:a", "copy",
		"-progress", "pipe:1",
		"-nostats",
		outPath,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg stdout pipe error: %w", err)
	}
	var stderrBuf strings.Builder
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start error: %w", err)
	}

	done := make(chan struct{})
	if onProgress != nil && durationSec > 0 {
		go func() {
			parseFFmpegProgress(stdout, durationSec, onProgress)
			close(done)
		}()
	} else {
		go func() {
			_, _ = io.Copy(io.Discard, stdout)
			close(done)
		}()
	}

	waitErr := cmd.Wait()
	<-done
	if waitErr != nil {
		return fmt.Errorf("ffmpeg error: %v\n%s", waitErr, truncateOutput(stderrBuf.String()))
	}
	return nil
}

// probeDurationSeconds — ffprobe ने video duration (seconds) काढतो. Error आला
// तर 0 देतो — त्यावेळी FFmpeg live % दिसत नाही (फक्त 0% → 80% थेट), पण watermark
// burn काम व्यवस्थित होतंच.
func probeDurationSeconds(path string) float64 {
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path).Output()
	if err != nil {
		return 0
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return d
}

// parseFFmpegProgress — "-progress pipe:1" चं key=value stdout वाचून "out_time="
// (HH:MM:SS.micro) वरून fraction-done काढतो व दर 2 सेकंदांनी onProgress ला कळवतो.
func parseFFmpegProgress(r io.Reader, durationSec float64, onProgress func(fracDone float64)) {
	scanner := bufio.NewScanner(r)
	lastSent := time.Time{}
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "out_time=") {
			continue
		}
		tStr := strings.TrimPrefix(line, "out_time=")
		parts := strings.Split(tStr, ":")
		if len(parts) != 3 {
			continue
		}
		h, err1 := strconv.ParseFloat(parts[0], 64)
		m, err2 := strconv.ParseFloat(parts[1], 64)
		s, err3 := strconv.ParseFloat(parts[2], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		doneSec := h*3600 + m*60 + s
		frac := doneSec / durationSec
		if frac > 1 {
			frac = 1
		}
		if frac < 0 {
			frac = 0
		}
		if time.Since(lastSent) < 2*time.Second {
			continue
		}
		lastSent = time.Now()
		onProgress(frac)
	}
}

// escapeDrawtext — ffmpeg drawtext filter साठी text value मधले special
// characters escape करतो (backslash, colon, single quote).
func escapeDrawtext(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`:`, `\:`,
		`'`, `\'`,
	)
	return r.Replace(s)
}

// escapeDrawtextPath — fontfile path मध्ये कधी ':' (उदा. Windows path) असेल तर escape.
func escapeDrawtextPath(p string) string {
	return strings.ReplaceAll(p, `:`, `\:`)
}

// resolveWatermarkFont — Debian/Ubuntu-based containers वर सर्रास असणाऱ्या
// fonts पैकी पहिला उपलब्ध path शोधतो. काहीच सापडलं नाही तर रिकामं string
// देतो — त्यावेळी ffmpeg स्वतःचा default (fontconfig असेल तर) वापरतो.
func resolveWatermarkFont() string {
	candidates := []string{
		"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Bold.ttf",
		"/usr/share/fonts/truetype/liberation2/LiberationSans-Bold.ttf",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func truncateOutput(s string) string {
	const maxLen = 500
	if len(s) <= maxLen {
		return s
	}
	return s[len(s)-maxLen:] // शेवटचा भाग — actual error तिथेच असतो
}

// ═══════════════════════════════════════════════════════════════════════════
// Colab routing — online असेल तर तिकडे, नाहीतर local fallback
// ═══════════════════════════════════════════════════════════════════════════

// deliverVideoToUser — मुख्य entry point. Colab worker खरंच online असेल
// (KOYEB_URL set + wmqueue.IsColabOnline()) तरच तिकडे job पाठवतो; एरर/timeout
// आला किंवा Colab offline असेल तर local (main server) FFmpeg fallback वापरतो
// — त्यामुळे download नेहमी काम करत राहतो, Colab वर अवलंबून नाही.
//
// Returns:
//
//	uploadedFile   — local path असेल तर caller ने MessagesSendMedia ने पाठवायचा (viaColab=false तेव्हाच)
//	sentMessageID  — colab path असेल तर आधीच पाठवलेल्या message चा ID (48hr auto-delete साठी)
//	viaColab       — true असेल तर Colab ने आधीच थेट student ला पाठवलंय, caller ने परत पाठवायची गरज नाही
func deliverVideoToUser(
	botClient *gotgproto.Client,
	channelID int64,
	messageID int,
	fileName string,
	watermarkText string,
	studentChatID int64,
	progressMsgID int,
	caption string,
	editProgress func(string),
) (uploadedFile tg.InputFileClass, sentMessageID int, viaColab bool, err error) {

	if os.Getenv("KOYEB_URL") != "" && wmqueue.IsColabOnline() {
		msgID, colabErr := deliverViaColab(
			botClient, channelID, messageID, fileName, watermarkText,
			studentChatID, progressMsgID, caption, editProgress,
		)
		if colabErr == nil {
			return nil, msgID, true, nil
		}
		utils.Logger.Sugar().Warnw("colab delivery failed, falling back to local server", "err", colabErr)
		editProgress("⚠️ Colab सध्या उपलब्ध नाही — server वर process करत आहोत...")
	}

	uploaded, localErr := deliverWatermarkedVideo(botClient, channelID, messageID, fileName, watermarkText, editProgress)
	return uploaded, 0, false, localErr
}

// deliverViaColab — source file साठी एक pipe-URL register करतो (Colab तो GET
// करून download करतो), मग wmqueue वर job submit करून Colab स्वतःच्या Telethon
// session ने (bot token वापरून) थेट studentChatID ला watermark केलेला video
// पाठवतो. Main server ला processed file कधीच परत येत नाही — फक्त success/fail
// आणि (नवीन) sent message ID कळतो.
func deliverViaColab(
	botClient *gotgproto.Client,
	channelID int64,
	messageID int,
	fileName string,
	watermarkText string,
	studentChatID int64,
	progressMsgID int,
	caption string,
	editProgress func(string),
) (int, error) {

	dlClient := botClient
	if GetLiveUserbotClient != nil {
		if uc := GetLiveUserbotClient(); uc != nil {
			dlClient = uc
		}
	}

	fetchCtx, fetchCancel := context.WithTimeout(context.Background(), 30*time.Second)
	file, err := utils.FileFromMessageInChannel(fetchCtx, dlClient, messageID, channelID)
	fetchCancel()
	if err != nil {
		return 0, fmt.Errorf("file reference fetch failed: %w", err)
	}
	if fileName == "" {
		fileName = file.FileName
	}

	dlCtx, dlCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer dlCancel()

	pipeToken := fmt.Sprintf("dl-pipe-%d-%d", time.Now().UnixNano(), rand.Intn(9999))
	pipeFn := func(w http.ResponseWriter) error {
		pipeReader, pipeWriter := io.Pipe()
		dlErrCh := make(chan error, 1)
		go func() {
			defer pipeWriter.Close()
			d := downloader.NewDownloader()
			_, dlErr := d.Download(dlClient.API(), file.Location).Stream(dlCtx, pipeWriter)
			dlErrCh <- dlErr
		}()
		_, copyErr := io.Copy(w, pipeReader)
		dlErr := <-dlErrCh
		if copyErr != nil {
			return copyErr
		}
		return dlErr
	}
	wmqueue.RegisterPipe(pipeToken, fileName, file.FileSize, pipeFn)
	defer wmqueue.UnregisterPipe(pipeToken)

	koyebURL := os.Getenv("KOYEB_URL")
	fileURL := fmt.Sprintf("%s/api/colab/tempfile/%s", koyebURL, pipeToken)
	botToken := os.Getenv("BOT_TOKEN")

	editProgress("📤 Uploading...\n☁️ पाठवत आहोत...")

	jobID, err := wmqueue.SubmitJob(
		fileURL,
		fileName,
		file.FileSize,
		watermarkText,
		botToken,
		studentChatID, // adminChatID param — progress-edit इथेच होतो (student ची चॅट)
		progressMsgID,
		studentChatID, // sendToChatID — student ला थेट पाठवायचा
		0,             // sendToAccessHash — private chat, Colab च्या entity cache वर (admin flow सारखाच पॅटर्न)
		0,             // sendToTopicID
		0,             // logChatID — student downloads साठी LOG_CHANNEL backup नको (स्पॅम टाळायला)
		0,             // logAccessHash
		caption,
		messageID,
		true, // protectContent — offline course student copy: forward/save block
	)
	if err != nil {
		return 0, fmt.Errorf("submit job failed: %w", err)
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer waitCancel()

	success, jobErr, sentMessageID := wmqueue.WaitForDone(waitCtx, jobID)
	if !success {
		if jobErr == "" {
			jobErr = "colab job timeout/failed"
		}
		return 0, errors.New(jobErr)
	}
	return sentMessageID, nil
}