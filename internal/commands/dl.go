// internal/commands/dl.go
//
// /dl <TOKEN>
//
// User copies a 5-digit token from the website course page and sends
// this command in any Telegram chat with the bot.
//
// The bot:
//   1. Validates the token and ownership (token is bound to the website user,
//      but since the bot has no website<→TG link, it uses the Telegram chat ID
//      stored in the token — see note below)
//   2. Re-checks course access in DB (access may have been revoked since token was issued)
//   3. Enforces the daily limit of 5 downloads (per website user, resets midnight IST)
//   4. Fetches the video from Telegram and sends it with NoForwards=true
//      (forward blocked, save to gallery blocked)
//
// ── NOTE on identity ─────────────────────────────────────────────────────────
// The token is generated on the website where the user is authenticated via
// their DB User record (primitive.ObjectID).  The bot knows only the Telegram
// chat ID.  To bridge this, CreateOfflineToken stores the DB UserID.
// When the bot receives /dl, it must resolve the Telegram chat ID → DB UserID.
// This requires a TelegramID field on the User struct OR a lookup via sessions.
//
// SIMPLEST approach (used here): the website stores TelegramID on the User
// record (optional field — user links their TG account once via /linkbot command
// or admin sets it via /settg <user_id> <tg_id>).
//
// If TelegramID is not linked yet, the bot asks the user to link first.
// See LoadSetTG below for the admin command to link.

package commands

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/storage"
	"github.com/gotd/td/tg"
)

// LoadDL registers /dl and /settg commands.
func (m *command) LoadDL(d dispatcher.Dispatcher) {
	log := m.log.Named("dl")
	defer log.Sugar().Info("Loaded /dl command")
	d.AddHandler(handlers.NewCommand("dl", cmdDL))
	d.AddHandler(handlers.NewCommand("settg", adminOnly(cmdSetTG)))
}

// ── /dl <TOKEN> ───────────────────────────────────────────────────────────────

func cmdDL(ctx *ext.Context, u *ext.Update) error {
	// Only in private chat
	chatID := u.EffectiveChat().GetID()
	peerInfo := ctx.PeerStorage.GetPeerById(chatID)
	if peerInfo.Type != int(storage.TypeUser) {
		reply(ctx, u, "⚠️ /dl command फक्त bot सोबत private chat मध्ये वापरा.")
		return dispatcher.EndGroups
	}

	a := args(u)
	if len(a) < 1 {
		reply(ctx, u,
			"📥 *Offline Video Download*\n\n"+
				"Usage: `/dl <5-digit token>`\n\n"+
				"Token कसा मिळवायचा:\n"+
				"1. Website वर course उघडा\n"+
				"2. Video च्या खाली 📥 *Offline* button दाबा\n"+
				"3. मिळालेला token इथे `/dl XXXXX` असा टाका\n\n"+
				"⚠️ रोज फक्त 3 downloads — midnight IST ला reset होतात.\n"+
				"⚠️ पाठवलेला video forward किंवा save करता येणार नाही.",
		)
		return dispatcher.EndGroups
	}

	tokenStr := strings.TrimSpace(a[0])
	if len(tokenStr) != 5 {
		reply(ctx, u, "❌ Token 5 digits चा असणे आवश्यक आहे. उदाहरण: `/dl 48271`")
		return dispatcher.EndGroups
	}

	// Resolve Telegram chat ID → DB User
	dbUser, err := db.GetUserByTelegramID(chatID)
	if err != nil || dbUser == nil {
		reply(ctx, u,
			"❌ तुमचा Telegram account bot शी link केलेला नाही.\n\n"+
				"cmd taka: `/register <your_mobile_or_email> "+fmt.Sprintf("%d", chatID)+"`\n\n"+
				"एकदा link झाल्यावर /dl command वापरता येईल.")
		return dispatcher.EndGroups
	}

	// ── Block check ───────────────────────────────────────────────────────────
	blocked, blockedUntil, blockReason := db.IsUserBlocked(dbUser.ID)
	if blocked {
		reply(ctx, u, fmt.Sprintf(
			"⛔ तुमचा account suspended आहे.\n\n"+
				"📅 Until: %s IST\n"+
				"📝 Reason: %s\n\n"+
				"जास्त माहितीसाठी admin शी संपर्क करा.",
			blockedUntil.In(istLoc()).Format("02 Jan 2006, 3:04 PM"),
			blockReason,
		))
		return dispatcher.EndGroups
	}

	// Bot client check
	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client चालू नाही. थोड्या वेळाने पुन्हा प्रयत्न करा.")
		return dispatcher.EndGroups
	}

	// Redeem token (validates ownership, increments daily counter, returns file info)
	result, err := db.RedeemOfflineToken(tokenStr, dbUser.ID)
	if err != nil {
		switch err {
		case db.ErrTokenNotFound:
			reply(ctx, u, "❌ Token सापडला नाही किंवा expire झाला.\nनवीन token website वरून generate करा.")
		case db.ErrTokenWrongUser:
			reply(ctx, u, "❌ हा token तुमच्यासाठी नाही.")
		case db.ErrDailyLimitReached:
			reply(ctx, u, "⛔ आजचे 5 downloads संपले!\n\nReset होईल: midnight IST (रात्री 12:00)\nउद्या पुन्हा download करा.")
		default:
			reply(ctx, u, "❌ Error: "+err.Error())
		}
		return dispatcher.EndGroups
	}

	// Re-check course access (access may have been revoked after token was generated)
	hasAccess, err := db.HasAccess(dbUser.ID, result.CourseID)
	if err != nil || !hasAccess {
		reply(ctx, u, "⛔ या course साठी तुमचा access नाही किंवा expire झाला.")
		return dispatcher.EndGroups
	}

	// Show "sending…" message
	sentMsg, _ := ctx.Reply(u, ext.ReplyTextString("⏳ Video पाठवत आहे..."), nil)

	// Resolve channel peer
	channelPeer, err := utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, result.ChannelID)
	if err != nil {
		reply(ctx, u, "❌ Channel access करता आला नाही: "+err.Error())
		return dispatcher.EndGroups
	}

	// Fetch the message from channel
	res, err := botClient.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: channelPeer,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: result.MessageID}},
	})
	if err != nil {
		reply(ctx, u, "❌ Message fetch करता आला नाही: "+err.Error())
		return dispatcher.EndGroups
	}

	messages, ok := res.(*tg.MessagesChannelMessages)
	if !ok || len(messages.Messages) == 0 {
		reply(ctx, u, "❌ Video message सापडला नाही.")
		return dispatcher.EndGroups
	}

	msg, ok := messages.Messages[0].(*tg.Message)
	if !ok || msg.Media == nil {
		reply(ctx, u, "❌ Message मध्ये media नाही.")
		return dispatcher.EndGroups
	}

	// Build InputMedia
	var inputMedia tg.InputMediaClass
	switch m := msg.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			reply(ctx, u, "❌ Document read करता आला नाही.")
			return dispatcher.EndGroups
		}
		inputMedia = &tg.InputMediaDocument{
			ID: &tg.InputDocument{
				ID:            doc.ID,
				AccessHash:    doc.AccessHash,
				FileReference: doc.FileReference,
			},
		}
	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			reply(ctx, u, "❌ Photo read करता आला नाही.")
			return dispatcher.EndGroups
		}
		inputMedia = &tg.InputMediaPhoto{
			ID: &tg.InputPhoto{
				ID:            photo.ID,
				AccessHash:    photo.AccessHash,
				FileReference: photo.FileReference,
			},
		}
	default:
		reply(ctx, u, "❌ हा media type supported नाही.")
		return dispatcher.EndGroups
	}

	// Get daily usage for footer message
	usedToday, _ := db.GetDailyUsageCount(dbUser.ID)

	// Send video with forward + save protection
	toPeer := ctx.PeerStorage.GetInputPeerById(chatID)
	caption := fmt.Sprintf(
		"📥 Offline Download\n🔒 Forward व Save blocked\n\n📊 आजचे downloads: %d/5",
		usedToday,
	)

	sentMedia, err := botClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:       toPeer,
		Media:      inputMedia,
		Message:    caption,
		Noforwards: true, // ← forward blocked + save blocked
		RandomID:   rand.Int63(),
	})
	if err != nil {
		reply(ctx, u, "❌ Video पाठवताना error: "+err.Error())
		return dispatcher.EndGroups
	}

	// ── Auto-delete schedule (48 hours नंतर) ─────────────────────────────────
	if updates, ok := sentMedia.(*tg.Updates); ok {
		for _, upd := range updates.Updates {
			if msg, ok := upd.(*tg.UpdateMessageID); ok {
				_ = db.ScheduleDelete(chatID, msg.ID)
				break
			}
		}
	}

	// ── Suspicious activity track ─────────────────────────────────────────────
	shouldAlert, score, _ := db.RecordDownload(dbUser.ID, usedToday)
	if shouldAlert {
		// Admin ला alert पाठवा
		go sendAdminAlert(ctx, dbUser, score)
	}

	// Delete the "sending…" placeholder if it was sent
	if sentMsg != nil {
		_ = ctx.DeleteMessages(chatID, []int{sentMsg.ID})
	}

	return dispatcher.EndGroups
}

// sendAdminAlert — suspicious user बद्दल admin ला Telegram message पाठवतो.
func sendAdminAlert(ctx *ext.Context, user *db.User, score int) {
	botClient := MainBotClient
	if botClient == nil {
		return
	}

	usedToday, _ := db.GetDailyUsageCount(user.ID)
	guard, _ := db.GetGuard(user.ID)
	consecutive := 0
	if guard != nil {
		consecutive = guard.ConsecutiveFullDays
	}

	msg := fmt.Sprintf(
		"🚨 *Suspicious Activity Alert*\n\n"+
			"👤 User: `%s`\n"+
			"📱 Mobile: %s\n"+
			"📧 Email: %s\n"+
			"📊 Suspicion Score: *%d* / 15\n"+
			"📅 Consecutive Full Days: %d\n"+
			"⬇️ Downloads Today: %d / 5\n\n"+
			"Block करायचे असल्यास:\n"+
			"`/blockuser %s 10 Suspicious activity`",
		user.Username, user.Mobile, user.Email,
		score, consecutive, usedToday,
		func() string {
			if user.Mobile != "" {
				return user.Mobile
			}
			return user.Email
		}(),
	)

	// AllowedUsers[0] = admin
	adminIDs := getAdminIDs()
	for _, adminID := range adminIDs {
		adminPeer := ctx.PeerStorage.GetInputPeerById(adminID)
		if adminPeer == nil {
			continue
		}
		_, _ = botClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     adminPeer,
			Message:  msg,
			RandomID: rand.Int63(),
		})
	}
}

func istLoc() *time.Location {
	loc, _ := time.LoadLocation("Asia/Kolkata")
	if loc == nil {
		return time.FixedZone("IST", 5*60*60+30*60)
	}
	return loc
}

func getAdminIDs() []int64 {
	// AllowedUsers = admin IDs (config मधून)
	// commands package मधून config import करणे avoid करायला,
	// commands.go मध्ये AdminIDs var ठेवता येतो (bot startup ला set होतो).
	// सध्या MainBotClient.Self.ID हा admin आहे असे assume करतो.
	if MainBotClient == nil {
		return nil
	}
	return []int64{MainBotClient.Self.ID}
}

// ── /settg <identifier> <telegram_id> ────────────────────────────────────────
// Admin command: links a website user's DB record to their Telegram ID.
// Usage: /settg 9876543210 987654321
//        /settg user@email.com 987654321

func cmdSetTG(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u,
			"Usage: `/settg <mobile_or_email> <telegram_id>`\n\n"+
				"Example:\n`/settg 9876543210 987654321`\n`/settg user@email.com 987654321`\n\n"+
				"User ला त्यांचा Telegram ID सांगायला सांगा: `/myid` command bot मध्ये.")
		return dispatcher.EndGroups
	}

	identifier := strings.ToLower(strings.TrimSpace(a[0]))

	var tgID int64
	if _, err := fmt.Sscanf(a[1], "%d", &tgID); err != nil || tgID < 1 {
		reply(ctx, u, "❌ Invalid Telegram ID. Positive number असायला हवे.")
		return dispatcher.EndGroups
	}

	user, err := db.GetUserByMobileOrEmail(identifier)
	if err != nil || user == nil {
		reply(ctx, u, "❌ User सापडला नाही: "+identifier)
		return dispatcher.EndGroups
	}

	if err := db.SetUserTelegramID(user.ID, tgID); err != nil {
		reply(ctx, u, "❌ DB update error: "+err.Error())
		return dispatcher.EndGroups
	}

	reply(ctx, u, fmt.Sprintf(
		"✅ Linked!\n👤 User: %s\n📱 Telegram ID: %d\n\nआता user /dl command वापरू शकतो.",
		user.Username, tgID,
	))
	return dispatcher.EndGroups
}

// ── /myid — user ला त्यांचा Telegram ID दाखवतो ─────────────────────────────

func (m *command) LoadMyID(d dispatcher.Dispatcher) {
	d.AddHandler(handlers.NewCommand("myid", func(ctx *ext.Context, u *ext.Update) error {
		chatID := u.EffectiveChat().GetID()
		reply(ctx, u, fmt.Sprintf("तुमचा Telegram ID:\n`%d`\n\nहा ID admin ला द्या /settg साठी.", chatID))
		return dispatcher.EndGroups
	}))
}
