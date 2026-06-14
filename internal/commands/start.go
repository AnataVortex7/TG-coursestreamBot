package commands

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"
	"fmt"
	"math/rand"
	"strings"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/storage"
	"github.com/gotd/td/tg"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func (m *command) LoadStart(dispatcher dispatcher.Dispatcher) {
	log := m.log.Named("start")
	defer log.Sugar().Info("Loaded")
	dispatcher.AddHandler(handlers.NewCommand("start", start))
}

func start(ctx *ext.Context, u *ext.Update) error {
	chatId := u.EffectiveChat().GetID()
	peerChatId := ctx.PeerStorage.GetPeerById(chatId)
	if peerChatId.Type != int(storage.TypeUser) {
		return dispatcher.EndGroups
	}
	// ── Deep Link: /start dl_48271 ────────────────────────────────────────────
	// Offline-token users are authorized via DB (/settg), so skip AllowedUsers check for this flow.
	a := args(u)
	if len(a) > 0 && strings.HasPrefix(a[0], "dl_") {
		token := strings.TrimSpace(strings.TrimPrefix(a[0], "dl_"))
		return processDLToken(ctx, u, chatId, token)
	}

	// ── Deep Link: /start ct_<userIDHex> ──────────────────────────────────────
	// Website "Contact via Telegram" button वरून आलेला user.
	// AllowedUsers check skip करतो — relay.go कडे आधीच forward होईल.
	if len(a) > 0 && strings.HasPrefix(a[0], "ct_") {
		return handleContactDeepLink(ctx, u, chatId, strings.TrimPrefix(a[0], "ct_"))
	}

	if len(config.ValueOf.AllowedUsers) != 0 && !utils.Contains(config.ValueOf.AllowedUsers, chatId) {
		ctx.Reply(u, ext.ReplyTextString(
			"👋 Namaskar!\n\nखाली तुमचा message किंवा file पाठवा — ते admin ला जाईल आणि लवकरच reply मिळेल.",
		), nil)
		return dispatcher.EndGroups
	}

	ctx.Reply(u, ext.ReplyTextString(
		"👋 Welcome!\n\n"+
			"📥 Offline Video Download साठी:\n"+
			"Website वर जा → Course उघडा → 📥 Offline button दाबा → Link click करा!\n\n"+
			"⚙️ पहिल्यांदा:\n"+
			"   /register 9876543210\n\n"+
			"❓ Telegram ID: /myid",
	), nil)
	return dispatcher.EndGroups
}

func processDLToken(ctx *ext.Context, u *ext.Update, chatID int64, tokenStr string) error {
	if len(tokenStr) != 5 {
		reply(ctx, u, "❌ Invalid token. Website वरून नवीन link generate करा.")
		return dispatcher.EndGroups
	}

	dbUser, err := db.GetUserByTelegramID(chatID)
	if err != nil || dbUser == nil {
		reply(ctx, u,
			"❌ तुमचा Telegram account link केलेला नाही.\n\n"+
				"एकदा register करा:\n"+
				"`/register <mobile_or_email> "+fmt.Sprintf("%d", chatID)+"`",
		)
		return dispatcher.EndGroups
	}

	blocked, blockedUntil, blockReason := db.IsUserBlocked(dbUser.ID)
	if blocked {
		reply(ctx, u, fmt.Sprintf(
			"⛔ Account suspended.\n📅 Until: %s IST\n📝 Reason: %s",
			blockedUntil.In(istLoc()).Format("02 Jan 2006, 3:04 PM"),
			blockReason,
		))
		return dispatcher.EndGroups
	}

	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot चालू नाही. थोड्या वेळाने पुन्हा प्रयत्न करा.")
		return dispatcher.EndGroups
	}

	// ── Course limit fetch (token मधून courseID मिळवतो) ──────────────────────
	// Token redeem होण्याआधी course limit जाणून घ्यायला
	// token DB मध्ये आहे, तिथून courseID → course → limit
	courseLimit := 0 // RedeemOfflineToken आत default handle होतो
	// (token valid असेल तर courseID मिळेल, नसेल तर redeem fail होईल)
	// Optional: token वरून course fetch करून limit pass करता येतो
	// पण RedeemOfflineToken स्वतः course limit accept करत नाही token stage ला
	// म्हणून इथे 0 पाठवतो — course-specific limit offline.go route मध्ये enforce होतो
	// Token already generate झाला म्हणजे limit check route ने केला आहे

	result, err := db.RedeemOfflineToken(tokenStr, dbUser.ID, courseLimit)
	if err != nil {
		switch err {
		case db.ErrTokenNotFound:
			reply(ctx, u, "❌ Token expire झाला. Website वरून नवीन link घ्या.")
		case db.ErrTokenWrongUser:
			reply(ctx, u, "❌ हा token तुमच्यासाठी नाही.")
		case db.ErrDailyLimitReached:
			reply(ctx, u, "⛔ या course साठी आजचे downloads संपले!\nरात्री 12:00 IST ला reset होतात.")
		default:
			reply(ctx, u, "❌ Error: "+err.Error())
		}
		return dispatcher.EndGroups
	}

	// ── Course limit Telegram message मध्ये दाखवण्यासाठी fetch ───────────────
	course, _ := db.GetCourseByID(result.CourseID)
	courseLimit = 3
	if course != nil && course.DownloadLimit > 0 {
		courseLimit = course.DownloadLimit
	}

	hasAccess, err := db.HasAccess(dbUser.ID, result.CourseID)
	if err != nil || !hasAccess {
		reply(ctx, u, "⛔ या course साठी access नाही किंवा expire झाला.")
		return dispatcher.EndGroups
	}

	sentMsg, _ := ctx.Reply(u, ext.ReplyTextString("⏳ Video पाठवत आहे..."), nil)

	channelPeer, err := utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, result.ChannelID)
	if err != nil {
		reply(ctx, u, "❌ Channel access error: "+err.Error())
		return dispatcher.EndGroups
	}

	res, err := botClient.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: channelPeer,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: result.MessageID}},
	})
	if err != nil {
		reply(ctx, u, "❌ Message fetch error: "+err.Error())
		return dispatcher.EndGroups
	}

	messages, ok := res.(*tg.MessagesChannelMessages)
	if !ok || len(messages.Messages) == 0 {
		reply(ctx, u, "❌ Video सापडला नाही.")
		return dispatcher.EndGroups
	}

	msg, ok := messages.Messages[0].(*tg.Message)
	if !ok || msg.Media == nil {
		reply(ctx, u, "❌ Message मध्ये media नाही.")
		return dispatcher.EndGroups
	}

	var inputMedia tg.InputMediaClass
	switch m := msg.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			reply(ctx, u, "❌ Document read error.")
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
			reply(ctx, u, "❌ Photo read error.")
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

	toPeer := ctx.PeerStorage.GetInputPeerById(chatID)
	caption := fmt.Sprintf(
		"📥 Offline Download\n🔒 Forward व Save blocked\n\n📊 आजचे downloads: %d/%d",
		result.UsedToday, courseLimit,
	)

	sentMedia, err := botClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:       toPeer,
		Media:      inputMedia,
		Message:    caption,
		Noforwards: true,
		RandomID:   rand.Int63(),
	})
	if err != nil {
		reply(ctx, u, "❌ Video पाठवताना error: "+err.Error())
		return dispatcher.EndGroups
	}

	if updates, ok := sentMedia.(*tg.Updates); ok {
		for _, upd := range updates.Updates {
			if m, ok := upd.(*tg.UpdateMessageID); ok {
				_ = db.ScheduleDelete(chatID, m.ID)
				break
			}
		}
	}

	shouldAlert, score, _ := db.RecordDownload(dbUser.ID, result.UsedToday)
	if shouldAlert {
		go sendAdminAlert(ctx, dbUser, score)
	}

	if sentMsg != nil {
		_ = ctx.DeleteMessages(chatID, []int{sentMsg.ID})
	}

	return dispatcher.EndGroups
}

// handleContactDeepLink — /start ct_<userIDHex>
// Website "Contact via Telegram" button वरून आलेला user.
// Admin ला website account info सोबत notify करतो, user ला reply करतो.
// पुढचा कोणताही message relay.go कडून automatically admin ला forward होईल.
func handleContactDeepLink(ctx *ext.Context, u *ext.Update, chatID int64, userIDHex string) error {
	tgUser := u.EffectiveUser()
	name := tgUser.FirstName
	if tgUser.LastName != "" {
		name += " " + tgUser.LastName
	}
	if tgUser.Username != "" {
		name += " (@" + tgUser.Username + ")"
	}

	// Website account details (असल्यास)
	siteInfo := ""
	if objID, err := primitive.ObjectIDFromHex(userIDHex); err == nil {
		if siteUser, err := db.GetUserByID(objID); err == nil && siteUser != nil {
			siteInfo = fmt.Sprintf(
				"\n👤 Website Account: %s\n📧 Email: %s\n📱 Mobile: %s",
				siteUser.FullName, siteUser.Email, siteUser.Mobile,
			)
		}
	}

	// Admin ला notify करतो
	botClient := MainBotClient
	if botClient != nil {
		header := fmt.Sprintf(
			"🌐 New Contact Request (via Website)\n👤 Telegram: %s\nTG ID: %d%s\n\n👇 User चा message खाली येईल — reply करायला त्या message ला reply करा.",
			name, chatID, siteInfo,
		)
		for _, adminID := range getAdminIDs() {
			adminPeer := ctx.PeerStorage.GetInputPeerById(adminID)
			if adminPeer == nil {
				continue
			}
			_, _ = botClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer:     adminPeer,
				Message:  header,
				RandomID: rand.Int63(),
			})
		}
	}

	ctx.Reply(u, ext.ReplyTextString(
		"👋 Namaskar!\n\nखाली तुमचा message किंवा file पाठवा — ते थेट admin ला जाईल आणि लवकरच reply मिळेल.",
	), nil)
	return dispatcher.EndGroups
}
