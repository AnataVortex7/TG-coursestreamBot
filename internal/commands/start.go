package commands

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/auth"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"
	"context"
	crand "crypto/rand"
	"fmt"
	"math/big"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"time"

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

	// ── Deep Link: /start totp_<token> ────────────────────────────────────────
	// Website login (OTP via Telegram) साठी. Token वरून कोणता website account
	// आहे ते कळतं → auto-link + लगेच OTP telegram वर पाठवला जातो.
	// AllowedUsers check skip — कुणीही (नवीन user सुद्धा) हा deep-link वापरू शकतो.
	if len(a) > 0 && strings.HasPrefix(a[0], "totp_") {
		token := strings.TrimSpace(strings.TrimPrefix(a[0], "totp_"))
		return processTelegramLoginToken(ctx, u, chatId, token)
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
		), &ext.ReplyOpts{Markup: coursesButtonMarkup()})
		return dispatcher.EndGroups
	}

	ctx.Reply(u, ext.ReplyTextString(
		"👋 Welcome!\n\n"+
			"📥 Offline Video Download साठी:\n"+
			"Website वर जा → Course उघडा → 📥 Offline button दाबा → Link click करा!\n\n"+
			"⚙️ पहिल्यांदा:\n"+
			"   /register 9876543210\n\n"+
			"❓ Telegram ID: /myid",
	), &ext.ReplyOpts{Markup: coursesButtonMarkup()})
	return dispatcher.EndGroups
}

func processDLToken(ctx *ext.Context, u *ext.Update, chatID int64, tokenStr string) error {
	if len(tokenStr) != 5 {
		reply(ctx, u, "❌ Invalid token. Website वरून नवीन link generate करा.")
		return dispatcher.EndGroups
	}

	dbUser, err := db.GetUserByTelegramID(chatID)
	if err != nil || dbUser == nil {
		tgUser := u.EffectiveUser()
		mention := fmt.Sprintf("[%s](tg://user?id=%d)", tgUser.FirstName, chatID)
		reply(ctx, u,
			fmt.Sprintf("❌ %s, तुमचा Telegram account link केलेला नाही.\n\n"+
				"एकदा register करा:\n"+
				"`/register <mobile_or_email>`\n\n"+
				"उदा: `/register 9876543210`", mention),
		)
		// Owner ला notify — token वरून website user काढतो
		go notifyOwnerNotLinked(ctx, tokenStr, tgUser.FirstName, chatID)
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

	// ── Course limit fetch (token मधून courseID → course → limit) ──────────
	// Token DB मधून courseID काढतो, मग course चा actual DownloadLimit वापरतो.
	// हे नसेल तर RedeemOfflineToken हमेशा default 3 वापरतो — /setdlimit काम करत नाही.
	courseLimit := 3 // safe default
	if offTok, tokErr := db.GetOfflineToken(tokenStr); tokErr == nil && offTok != nil {
		if c, cErr := db.GetCourseByID(offTok.CourseID); cErr == nil && c != nil && c.DownloadLimit > 0 {
			courseLimit = c.DownloadLimit
		}
	}

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
		reply(ctx, u, "⛔ या course साठी access नाही किंवा expire झाला.\n\nAdmin शी संपर्क करा.")
		go notifyOwnerNoAccess(ctx, dbUser, course, result.CourseID, chatID)
		return dispatcher.EndGroups
	}

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

	// [NEW] ── Cache hit — हीच lecture आधीच (offline-token मधून किंवा bot च्या
	// in-bot "Watch" catalog मधून) याच user ला watermark सोबत LOG_CHANNEL मार्गे
	// गेलेली असेल, तर नवीन watermark job/re-upload न करता तिथूनच थेट forward
	// करतो — जलद, आणि CPU/bandwidth दोन्ही वाचतं. दोन्ही मार्ग (offline-token,
	// in-bot watch) एकाच DeliveredCache (userID+courseID+messageID) वर लिहितात
	// आणि वाचतात, त्यामुळे कुठूनही आधी घेतलेली lecture कुठूनही cache-hit होते.
	if cached, cErr := db.GetDeliveredCache(dbUser.ID, result.CourseID, result.MessageID); cErr == nil && cached != nil && cached.LogChannelMsgID != 0 {
		if fwdUpdates, fErr := utils.ForwardFromLogChannel(ctx, cached.LogChannelMsgID, chatID); fErr == nil {
			newMsgID := extractNewMessageID(fwdUpdates)
			if newMsgID != 0 {
				_ = db.ScheduleDelete(chatID, newMsgID)
			}
			shouldAlert, score, _ := db.RecordDownload(dbUser.ID, result.UsedToday)
			if shouldAlert {
				go sendAdminAlert(ctx, dbUser, score)
			}
			go sendDownloadNotification(ctx, dbUser, msg, course, result, courseLimit)
			reply(ctx, u, fmt.Sprintf("✅ Video पाठवलं!\n🔒 Forward व Save blocked\n\n📊 आजचे downloads: %d/%d", result.UsedToday, courseLimit))
			return dispatcher.EndGroups
		}
		// Cache forward fail झालं (log channel मधून message delete झाला असेल) —
		// खालच्या fresh-delivery मार्गावर पडतो.
	}

	sentMsg, _ := ctx.Reply(u, ext.ReplyTextString("⏳ Video पाठवत आहे..."), nil)

	toPeer := ctx.PeerStorage.GetInputPeerById(chatID)

	var editMu sync.Mutex
	lastEdit := time.Time{}
	editProgress := func(text string) {
		if sentMsg == nil {
			return
		}
		editMu.Lock()
		if time.Since(lastEdit) < 3*time.Second {
			editMu.Unlock()
			return
		}
		lastEdit = time.Now()
		editMu.Unlock()
		editCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = botClient.API().MessagesEditMessage(editCtx, &tg.MessagesEditMessageRequest{
			Peer:    toPeer,
			ID:      sentMsg.ID,
			Message: text,
		})
	}

	// [NEW] LOG_CHANNEL peer एकदाच resolve करतो — colab job साठी (logChatID)
	// आणि खालच्या direct-send fallback साठी (log-first-then-forward) दोन्हीसाठी
	// वापरतो. config.ValueOf.LogChannelID हा stripInt() मुळे "-100" शिवायचा raw
	// channel_id असतो — worker ला मात्र पूर्ण Bot-API स्वरूपाचा chat_id
	// ("-100"+channelID) लागतो, नाहीतर PEER_ID_INVALID येतं.
	var logChatID, logAccessHash int64
	var logPeer *tg.InputChannel
	if config.ValueOf.LogChannelID != 0 {
		if lp, lpErr := utils.GetLogChannelPeer(ctx, botClient.API(), botClient.PeerStorage); lpErr == nil && lp != nil {
			logPeer = lp
			logIDStr := fmt.Sprintf("-100%d", lp.ChannelID)
			logChatID, _ = strconv.ParseInt(logIDStr, 10, 64)
			logAccessHash = lp.AccessHash
		}
	}

	// ── Video असेल तर: download → user चा mobile watermark म्हणून burn → reupload.
	// इतर file types (photo/PDF/etc) आधीसारखेच थेट copy-forward होतात.
	// (offline watermark toggle OFF असेल तर video सुद्धा direct copy-forward होतो —
	// खाली watermarked var caption साठी वेगळा ट्रॅक करतो, isVideo केवळ media-type ओळखतो.)
	var inputMedia tg.InputMediaClass
	isVideo := false
	watermarked := false
	wmEnabled := db.GetSettingBool(db.SettingOfflineWatermarkEnabled, true)

	caption := fmt.Sprintf(
		"📥 Offline Download\n🔒 Forward व Save blocked\n\n📊 आजचे downloads: %d/%d",
		result.UsedToday, courseLimit,
	)

	deliveredByColab := false
	colabMessageID := 0
	logChannelMsgID := 0 // LOG_CHANNEL मध्ये गेलेल्या master copy चा msg ID (DeliveredCache साठी)
	deliveredOK := false // worker ने आधीच थेट पाठवलंय अशा वेळी sentMessageID 0 असला तरी delivery यशस्वी आहे हे दाखवायला

	switch m := msg.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			reply(ctx, u, "❌ Document read error.")
			return dispatcher.EndGroups
		}
		for _, attr := range doc.Attributes {
			if _, ok := attr.(*tg.DocumentAttributeVideo); ok {
				isVideo = true
				break
			}
		}
		if !isVideo && strings.HasPrefix(strings.ToLower(doc.MimeType), "video/") {
			isVideo = true
		}

		if isVideo && wmEnabled {
			fileName := ""
			for _, attr := range doc.Attributes {
				if fn, ok := attr.(*tg.DocumentAttributeFilename); ok {
					fileName = fn.FileName
					break
				}
			}
			watermarkText := dbUser.Mobile
			if watermarkText == "" {
				watermarkText = dbUser.Email
			}
			watermarkedCaption := caption + "\n💧 Protected copy"
			progressMsgID := 0
			if sentMsg != nil {
				progressMsgID = sentMsg.ID
			}
			uploaded, sentMsgID, colabLogMsgID, viaColab, skipWM, wmErr := deliverVideoToUser(
				botClient, result.ChannelID, result.MessageID, fileName, watermarkText,
				chatID, progressMsgID, watermarkedCaption, logChatID, logAccessHash, editProgress,
			)
			if wmErr != nil {
				utils.Logger.Sugar().Errorw("watermark delivery failed", "err", wmErr, "user", dbUser.Username)
				reply(ctx, u, "❌ Video process करताना error आली. थोड्या वेळाने पुन्हा प्रयत्न करा किंवा admin शी संपर्क करा.")
				return dispatcher.EndGroups
			}
			if skipWM {
				// कोणताही watermark worker/Colab उपलब्ध नाही (किंवा job fail झाला) —
				// main server वर आता कधीच local FFmpeg चालत नाही, त्यामुळे
				// original video plain copy-forward (unwatermarked) होतो.
				watermarked = false
				inputMedia = &tg.InputMediaDocument{
					ID: &tg.InputDocument{
						ID:            doc.ID,
						AccessHash:    doc.AccessHash,
						FileReference: doc.FileReference,
					},
				}
			} else {
				watermarked = true
				if viaColab {
					deliveredByColab = true
					colabMessageID = sentMsgID
					logChannelMsgID = colabLogMsgID
					deliveredOK = true // worker ने आधीच थेट student ला पाठवलंय — sentMsgID 0 असला तरी delivery यशस्वी आहे
				} else {
					inputMedia = &tg.InputMediaUploadedDocument{
						File:       uploaded,
						MimeType:   doc.MimeType,
						Attributes: doc.Attributes,
					}
				}
			}
		} else {
			inputMedia = &tg.InputMediaDocument{
				ID: &tg.InputDocument{
					ID:            doc.ID,
					AccessHash:    doc.AccessHash,
					FileReference: doc.FileReference,
				},
			}
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

	if watermarked {
		caption += "\n💧 Protected copy"
	}

	sentMessageID := colabMessageID

	// [FIX] आधी थेट student ला Noforwards:true सोबत पाठवायचो — त्यामुळे ना
	// LOG_CHANNEL backup व्हायचा, ना DeliveredCache भरायचा (पुढच्या वेळी परत
	// तीच lecture मागितली तरी परत नवीन upload व्हायचा). आता (worker मार्फत
	// गेलं नसेल तरच) आधी LOG_CHANNEL ला unprotected master म्हणून पाठवतो, मग
	// तिथूनच student ला protected म्हणून forward करतो — catalog.go च्या
	// in-bot "Watch" flow सारखाच पॅटर्न, आणि तीच DeliveredCache वापरतो.
	if !deliveredByColab && inputMedia != nil {
		editProgress("📨 पाठवत आहोत...")

		if logPeer != nil {
			logPeerInput := &tg.InputPeerChannel{ChannelID: logPeer.ChannelID, AccessHash: logPeer.AccessHash}
			logSent, logErr := botClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
				Peer:     logPeerInput,
				Media:    inputMedia,
				Message:  caption,
				RandomID: rand.Int63(),
			})
			if logErr == nil {
				logMsgID := extractNewMessageID(toUpdates(logSent))
				if logMsgID != 0 {
					logChannelMsgID = logMsgID
					if fwdUpdates, fErr := utils.ForwardFromLogChannel(ctx, logMsgID, chatID); fErr == nil {
						sentMessageID = extractNewMessageID(fwdUpdates)
					} else {
						utils.Logger.Sugar().Warnw("processDLToken: forward from log-channel to student failed", "err", fErr)
					}
				}
			} else {
				utils.Logger.Sugar().Warnw("processDLToken: log-channel upload failed, sending direct to student instead", "err", logErr)
			}
		}

		// LOG_CHANNEL configure नसेल, किंवा वरचा मार्ग काही कारणाने जमला नाही —
		// तरी student ला video मिळायलाच हवं, थेट पाठवतो (backup/cache न होता).
		if sentMessageID == 0 {
			sentMedia, sendErr := botClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
				Peer:       toPeer,
				Media:      inputMedia,
				Message:    caption,
				Noforwards: true,
				RandomID:   rand.Int63(),
			})
			if sendErr != nil {
				reply(ctx, u, "❌ Video पाठवताना error: "+sendErr.Error())
				return dispatcher.EndGroups
			}
			sentMessageID = extractNewMessageID(toUpdates(sentMedia))
		}
	}

	if sentMessageID != 0 {
		_ = db.ScheduleDelete(chatID, sentMessageID)
	}

	// [NEW] DeliveredCache save — याच cache वर catalog.go चा in-bot "Watch" flow
	// सुद्धा लिहितो/वाचतो, त्यामुळे offline-token आणि in-bot watch दोन्ही
	// मार्गांनी एकदा घेतलेली lecture कुठूनही परत मागितली तरी fast-path
	// forward होते (नवीन watermark job न चालवता).
	if sentMessageID != 0 || deliveredOK {
		_ = db.UpsertDeliveredCache(dbUser.ID, result.CourseID, result.MessageID, logChannelMsgID, watermarked)
	}

	shouldAlert, score, _ := db.RecordDownload(dbUser.ID, result.UsedToday)
	if shouldAlert {
		go sendAdminAlert(ctx, dbUser, score)
	}

	// ── Owner ला download notification पाठवतो ────────────────────────────────
	go sendDownloadNotification(ctx, dbUser, msg, course, result, courseLimit)

	if sentMsg != nil {
		_ = ctx.DeleteMessages(chatID, []int{sentMsg.ID})
	}

	return dispatcher.EndGroups
}

// processTelegramLoginToken — /start totp_<token>
// Website login साठी वापरलेला deep-link. Token वरून कोणता website account आहे
// ते कळतं. One-user-one-Telegram-ID कायमचं lock (relink नाही — फक्त admin चा
// /unlinktg वापरून unlink झाल्यावरच परत link शक्य). Link झाल्यावर लगेच OTP
// त्याच Telegram chat वर पाठवला जातो — user ला परत website वर जाऊन तो टाकायचा असतो.
func processTelegramLoginToken(ctx *ext.Context, u *ext.Update, chatID int64, tokenStr string) error {
	rec, err := db.GetTGLoginToken(tokenStr)
	if err != nil || rec == nil {
		reply(ctx, u, "❌ Link expire झाला किंवा आधीच वापरला गेला.\n\nWebsite वरून परत \"Login via Telegram\" दाबा.")
		return dispatcher.EndGroups
	}

	siteUser, err := db.GetUserByID(rec.UserID)
	if err != nil || siteUser == nil {
		reply(ctx, u, "❌ Website account सापडला नाही. Admin शी संपर्क करा.")
		return dispatcher.EndGroups
	}

	if siteUser.TelegramID != 0 && siteUser.TelegramID != chatID {
		// One user ↔ one Telegram ID — दुसऱ्याच telegram वरून हा link वापरता येणार नाही.
		reply(ctx, u, "❌ हा website account आधीच दुसऱ्या Telegram account शी link आहे.")
		return dispatcher.EndGroups
	}

	if siteUser.TelegramID == 0 {
		// दुसऱ्या कुठल्या website account ला हा Telegram ID आधीच link आहे का?
		if other, _ := db.GetUserByTelegramID(chatID); other != nil && other.ID != siteUser.ID {
			reply(ctx, u, "❌ तुमचा Telegram account आधीच दुसऱ्या website account शी link आहे.\n\nएक Telegram account फक्त एकाच account शी link होऊ शकतो.")
			return dispatcher.EndGroups
		}

		if err := db.SetUserTelegramID(siteUser.ID, chatID); err != nil {
			reply(ctx, u, "❌ Link करताना error आली. पुन्हा प्रयत्न करा किंवा admin शी संपर्क करा.")
			return dispatcher.EndGroups
		}

		// लगेच peer cache warm करतो — पुढच्या OTP साठी redeploy नंतरही पुन्हा
		// user कडून message लागणार नाही (WarmUserPeerCache startup ला हेच पुढे continue करतो).
		if tgUser := u.EffectiveUser(); tgUser != nil {
			ctx.PeerStorage.AddPeer(tgUser.ID, tgUser.AccessHash, storage.TypeUser, tgUser.Username)
		}
	}

	_ = db.MarkTGLoginTokenUsed(rec.ID)

	// ── लगेच OTP generate करून याच chat वर पाठवतो ──────────────────────────────
	identifier := siteUser.Mobile
	if identifier == "" {
		identifier = siteUser.Email
	}

	otpPlain := generateLoginOTP()
	otpHash, err := auth.HashPassword(otpPlain)
	if err != nil {
		reply(ctx, u, "❌ Server error. Website वरून पुन्हा प्रयत्न करा.")
		return dispatcher.EndGroups
	}
	if err := db.CreateOTP(identifier, otpHash); err != nil {
		reply(ctx, u, "❌ Server error. Website वरून पुन्हा प्रयत्न करा.")
		return dispatcher.EndGroups
	}

	reply(ctx, u, fmt.Sprintf(
		"✅ *Telegram Linked!*\n\n"+
			"🔐 तुमचा Login OTP: `%s`\n\n"+
			"⏳ 10 मिनिटांत expire होईल.\n"+
			"👉 website वर परत जाऊन हा OTP टाका.",
		otpPlain,
	))
	return dispatcher.EndGroups
}

// generateLoginOTP — cryptographically random 4-digit code (1000–9999).
// internal/auth चा generateOTP सारखाच (unexported असल्यामुळे इथे स्वतंत्र copy).
func generateLoginOTP() string {
	n, _ := crand.Int(crand.Reader, big.NewInt(9000))
	return fmt.Sprintf("%d", 1000+n.Int64())
}

// sendDownloadNotification — user ने video download केल्यावर owner ला notify करतो.
// User details + video name + course + कोटा पाठवतो.
func sendDownloadNotification(ctx *ext.Context, user *db.User, msg *tg.Message, course *db.Course, result *db.RedeemResult, courseLimit int) {
	botClient := MainBotClient
	if botClient == nil {
		return
	}

	// Video name — Document attributes मधून filename काढतो
	videoName := "Unknown"
	if msg.Message != "" {
		videoName = msg.Message // original caption असेल तर तो वापरतो
	}
	if mediaDoc, ok := msg.Media.(*tg.MessageMediaDocument); ok {
		if doc, ok := mediaDoc.Document.(*tg.Document); ok {
			for _, attr := range doc.Attributes {
				if fn, ok := attr.(*tg.DocumentAttributeFilename); ok {
					videoName = fn.FileName
					break
				}
				if video, ok := attr.(*tg.DocumentAttributeVideo); ok {
					_ = video // video attribute आहे हे confirm
				}
			}
		}
	}

	courseName := "Unknown Course"
	if course != nil {
		courseName = course.Title
	}

	identifier := user.Mobile
	if identifier == "" {
		identifier = user.Email
	}

	notifyMsg := fmt.Sprintf(
		"📥 *Video Downloaded*\n\n"+
			"👤 User: `%s`\n"+
			"📱 Mobile: %s\n"+
			"📧 Email: %s\n"+
			"🎓 Course: %s\n"+
			"🎬 Video: `%s`\n"+
			"📊 Downloads Today: %d/%d",
		user.Username,
		user.Mobile,
		user.Email,
		courseName,
		videoName,
		result.UsedToday,
		courseLimit,
	)

	adminIDs := []int64(config.ValueOf.AllowedUsers)
	for _, adminID := range adminIDs {
		adminPeer := ctx.PeerStorage.GetInputPeerById(adminID)
		if adminPeer == nil {
			continue
		}
		_, _ = botClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     adminPeer,
			Message:  notifyMsg,
			RandomID: rand.Int63(),
		})
	}
}

// notifyOwnerNotLinked — user ने token टाकला पण Telegram link नाही तेव्हा owner ला notify करतो.
// Token वरून website user काढतो आणि ready-made /settg command पाठवतो.
func notifyOwnerNotLinked(ctx *ext.Context, tokenStr string, tgName string, tgChatID int64) {
	botClient := MainBotClient
	if botClient == nil {
		return
	}

	// Token वरून website user काढतो
	tok, err := db.GetOfflineToken(tokenStr)
	if err != nil || tok == nil {
		// Token सापडला नाही — फक्त TG info पाठवतो
		msg := fmt.Sprintf(
			"🔗 *Telegram Link नाही*\n\n"+
				"👤 TG Name: %s\n"+
				"🆔 TG ID: `%d`\n\n"+
				"Token invalid/expire — website user सापडला नाही.\n"+
				"User ला /register करायला सांगा.",
			tgName, tgChatID,
		)
		for _, adminID := range []int64(config.ValueOf.AllowedUsers) {
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
		return
	}

	// Token मधून website user काढतो
	siteUser, err := db.GetUserByID(tok.UserID)
	if err != nil || siteUser == nil {
		return
	}

	identifier := siteUser.Mobile
	if identifier == "" {
		identifier = siteUser.Email
	}

	settgCmd := fmt.Sprintf("/settg %s %d", identifier, tgChatID)

	notifyMsg := fmt.Sprintf(
		"🔗 *Telegram Link नाही — Token वापरला*\n\n"+
			"👤 TG Name: %s\n"+
			"🆔 TG ID: `%d`\n\n"+
			"🌐 Website Account:\n"+
			"   Name: %s\n"+
			"   📱 Mobile: %s\n"+
			"   📧 Email: %s\n\n"+
			"Link करायला खाली copy करा:\n"+
			"`%s`",
		tgName, tgChatID,
		siteUser.FullName, siteUser.Mobile, siteUser.Email,
		settgCmd,
	)

	for _, adminID := range []int64(config.ValueOf.AllowedUsers) {
		adminPeer := ctx.PeerStorage.GetInputPeerById(adminID)
		if adminPeer == nil {
			continue
		}
		_, _ = botClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     adminPeer,
			Message:  notifyMsg,
			RandomID: rand.Int63(),
		})
	}
}

// notifyOwnerNoAccess — user चा course access नाही/expire तेव्हा owner ला notify करतो.
// User चे full details + ready-made /grantaccess command पाठवतो.
func notifyOwnerNoAccess(ctx *ext.Context, user *db.User, course *db.Course, courseID primitive.ObjectID, tgChatID int64) {
	botClient := MainBotClient
	if botClient == nil {
		return
	}

	courseName := "Unknown Course"
	if course != nil {
		courseName = course.Title
	}

	identifier := user.Mobile
	if identifier == "" {
		identifier = user.Email
	}

	grantCmd := fmt.Sprintf("/grantaccess %s %s", identifier, courseID.Hex())

	notifyMsg := fmt.Sprintf(
		"⛔ *Access नाही — Token वापरला*\n\n"+
			"👤 User: `%s`\n"+
			"📱 Mobile: %s\n"+
			"📧 Email: %s\n"+
			"🆔 TG ID: `%d`\n"+
			"🎓 Course: %s\n\n"+
			"Access द्यायला खाली copy करा:\n"+
			"`%s`",
		user.Username,
		user.Mobile,
		user.Email,
		tgChatID,
		courseName,
		grantCmd,
	)

	adminIDs := []int64(config.ValueOf.AllowedUsers)
	for _, adminID := range adminIDs {
		adminPeer := ctx.PeerStorage.GetInputPeerById(adminID)
		if adminPeer == nil {
			continue
		}
		_, _ = botClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     adminPeer,
			Message:  notifyMsg,
			RandomID: rand.Int63(),
		})
	}
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
		for _, adminID := range []int64(config.ValueOf.AllowedUsers) {
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
