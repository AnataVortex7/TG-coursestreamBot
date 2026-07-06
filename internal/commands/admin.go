package commands

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/email"
	"EverythingSuckz/fsb/internal/utils"
	"EverythingSuckz/fsb/internal/wmqueue"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

// ─── Register all admin commands ─────────────────────────────────────────────

func (m *command) LoadAdmin(d dispatcher.Dispatcher) {
	log := m.log.Named("admin")
	defer log.Sugar().Info("Loaded admin commands")

	d.AddHandler(handlers.NewCommand("cmd", adminOnly(cmdMenuRoot)))
	d.AddHandler(handlers.NewCommand("adduser", adminOnly(cmdAddUser)))
	d.AddHandler(handlers.NewCommand("deluser", adminOnly(cmdDelUser)))
	d.AddHandler(handlers.NewCommand("listusers", adminOnly(cmdListUsers)))

	d.AddHandler(handlers.NewCommand("addcourse", adminOnly(cmdAddCourse)))
	d.AddHandler(handlers.NewCommand("delcourse", adminOnly(cmdDelCourse)))
	d.AddHandler(handlers.NewCommand("listcourses", adminOnly(cmdListCourses)))
	d.AddHandler(handlers.NewCommand("setdlimit", adminOnly(cmdSetDLimit)))

	d.AddHandler(handlers.NewCommand("grantaccess", adminOnly(cmdGrantAccess)))
	d.AddHandler(handlers.NewCommand("revokeaccess", adminOnly(cmdRevokeAccess)))

	d.AddHandler(handlers.NewCommand("addvideo", adminOnly(cmdAddVideo)))
	d.AddHandler(handlers.NewCommand("syncchannel", adminOnly(cmdSyncChannel)))
	d.AddHandler(handlers.NewCommand("sessions", adminOnly(cmdSessions)))
	d.AddHandler(handlers.NewCommand("kill", adminOnly(cmdKillDevice)))
	d.AddHandler(handlers.NewCommand("killsession", adminOnly(cmdKillSession)))
	d.AddHandler(handlers.NewCommand("stats", adminOnly(cmdStats)))

	d.AddHandler(handlers.NewCommand("colabstop", adminOnly(cmdColabStop)))
	d.AddHandler(handlers.NewCommand("colabstart", adminOnly(cmdColabStart)))
	d.AddHandler(handlers.NewCommand("colabstatus", adminOnly(cmdColabStatus)))

	d.AddHandler(handlers.NewCommand("regopen", adminOnly(cmdRegOpen)))
	d.AddHandler(handlers.NewCommand("regclose", adminOnly(cmdRegClose)))
	d.AddHandler(handlers.NewCommand("regstatus", adminOnly(cmdRegStatus)))

	d.AddHandler(handlers.NewCommand("offlinewmon", adminOnly(cmdOfflineWmOn)))
	d.AddHandler(handlers.NewCommand("offlinewmoff", adminOnly(cmdOfflineWmOff)))
	d.AddHandler(handlers.NewCommand("offlinewmstatus", adminOnly(cmdOfflineWmStatus)))
}

// ─── adminOnly wrapper ────────────────────────────────────────────────────────

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
	// Button-menu (menu.go) मधून call झाला असेल (callback query) तर message
	// edit करून back/refresh buttons attach करतो — normal /command flow
	// साठी काहीच बदलत नाही.
	if u.CallbackQuery != nil {
		answerCallbackText(ctx, u, text)
		return
	}
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

	_ = db.SetUserActive(user.ID, false)
	_ = db.DeleteAllSessionsForUser(user.ID)

	reply(ctx, u, fmt.Sprintf("✅ User @%s disabled and sessions killed.", user.Username))
	return dispatcher.EndGroups
}

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

func cmdAddCourse(ctx *ext.Context, u *ext.Update) error {
	text := u.EffectiveMessage.Text
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

	// Channel madhe userbot admin/owner असेल tar sagle bots (main bot + worker
	// bots) automatic admin karaychा प्रयत्न background madhe होतो. Userbot tya
	// channel madhe admin nasel tar fakt channel ID log madhe print hoईल.
	if SyncChannelAdmins != nil {
		go SyncChannelAdmins(channelID)
	}

	reply(ctx, u, fmt.Sprintf(
		"✅ Course created\n\nTitle: %s\nID: %s\nPrice: %s\nChannel: %d\n\nMake sure the bot is admin in that channel.\nThen use /syncchannel %s to index existing videos.",
		course.Title, course.ID.Hex(), course.Price, course.ChannelID, course.ID.Hex(),
	))
	return dispatcher.EndGroups
}

// ── /setdlimit <course_id> <limit> ────────────────────────────────────────────
// Course साठी per-user daily offline download limit set करतो.
// Usage: /setdlimit 507f1f77bcf86cd799439011 5
//        /setdlimit 507f1f77bcf86cd799439011 0   ← 0 = default (3) वापर

func cmdSetDLimit(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u,
			"Usage: `/setdlimit <course_id> <limit>`\n\n"+
				"Examples:\n"+
				"`/setdlimit 507f1f77bcf86cd799439011 5` — 5 downloads/day\n"+
				"`/setdlimit 507f1f77bcf86cd799439011 1` — 1 download/day\n"+
				"`/setdlimit 507f1f77bcf86cd799439011 0` — default (3) वर reset\n\n"+
				"Course IDs बघायला: /listcourses")
		return dispatcher.EndGroups
	}

	courseObjID, err := primitive.ObjectIDFromHex(strings.TrimSpace(a[0]))
	if err != nil {
		reply(ctx, u, "❌ Invalid course ID. /listcourses वापरा.")
		return dispatcher.EndGroups
	}

	var limit int
	if _, err := fmt.Sscanf(a[1], "%d", &limit); err != nil || limit < 0 {
		reply(ctx, u, "❌ Limit एक positive number असायला हवे (0 = default 3).")
		return dispatcher.EndGroups
	}

	course, err := db.GetCourseByID(courseObjID)
	if err != nil || course == nil {
		reply(ctx, u, "❌ Course सापडला नाही.")
		return dispatcher.EndGroups
	}

	if err := db.UpdateCourse(courseObjID, bson.M{"download_limit": limit}); err != nil {
		reply(ctx, u, "❌ DB update error: "+err.Error())
		return dispatcher.EndGroups
	}

	displayLimit := limit
	if displayLimit == 0 {
		displayLimit = 3
	}
	reply(ctx, u, fmt.Sprintf(
		"✅ Download limit updated!\n\n"+
			"📚 Course: %s\n"+
			"⬇️ Daily Limit: %d downloads/user%s",
		course.Title, displayLimit,
		func() string {
			if limit == 0 {
				return " (default)"
			}
			return ""
		}(),
	))
	return dispatcher.EndGroups
}

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
	_ = db.DeleteCachedFilesForCourse(courseID)

	reply(ctx, u, "✅ Course deleted and cached files removed.")
	return dispatcher.EndGroups
}

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

	go func() {
		if user.Email == "" {
			return
		}
		appName := os.Getenv("BREVO_FROM_NAME")
		if appName == "" {
			appName = "ONLINE COURSE"
		}
		_ = email.SendAccessGrantedEmail(user.Email, user.FullName, course.Title, expiryMsg, appName)
	}()

	reply(ctx, u, fmt.Sprintf(
		"✅ Access granted\n\nUser: %s\nCourse: %s\nExpiry: %s",
		user.Username, course.Title, expiryMsg,
	))
	return dispatcher.EndGroups
}

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
// ADD VIDEO — manually insert missing message IDs into a course
// ═══════════════════════════════════════════════════════════════════════════════

// /addvideo <course_id> <start_msg_id> <end_msg_id> <caption>
//
// syncchannel media नसलेले messages skip करतो.
// addvideo त्या skipped IDs साठी manually record insert करतो.
//
// - MongoDB मध्ये (message_id + channel_id) आधीच असेल → skip
// - नसेल → insert: caption + message_id as title, video/mp4 defaults
// - दुसऱ्या channel च्या duplicate key error → skip
//
// Example:
//   /addvideo 507f1f... 10 50 Python Basics - Lecture
//   → inserts missing IDs with titles: "Python Basics - Lecture 10", "...11" …
func cmdAddVideo(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 4 {
		reply(ctx, u, "Usage: /addvideo <course_id> <start_msg_id> <end_msg_id> <caption>\n\nExample:\n/addvideo 507f1f... 10 50 Python Basics - Lecture")
		return dispatcher.EndGroups
	}

	courseID, err := primitive.ObjectIDFromHex(a[0])
	if err != nil {
		reply(ctx, u, "❌ Invalid course ID.")
		return dispatcher.EndGroups
	}

	startID, err1 := strconv.Atoi(a[1])
	endID, err2 := strconv.Atoi(a[2])
	if err1 != nil || err2 != nil || startID < 1 || endID < startID {
		reply(ctx, u, "❌ Invalid range. Both must be positive integers and start ≤ end.")
		return dispatcher.EndGroups
	}

	baseCaption := strings.TrimSpace(strings.Join(a[3:], " "))
	if baseCaption == "" {
		reply(ctx, u, "❌ Caption cannot be empty.")
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

	inserted := 0
	skipped := 0

	for id := startID; id <= endID; id++ {
		// या course च्या channel मध्ये आधीच आहे → skip, touch नाही
		existing, _ := db.GetCachedFile(id, course.ChannelID)
		if existing != nil {
			skipped++
			continue
		}

		// Caption = owner दिलेला text + message_id as index
		caption := fmt.Sprintf("%s %d", baseCaption, id)

		cached := &db.CachedFile{
			MessageID: id,
			ChannelID: course.ChannelID,
			CourseID:  courseID,
			Title:     caption,
			Caption:   caption,
			FileName:  fmt.Sprintf("video_%d.mp4", id),
			MimeType:  "video/mp4",
			FileSize:  0,
			SortOrder: id,
			CreatedAt: time.Now(),
		}

		if err := db.UpsertCachedFile(cached); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				// message_id unique index — दुसऱ्या channel मध्ये conflict → skip
				skipped++
				continue
			}
			reply(ctx, u, fmt.Sprintf("❌ Failed at ID %d: %s", id, err.Error()))
			return dispatcher.EndGroups
		}
		inserted++
	}

	reply(ctx, u, fmt.Sprintf(
		"✅ Done!\n\nCourse: %s\nChannel: %d\nRange: %d → %d\nCaption prefix: %s\n\n📥 Inserted: %d\n⏭ Skipped (already cached): %d",
		course.Title, course.ChannelID, startID, endID, baseCaption, inserted, skipped,
	))
	return dispatcher.EndGroups
}

// ═══════════════════════════════════════════════════════════════════════════════
// CHANNEL SYNC
// ═══════════════════════════════════════════════════════════════════════════════

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

	botClient := MainBotClient
	channel, err := utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, course.ChannelID)
	if err != nil {
		reply(ctx, u, "❌ Cannot access course channel: "+err.Error()+"\n\nMake sure the bot is admin in that channel.")
		return dispatcher.EndGroups
	}

	const (
		batchSize    = 100
		breakEvery   = 100
		breakSeconds = 10
	)

	total := 0
	skipped := 0
	sinceBreak := 0

	for batchStart := startMsgID; batchStart <= endMsgID; batchStart += batchSize {

		batchEnd := batchStart + batchSize - 1
		if batchEnd > endMsgID {
			batchEnd = endMsgID
		}

		ids := make([]tg.InputMessageClass, 0, batchEnd-batchStart+1)
		for id := batchStart; id <= batchEnd; id++ {
			ids = append(ids, &tg.InputMessageID{ID: id})
		}

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
			msg, ok := rawMsg.(*tg.Message)
			if !ok || msg.Media == nil {
				skipped++
				continue
			}

			file, err := utils.FileFromMedia(msg.Media)
			if err != nil {
				skipped++
				continue
			}

			caption := strings.TrimSpace(msg.Message)

			title := caption
			if title == "" {
				title = stripExt(file.FileName)
			}

			cached := &db.CachedFile{
				MessageID: msg.ID,
				ChannelID: course.ChannelID,
				CourseID:  courseID,
				Title:     title,
				Caption:   caption,
				FileName:  file.FileName,
				MimeType:  file.MimeType,
				FileSize:  file.FileSize,
				SortOrder: msg.ID,
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
			s.Token[max(0, len(s.Token)-8):],
		))
	}
	sb.WriteString("Use /killsession <username> to force logout.")
	reply(ctx, u, sb.String())
	return dispatcher.EndGroups
}

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

// cmdKillDevice — /kill <mobile>
// Device-lock fingerprint clear karto + active session pn kill karto, jaymule
// user kuthlyahi navin device varun fresh login karu shakto. (cmdKillSession
// pasun veglं — te fakt session kill karte, fingerprint touch karat nahi.)
func cmdKillDevice(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 1 {
		reply(ctx, u, "Usage: /kill <mobile_number>\n\nYa command ne user cha device-lock (fingerprint) + active session clear hoто, mhanun to navin device varun fresh login karu shakto.")
		return dispatcher.EndGroups
	}

	mobile := strings.TrimSpace(a[0])
	user, err := db.GetUserByMobileOrEmail(mobile)
	if err != nil || user == nil {
		reply(ctx, u, "❌ User not found with mobile: "+mobile)
		return dispatcher.EndGroups
	}

	if err := db.ClearUserDevice(user.ID); err != nil {
		reply(ctx, u, "❌ "+err.Error())
		return dispatcher.EndGroups
	}
	_ = db.DeleteAllSessionsForUser(user.ID)

	reply(ctx, u, fmt.Sprintf(
		"✅ Device-lock cleared for %s (%s).\nUser navin device varun fresh login karu shakto.",
		user.FullName, mobile,
	))
	return dispatcher.EndGroups
}

// ═══════════════════════════════════════════════════════════════════════════════
// STATS
// ═══════════════════════════════════════════════════════════════════════════════

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

// ─── Website Registration Open/Close ──────────────────────────────────────────
// /register (website POST) create-account path db.SettingRegistrationOpen
// check karto (internal/auth/auth.go, DoRegister function, sagळ्यat var) —
// ha check UI/form bypass karun direct API call kela tarihi honarach, karan
// to server-side, account create honya AADHI ahe. Ithe फक्त tya flag la
// admin साठी Telegram bot madhun ON/OFF karnyacha button dila ahe.

func cmdRegOpen(ctx *ext.Context, u *ext.Update) error {
	if err := db.SetSettingBool(db.SettingRegistrationOpen, true); err != nil {
		reply(ctx, u, "❌ Registration ON karta ala nahi: "+err.Error())
		return dispatcher.EndGroups
	}
	reply(ctx, u,
		"✅ *Registration ON kela*\n\n"+
			"आता website च्या `/register` page वरून कोणीही नवीन account बनवू शकेल.\n\n"+
			"बंद करण्यासाठी: `/regclose`")
	return dispatcher.EndGroups
}

func cmdRegClose(ctx *ext.Context, u *ext.Update) error {
	if err := db.SetSettingBool(db.SettingRegistrationOpen, false); err != nil {
		reply(ctx, u, "❌ Registration OFF karta ala nahi: "+err.Error())
		return dispatcher.EndGroups
	}
	reply(ctx, u,
		"⛔ *Registration OFF kela*\n\n"+
			"आतापासून website वर नवीन कोणीही signup करू शकणार नाही — "+
			"form बायपास करून direct API call केला तरीही (server-side block).\n\n"+
			"Existing users चा login/access यावर काहीच परिणाम होणार नाही.\n\n"+
			"परत सुरू करण्यासाठी: `/regopen`")
	return dispatcher.EndGroups
}

func cmdRegStatus(ctx *ext.Context, u *ext.Update) error {
	open := db.GetSettingBool(db.SettingRegistrationOpen, true)
	if open {
		reply(ctx, u, "🟢 Registration सध्या *ON* आहे — नवीन users signup करू शकतात.\n\nबंद करण्यासाठी: `/regclose`")
	} else {
		reply(ctx, u, "🔴 Registration सध्या *OFF* आहे — नवीन signup ब्लॉक्ड आहे.\n\nसुरू करण्यासाठी: `/regopen`")
	}
	return dispatcher.EndGroups
}

// ─── Offline Download — Video Watermark ON/OFF ────────────────────────────────
// ON  → offline video: download → user चा mobile/email watermark burn → reupload
//       (जुनी, default behaviour — deliverWatermarkedVideo वापरतो, start.go मध्ये).
// OFF → offline video थेट copy-forward होतो (photo/PDF सारखं) — कोणताही
//       download/FFmpeg/re-upload होत नाही, त्यामुळे जलद पण watermark शिवाय.

func cmdOfflineWmOn(ctx *ext.Context, u *ext.Update) error {
	if err := db.SetSettingBool(db.SettingOfflineWatermarkEnabled, true); err != nil {
		reply(ctx, u, "❌ Watermark ON करता आला नाही: "+err.Error())
		return dispatcher.EndGroups
	}
	reply(ctx, u,
		"✅ *Offline Video Watermark ON केलं*\n\n"+
			"आतापासून offline downloads मध्ये user चा mobile/email number video वर burn होऊन जाईल.\n\n"+
			"बंद करण्यासाठी: `/offlinewmoff`")
	return dispatcher.EndGroups
}

func cmdOfflineWmOff(ctx *ext.Context, u *ext.Update) error {
	if err := db.SetSettingBool(db.SettingOfflineWatermarkEnabled, false); err != nil {
		reply(ctx, u, "❌ Watermark OFF करता आला नाही: "+err.Error())
		return dispatcher.EndGroups
	}
	reply(ctx, u,
		"⛔ *Offline Video Watermark OFF केलं*\n\n"+
			"आतापासून offline video downloads user ला direct पाठवले जातील — "+
			"कोणताही watermark burn होणार नाही (जलद delivery, पण unprotected).\n\n"+
			"परत सुरू करण्यासाठी: `/offlinewmon`")
	return dispatcher.EndGroups
}

func cmdOfflineWmStatus(ctx *ext.Context, u *ext.Update) error {
	on := db.GetSettingBool(db.SettingOfflineWatermarkEnabled, true)
	if on {
		reply(ctx, u, "🟢 Offline Video Watermark सध्या *ON* आहे — videos वर mobile/email burn होतो.\n\nबंद करण्यासाठी: `/offlinewmoff`")
	} else {
		reply(ctx, u, "🔴 Offline Video Watermark सध्या *OFF* आहे — videos direct (without watermark) पाठवले जातात.\n\nसुरू करण्यासाठी: `/offlinewmon`")
	}
	return dispatcher.EndGroups
}

// ─── Go 1.21 min/max compat ───────────────────────────────────────────────────

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ─── Colab Worker Control ─────────────────────────────────────────────────────

func cmdColabStop(ctx *ext.Context, u *ext.Update) error {
	wmqueue.RequestColabStop()
	reply(ctx, u,
		"🛑 Colab stop request पाठवला\n\n"+
			"Colab worker पुढच्या poll वर हे बघेल आणि चालू job पूर्ण झाल्यावर "+
			"loop graceful बंद करेल — नवीन jobs घेणार नाही.\n\n"+
			"परत सुरू करण्यासाठी: `/colabstart`")
	return dispatcher.EndGroups
}

func cmdColabStart(ctx *ext.Context, u *ext.Update) error {
	wmqueue.ClearColabStop()
	reply(ctx, u,
		"✅ Colab stop request मागे घेतला\n\n"+
			"Colab worker (notebook) चालू असेल तर पुढच्या poll वर परत jobs घेईल. "+
			"जर Colab बंद/restart झालं असेल तर notebook पुन्हा run करा.")
	return dispatcher.EndGroups
}

func cmdColabStatus(ctx *ext.Context, u *ext.Update) error {
	online := wmqueue.IsColabOnline()
	lastPoll := wmqueue.LastPollAgo()
	stopReq := wmqueue.IsColabStopRequested()
	processing := wmqueue.Default.ProcessingJobs()
	activeWorkers := wmqueue.ActiveWorkers()

	status := "🔴 Offline"
	if online {
		status = "🟢 Online"
	}
	stopStatus := "❌ नाही"
	if stopReq {
		stopStatus = "🛑 हो (stop pending)"
	}

	msg := fmt.Sprintf(
		"☁️ Colab Status\n\n"+
			"स्थिती: %s\n"+
			"Active workers: %d\n"+
			"शेवटचा poll: %s पूर्वी\n"+
			"Stop request: %s\n"+
			"सध्या processing: %d job(s)",
		status, len(activeWorkers), lastPoll, stopStatus, len(processing),
	)
	for _, id := range activeWorkers {
		idShort := id
		if len(idShort) > 8 {
			idShort = idShort[:8]
		}
		msg += fmt.Sprintf("\n  • worker %s", idShort)
	}
	for _, job := range processing {
		workerShort := job.WorkerID
		if len(workerShort) > 8 {
			workerShort = workerShort[:8]
		}
		msg += fmt.Sprintf("\n  • %s — %s (worker %s)", job.JobID, job.FileName, workerShort)
	}
	reply(ctx, u, msg)
	return dispatcher.EndGroups
}

// ─── Auto-append new uploads to channel cache ─────────────────────────────────

func AutoCacheNewFile(messageID int, channelID int64, courseID primitive.ObjectID, caption, fileName, mimeType string, fileSize int64) error {
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

// ── /cmd ──────────────────────────────────────────────────────────────────────
// सर्व admin commands एका जागी दाखवतो.

func cmdHelp(ctx *ext.Context, u *ext.Update) error {
	msg := `👑 *Admin Commands*

👤 *Users*
/adduser \<mobile\> \<email\> \<name\> — नवीन user add करा
/deluser \<mobile\_or\_email\> — user delete करा
/listusers — सर्व users list
/userinfo \<mobile\_or\_email\> — user चा पूर्ण info
/settg \<mobile\_or\_email\> \<telegram\_id\> — Telegram ID link करा
/unlinktg \<mobile\_or\_email\> — ⚠️ emergency: Telegram ID unlink करा \(support साठीच\)
/blockuser \<identifier\> \<days\> \<reason\> — user block करा
/unblockuser \<identifier\> — user unblock करा
/suspicious — suspicious users list

📚 *Courses*
/addcourse \<channel\_id\> \<title\> — नवीन course add करा
/delcourse \<course\_id\> — course delete करा
/listcourses — सर्व courses व त्यांचे IDs
/setdlimit \<course\_id\> \<limit\> — daily download limit set करा \(0 \= default 3\)
/regopen — website registration ON करा
/regclose — website registration OFF करा
/regstatus — registration सध्या ON/OFF आहे का ते बघा
/offlinewmon — offline video watermark ON करा
/offlinewmoff — offline video watermark OFF करा \(video direct, without watermark, पाठवला जाईल\)
/offlinewmstatus — offline watermark सध्या ON/OFF आहे का ते बघा

🔑 *Access*
/grantaccess \<identifier\> \<course\_id\> \[months\] — user ला access द्या
/revokeaccess \<identifier\> \<course\_id\> — access काढा

🎬 *Videos / Channel*
/addvideo — reply to forwarded msg — video manually add करा
/syncchannel \<channel\_id\> — channel चे सर्व videos sync करा

📱 *Sessions / Devices*
/sessions — active sessions list
/kill \<device\_id\> — device kill करा
/killsession \<session\_id\> — session kill करा

☁️ *Colab*
/colabstart — Colab worker start करा
/colabstop — Colab worker stop करा
/colabstatus — Colab status बघा

📊 *Info*
/stats — bot stats \(users, courses, downloads\)
/myid — तुमचा Telegram ID
/cmd — हे menu`

	reply(ctx, u, msg)
	return dispatcher.EndGroups
}
