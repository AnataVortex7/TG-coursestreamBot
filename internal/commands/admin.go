package commands

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ─── Register all admin commands ─────────────────────────────────────────────

func (m *command) LoadAdmin(d dispatcher.Dispatcher) {
	log := m.log.Named("admin")
	defer log.Sugar().Info("Loaded admin commands")

	d.AddHandler(handlers.NewCommand("adduser", adminOnly(cmdAddUser)))
	d.AddHandler(handlers.NewCommand("deluser", adminOnly(cmdDelUser)))
	d.AddHandler(handlers.NewCommand("listusers", adminOnly(cmdListUsers)))

	d.AddHandler(handlers.NewCommand("addcourse", adminOnly(cmdAddCourse)))
	d.AddHandler(handlers.NewCommand("delcourse", adminOnly(cmdDelCourse)))
	d.AddHandler(handlers.NewCommand("listcourses", adminOnly(cmdListCourses)))

	d.AddHandler(handlers.NewCommand("grantaccess", adminOnly(cmdGrantAccess)))
	d.AddHandler(handlers.NewCommand("revokeaccess", adminOnly(cmdRevokeAccess)))

	d.AddHandler(handlers.NewCommand("syncchannel", adminOnly(cmdSyncChannel)))
	d.AddHandler(handlers.NewCommand("sessions", adminOnly(cmdSessions)))
	d.AddHandler(handlers.NewCommand("killsession", adminOnly(cmdKillSession)))
	d.AddHandler(handlers.NewCommand("stats", adminOnly(cmdStats)))
}

// ─── adminOnly wrapper ────────────────────────────────────────────────────────
// Rejects anyone who is not in config.ValueOf.AllowedUsers.

func adminOnly(fn func(*ext.Context, *ext.Update) error) func(*ext.Context, *ext.Update) error {
	return func(ctx *ext.Context, u *ext.Update) error {
		senderID := u.EffectiveUser().GetID()
		if !utils.Contains(config.ValueOf.AllowedUsers, senderID) {
			ctx.Reply(u, ext.ReplyTextString("⛔ Not authorised."), nil)
			return dispatcher.EndGroups
		}
		return fn(ctx, u)
	}
}

// ─── reply helpers ────────────────────────────────────────────────────────────

func reply(ctx *ext.Context, u *ext.Update, text string) {
	ctx.Reply(u, ext.ReplyTextString(text), nil)
}

func args(u *ext.Update) []string {
	text := u.EffectiveMessage.Text
	parts := strings.Fields(text)
	if len(parts) <= 1 {
		return nil
	}
	return parts[1:]
}

// ═══════════════════════════════════════════════════════════════════════════════
// USER COMMANDS
// ═══════════════════════════════════════════════════════════════════════════════

// /adduser <mobile_or_email> <password> [full_name]
// Examples:
//   /adduser 9876543210 pass123
//   /adduser user@email.com pass123 Rahul Sharma
func cmdAddUser(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u, "Usage: /adduser <mobile_or_email> <password> [full name]\nExamples:\n/adduser 9876543210 pass123\n/adduser user@email.com pass123 Rahul Sharma")
		return dispatcher.EndGroups
	}
	identifier := strings.ToLower(strings.TrimSpace(a[0]))
	password := a[1]
	fullName := ""
	if len(a) > 2 {
		fullName = strings.Join(a[2:], " ")
	}

	// Decide if identifier is mobile or email
	mobile, email := "", ""
	if strings.Contains(identifier, "@") {
		email = identifier
	} else {
		mobile = identifier
	}

	if len(password) < 6 {
		reply(ctx, u, "❌ Password must be at least 6 characters.")
		return dispatcher.EndGroups
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		reply(ctx, u, "❌ Server error: "+err.Error())
		return dispatcher.EndGroups
	}

	user, err := db.CreateUserFull("", fullName, mobile, email, hash)
	if err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}

	info := identifier
	if fullName != "" {
		info = fullName + " (" + identifier + ")"
	}
	reply(ctx, u, fmt.Sprintf(
		"✅ User created\n\nName: %s\nLogin ID: %s\nDB ID: %s",
		info, identifier, user.ID.Hex(),
	))
	return dispatcher.EndGroups
}

// /deluser <username>
func cmdDelUser(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		reply(ctx, u, "Usage: /deluser <username>")
		return dispatcher.EndGroups
	}

	user, err := db.GetUserByUsername(strings.ToLower(a[0]))
	if err != nil || user == nil {
		reply(ctx, u, "❌ User not found.")
		return dispatcher.EndGroups
	}

	// Disable + kill all sessions
	_ = db.SetUserActive(user.ID, false)
	_ = db.DeleteAllSessionsForUser(user.ID)

	reply(ctx, u, fmt.Sprintf("✅ User @%s disabled and sessions killed.", user.Username))
	return dispatcher.EndGroups
}

// /listusers
func cmdListUsers(ctx *ext.Context, u *ext.Update) error {
	users, err := db.ListUsers()
	if err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	if len(users) == 0 {
		reply(ctx, u, "No users yet. Use /adduser to create one.")
		return dispatcher.EndGroups
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("👥 Users (%d)\n\n", len(users)))
	for _, usr := range users {
		status := "✅"
		if !usr.IsActive {
			status = "🚫"
		}
		sb.WriteString(fmt.Sprintf("%s %s  |  %s\n",
			status, usr.Username, usr.ID.Hex()))
	}
	reply(ctx, u, sb.String())
	return dispatcher.EndGroups
}

// ═══════════════════════════════════════════════════════════════════════════════
// COURSE COMMANDS
// ═══════════════════════════════════════════════════════════════════════════════

// /addcourse <title> | <description> | <price> | <buy_link> | <channel_id>
// Fields separated by | for multi-word values.
// channel_id is the Telegram channel ID for this course's content (e.g. -1001234567890)
func cmdAddCourse(ctx *ext.Context, u *ext.Update) error {
	text := u.EffectiveMessage.Text
	// Remove command prefix
	idx := strings.Index(text, " ")
	if idx == -1 {
		reply(ctx, u, "Usage: /addcourse <title> | <description> | <price> | <buy_link> | <channel_id>\n\nExample:\n/addcourse Python Basics | Learn Python from scratch | ₹999 | https://pay.example.com/python | -1001234567890\n\nEach course must have its own dedicated Telegram channel.")
		return dispatcher.EndGroups
	}
	raw := strings.TrimSpace(text[idx+1:])
	parts := strings.Split(raw, "|")
	if len(parts) < 5 {
		reply(ctx, u, "❌ Need 5 fields separated by |\nFormat: title | description | price | buy_link | channel_id\n\nEach course must have its own dedicated Telegram channel.")
		return dispatcher.EndGroups
	}

	title := strings.TrimSpace(parts[0])
	desc := strings.TrimSpace(parts[1])
	price := strings.TrimSpace(parts[2])
	buyLink := strings.TrimSpace(parts[3])
	channelIDStr := strings.TrimSpace(parts[4])

	if title == "" || buyLink == "" || channelIDStr == "" {
		reply(ctx, u, "❌ Title, buy_link and channel_id cannot be empty.")
		return dispatcher.EndGroups
	}

	channelID, err := strconv.ParseInt(channelIDStr, 10, 64)
	if err != nil {
		reply(ctx, u, "❌ Invalid channel_id. Must be a number like -1001234567890\nForward any message from your channel to @userinfobot to get its ID.")
		return dispatcher.EndGroups
	}

	course, err := db.CreateCourse(title, desc, "", price, buyLink, channelID)
	if err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}

	reply(ctx, u, fmt.Sprintf(
		"✅ Course created\n\nTitle: %s\nID: %s\nPrice: %s\nChannel: %d\n\nMake sure the bot is admin in that channel.\nThen use /syncchannel %s to index existing videos.",
		course.Title, course.ID.Hex(), course.Price, course.ChannelID, course.ID.Hex(),
	))
	return dispatcher.EndGroups
}

// /delcourse <course_id>
func cmdDelCourse(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		reply(ctx, u, "Usage: /delcourse <course_id>")
		return dispatcher.EndGroups
	}

	courseID, err := primitive.ObjectIDFromHex(a[0])
	if err != nil {
		reply(ctx, u, "❌ Invalid course ID.")
		return dispatcher.EndGroups
	}

	if err := db.DeleteCourse(courseID); err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	// Also clean cached files
	_ = db.DeleteCachedFilesForCourse(courseID)

	reply(ctx, u, "✅ Course deleted and cached files removed.")
	return dispatcher.EndGroups
}

// /listcourses
func cmdListCourses(ctx *ext.Context, u *ext.Update) error {
	courses, err := db.ListAllCourses()
	if err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	if len(courses) == 0 {
		reply(ctx, u, "No courses yet. Use /addcourse to create one.")
		return dispatcher.EndGroups
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📚 Courses (%d)\n\n", len(courses)))
	for _, c := range courses {
		count, _ := db.CountCachedFiles(c.ID)
		sb.WriteString(fmt.Sprintf(
			"📖 %s\nID: %s\nPrice: %s | Files: %d\nChannel: %d\n\n",
			c.Title, c.ID.Hex(), c.Price, count, c.ChannelID,
		))
	}
	reply(ctx, u, sb.String())
	return dispatcher.EndGroups
}

// ═══════════════════════════════════════════════════════════════════════════════
// ACCESS COMMANDS
// ═══════════════════════════════════════════════════════════════════════════════

// /grantaccess <username> <course_id>
// /grantaccess <mobile_or_email> <course_id> [months]
// months=0 or omitted → permanent access
// Example: /grantaccess 9876543210 <id> 3   → 3 months access
func cmdGrantAccess(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u, "Usage: /grantaccess <mobile_or_email> <course_id> [months]\nExamples:\n/grantaccess 9876543210 <course_id>       → permanent\n/grantaccess 9876543210 <course_id> 3     → 3 months")
		return dispatcher.EndGroups
	}

	user, err := db.GetUserByMobileOrEmail(strings.ToLower(a[0]))
	if err != nil || user == nil {
		reply(ctx, u, "❌ User not found.")
		return dispatcher.EndGroups
	}

	courseID, err := primitive.ObjectIDFromHex(a[1])
	if err != nil {
		reply(ctx, u, "❌ Invalid course ID.")
		return dispatcher.EndGroups
	}

	course, err := db.GetCourseByID(courseID)
	if err != nil || course == nil {
		reply(ctx, u, "❌ Course not found.")
		return dispatcher.EndGroups
	}

	months := 0
	if len(a) >= 3 {
		if m, err2 := strconv.Atoi(a[2]); err2 == nil && m > 0 {
			months = m
		}
	}

	adminID := u.EffectiveUser().GetID()
	if err := db.GrantAccess(user.ID, courseID, adminID, months); err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}

	expiryMsg := "♾️ Permanent (no expiry)"
	if months > 0 {
		expDate := time.Now().AddDate(0, months, 0)
		expiryMsg = fmt.Sprintf("⏳ %d month(s) → expires on %s", months, expDate.Format("02 Jan 2006"))
	}

	reply(ctx, u, fmt.Sprintf(
		"✅ Access granted\n\nUser: %s\nCourse: %s\nExpiry: %s",
		user.Username, course.Title, expiryMsg,
	))
	return dispatcher.EndGroups
}

// /revokeaccess <username> <course_id>
func cmdRevokeAccess(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u, "Usage: /revokeaccess <username> <course_id>")
		return dispatcher.EndGroups
	}

	user, err := db.GetUserByUsername(strings.ToLower(a[0]))
	if err != nil || user == nil {
		reply(ctx, u, "❌ User not found.")
		return dispatcher.EndGroups
	}

	courseID, err := primitive.ObjectIDFromHex(a[1])
	if err != nil {
		reply(ctx, u, "❌ Invalid course ID.")
		return dispatcher.EndGroups
	}

	if err := db.RevokeAccess(user.ID, courseID); err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}

	reply(ctx, u, fmt.Sprintf("✅ Access revoked for user: %s", user.Username))
	return dispatcher.EndGroups
}

// ═══════════════════════════════════════════════════════════════════════════════
// CHANNEL SYNC
// ═══════════════════════════════════════════════════════════════════════════════

// /syncchannel <course_id> [start_msg_id] [end_msg_id]
//
// Examples:
//   /syncchannel <id>              → sync all messages
//   /syncchannel <id> 100          → sync from msg ID 100 to end
//   /syncchannel <id> 100 500      → sync only msg ID 100 to 500
//
// Features:
//   - Flood wait handling (auto retry after wait)
//   - 10 second break after every 100 messages cached
//   - Progress updates every 100 messages

// /syncchannel <course_id> <start_msg_id> <end_msg_id>
//
// Examples:
//   /syncchannel <id> 1 500     → sync msg ID 1 to 500
//   /syncchannel <id> 100 200   → sync msg ID 100 to 200
//
// Uses ChannelsGetMessages (same as /stream route) — explicit ID fetch.
// Requires both start and end. Bot client used (fixes PeerStorage issue).

func cmdSyncChannel(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 3 {
		reply(ctx, u, "Usage:\n/syncchannel <course_id> <start_msg_id> <end_msg_id>\n\nExample:\n/syncchannel 507f1f... 1 500")
		return dispatcher.EndGroups
	}

	courseID, err := primitive.ObjectIDFromHex(a[0])
	if err != nil {
		reply(ctx, u, "❌ Invalid course ID.")
		return dispatcher.EndGroups
	}

	course, err := db.GetCourseByID(courseID)
	if err != nil || course == nil {
		reply(ctx, u, "❌ Course not found.")
		return dispatcher.EndGroups
	}

	if course.ChannelID == 0 {
		reply(ctx, u, "❌ This course has no channel set.")
		return dispatcher.EndGroups
	}

	startMsgID, err1 := strconv.Atoi(a[1])
	endMsgID, err2 := strconv.Atoi(a[2])
	if err1 != nil || err2 != nil || startMsgID < 1 || endMsgID < startMsgID {
		reply(ctx, u, "❌ Invalid range.\nstart_msg_id and end_msg_id must be positive integers, start ≤ end.")
		return dispatcher.EndGroups
	}

	reply(ctx, u, fmt.Sprintf(
		"⏳ Syncing channel %d\nCourse: %s\nRange: msg ID %d → %d\n\nStarting...",
		course.ChannelID, course.Title, startMsgID, endMsgID,
	))

	// ✅ bot.Bot वापर — ctx.Raw नाही (PeerStorage fix)
	botClient := MainBotClient
	channel, err := utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, course.ChannelID)
	if err != nil {
		reply(ctx, u, "❌ Cannot access course channel: "+err.Error()+"\n\nMake sure the bot is admin in that channel.")
		return dispatcher.EndGroups
	}

	const (
		batchSize    = 100 // Telegram ChannelsGetMessages max per call
		breakEvery   = 100
		breakSeconds = 10
	)

	total := 0
	skipped := 0
	sinceBreak := 0

	// startMsgID to endMsgID ला batches मध्ये process कर
	for batchStart := startMsgID; batchStart <= endMsgID; batchStart += batchSize {

		batchEnd := batchStart + batchSize - 1
		if batchEnd > endMsgID {
			batchEnd = endMsgID
		}

		// ✅ Explicit ID list बनव — /channel route सारखंच
		ids := make([]tg.InputMessageClass, 0, batchEnd-batchStart+1)
		for id := batchStart; id <= batchEnd; id++ {
			ids = append(ids, &tg.InputMessageID{ID: id})
		}

		// ✅ ChannelsGetMessages — /stream सारखीच API method
		var res tg.MessagesMessagesClass
		var fetchErr error

		for attempt := 0; attempt < 5; attempt++ {
			res, fetchErr = botClient.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
				Channel: channel,
				ID:      ids,
			})
			if fetchErr == nil {
				break
			}
			errStr := fetchErr.Error()
			if strings.Contains(errStr, "FLOOD_WAIT_") {
				waitSec := 30
				parts := strings.Split(errStr, "FLOOD_WAIT_")
				if len(parts) > 1 {
					if n, e := strconv.Atoi(strings.Fields(parts[1])[0]); e == nil {
						waitSec = n
					}
				}
				reply(ctx, u, fmt.Sprintf("⚠️ Flood wait: pausing %d seconds...", waitSec))
				time.Sleep(time.Duration(waitSec+2) * time.Second)
				continue
			}
			break
		}

		if fetchErr != nil {
			reply(ctx, u, "❌ Fetch error: "+fetchErr.Error())
			return dispatcher.EndGroups
		}

		messages, ok := res.(*tg.MessagesChannelMessages)
		if !ok {
			reply(ctx, u, "❌ Unexpected response type from Telegram.")
			return dispatcher.EndGroups
		}

		for _, rawMsg := range messages.Messages {
			// Message ID काढा — *tg.Message किंवा *tg.MessageEmpty दोन्हीतून
			var msgID int
			var caption string
			var media tg.MessageMediaClass

			switch m := rawMsg.(type) {
			case *tg.Message:
				msgID = m.ID
				caption = strings.TrimSpace(m.Message)
				media = m.Media
			case *tg.MessageEmpty:
				msgID = m.ID
				// empty message — no caption, no media
			default:
				// *tg.MessageService (join/pin) — skip
				skipped++
				continue
			}

			if msgID == 0 {
				skipped++
				continue
			}

			// Default: video/mp4 — caption नसेल तर msg ID
			fileName := fmt.Sprintf("video_%d.mp4", msgID)
			mimeType := "video/mp4"
			var fileSize int64 = 0
			title := caption
			if title == "" {
				title = fmt.Sprintf("Video #%d", msgID)
			}

			// Media असेल तर actual values वापर
			if media != nil {
				if file, err := utils.FileFromMedia(media); err == nil {
					if file.FileName != "" {
						fileName = file.FileName
					}
					if file.MimeType != "" {
						mimeType = normalizeMime(file.MimeType, file.FileName)
					}
					fileSize = file.FileSize
					if title == fmt.Sprintf("Video #%d", msgID) && file.FileName != "" {
						title = stripExt(file.FileName)
					}
				}
			}

			cached := &db.CachedFile{
				MessageID: msgID,
				ChannelID: course.ChannelID,
				CourseID:  courseID,
				Title:     title,
				Caption:   caption,
				FileName:  fileName,
				MimeType:  mimeType,
				FileSize:  fileSize,
				SortOrder: msgID,
				CreatedAt: time.Now(),
			}
			if err := db.UpsertCachedFile(cached); err != nil {
				skipped++
				continue
			}
			total++
			sinceBreak++

			if sinceBreak >= breakEvery {
				reply(ctx, u, fmt.Sprintf(
					"📦 Progress: %d files cached so far...\n⏸ Pausing %d seconds...",
					total, breakSeconds,
				))
				time.Sleep(time.Duration(breakSeconds) * time.Second)
				sinceBreak = 0
			}
		}
	}

	reply(ctx, u, fmt.Sprintf(
		"✅ Sync complete!\n\nCourse: %s\nChannel: %d\nRange: msg %d → %d\n\n📁 Files cached: %d\n⚠️ Skipped (no media): %d",
		course.Title, course.ChannelID, startMsgID, endMsgID, total, skipped,
	))
	return dispatcher.EndGroups
}

// ═══════════════════════════════════════════════════════════════════════════════
// SESSION COMMANDS
// ═══════════════════════════════════════════════════════════════════════════════

// /sessions — list all active sessions
func cmdSessions(ctx *ext.Context, u *ext.Update) error {
	sessions, err := db.ListActiveSessions()
	if err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	if len(sessions) == 0 {
		reply(ctx, u, "No active sessions.")
		return dispatcher.EndGroups
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔐 Active sessions (%d)\n\n", len(sessions)))
	for i, s := range sessions {
		user, _ := db.GetUserByID(s.UserID)
		uname := "unknown"
		if user != nil {
			uname = user.Username
		}
		expires := s.ExpiresAt.Format("02 Jan 2006 15:04")
		sb.WriteString(fmt.Sprintf(
			"%d. %s\n   IP: %s\n   Expires: %s\n   Token: ...%s\n\n",
			i+1, uname, s.IP, expires,
			s.Token[max(0, len(s.Token)-8):], // last 8 chars only
		))
	}
	sb.WriteString("Use /killsession <username> to force logout.")
	reply(ctx, u, sb.String())
	return dispatcher.EndGroups
}

// /killsession <username> — force logout a user
func cmdKillSession(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		reply(ctx, u, "Usage: /killsession <username>")
		return dispatcher.EndGroups
	}

	user, err := db.GetUserByUsername(strings.ToLower(a[0]))
	if err != nil || user == nil {
		reply(ctx, u, "❌ User not found.")
		return dispatcher.EndGroups
	}

	if err := db.DeleteAllSessionsForUser(user.ID); err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}

	reply(ctx, u, fmt.Sprintf("✅ All sessions killed for user: %s\nThey will be logged out on next request.", user.Username))
	return dispatcher.EndGroups
}

// ═══════════════════════════════════════════════════════════════════════════════
// STATS
// ═══════════════════════════════════════════════════════════════════════════════

// /stats
func cmdStats(ctx *ext.Context, u *ext.Update) error {
	users, _ := db.ListUsers()
	courses, _ := db.ListAllCourses()
	sessions, _ := db.ListActiveSessions()

	activeUsers := 0
	for _, usr := range users {
		if usr.IsActive {
			activeUsers++
		}
	}

	totalFiles := int64(0)
	for _, c := range courses {
		n, _ := db.CountCachedFiles(c.ID)
		totalFiles += n
	}

	reply(ctx, u, fmt.Sprintf(
		"📊 Stats\n\n"+
			"👥 Users: %d total, %d active\n"+
			"📚 Courses: %d\n"+
			"🎬 Cached files: %d\n"+
			"🔐 Active sessions: %d",
		len(users), activeUsers,
		len(courses),
		totalFiles,
		len(sessions),
	))
	return dispatcher.EndGroups
}

// ─── Go 1.21 min/max compat ───────────────────────────────────────────────────

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ─── Auto-append new uploads to channel cache ─────────────────────────────────
// This is called from the existing stream.go sendLink handler AFTER forwarding.
// When admin uploads a new file to the bot, it gets forwarded to LOG_CHANNEL
// and we cache it here so /syncchannel doesn't need to be re-run.

func AutoCacheNewFile(messageID int, channelID int64, courseID primitive.ObjectID, caption, fileName, mimeType string, fileSize int64) error {
	
	// Title: caption असेल तर caption, नाहीतर filename extension शिवाय
	title := strings.TrimSpace(caption)
	if title == "" {
		title = stripExt(fileName)
	}

	cached := &db.CachedFile{
		MessageID: messageID,
		ChannelID: channelID,
		CourseID:  courseID,
		Title:     title,
		Caption:   strings.TrimSpace(caption),
		FileName:  fileName,
		MimeType:  mimeType,
		FileSize:  fileSize,
		SortOrder: messageID, 
		CreatedAt: time.Now(),
	}
	return db.UpsertCachedFile(cached)
}

func stripExt(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			return name[:i]
		}
	}
	return name
}

// unused import guard
var _ = strconv.Itoa

// normalizeMime — browser-safe video mime type return करतो.
// MKV/AVI सारखे types mp4 म्हणून serve केले जातात जेणेकरून browser play करू शकतो.
func normalizeMime(mime, fileName string) string {
	switch strings.ToLower(mime) {
	case "video/x-matroska", "video/mkv":
		return "video/mp4"
	case "video/x-msvideo", "video/avi":
		return "video/mp4"
	case "video/quicktime":
		return "video/mp4"
	case "video/mp4", "video/webm", "video/ogg":
		return mime
	default:
		if strings.HasPrefix(strings.ToLower(mime), "video/") {
			return "video/mp4"
		}
	}
	ext := strings.ToLower(filepath.Ext(fileName))
	switch ext {
	case ".mp4", ".m4v", ".mkv", ".avi", ".mov", ".wmv", ".flv", ".3gp":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	}
	return "video/mp4"
}
