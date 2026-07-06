package commands

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/utils"
	tokenpkg "EverythingSuckz/fsb/internal/token"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/storage"
	"github.com/celestix/gotgproto/types"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/tg"
)

// Telegram URL patterns supported:
//   https://t.me/c/1234567890/42        → private channel (MTProto ID without -100)
//   https://t.me/+AbCd.../42            → private invite-link channel (not supported, need join first)
//   https://t.me/publicname/42          → public channel (forward to log channel like before)

var (
	reTgPrivate = regexp.MustCompile(`https?://t\.me/c/(\d+)/(\d+)`)
	reTgPublic  = regexp.MustCompile(`https?://t\.me/([A-Za-z][A-Za-z0-9_]{3,})/(\d+)`)
)

func (m *command) LoadStream(dispatcher dispatcher.Dispatcher) {
	log := m.log.Named("start")
	defer log.Sugar().Info("Loaded")
	dispatcher.AddHandler(
		handlers.NewMessage(nil, sendLink),
	)
}

func supportedMediaFilter(m *types.Message) (bool, error) {
	if not := m.Media == nil; not {
		return false, dispatcher.EndGroups
	}
	switch m.Media.(type) {
	case *tg.MessageMediaDocument:
		return true, nil
	case *tg.MessageMediaPhoto:
		return true, nil
	case tg.MessageMediaClass:
		return false, dispatcher.EndGroups
	default:
		return false, nil
	}
}

func sendLink(ctx *ext.Context, u *ext.Update) error {
	chatId := u.EffectiveChat().GetID()
	peerChatId := ctx.PeerStorage.GetPeerById(chatId)
	if peerChatId.Type != int(storage.TypeUser) {
		return dispatcher.EndGroups
	}
	if len(config.ValueOf.AllowedUsers) != 0 && !utils.Contains(config.ValueOf.AllowedUsers, chatId) {
		return dispatcher.ContinueGroups
	}

	// ── Private channel URL detection ──────────────────────────────────────────
	// Bot la ek text message aala aani tyat t.me/c/CHANNELID/MSGID format URL
	// asel tar → userbot cha MainUserbotClient vaaprun file fetch karto,
	// log channel la forward na karta directly token banto (channelID pn store).
	msg := u.EffectiveMessage
	if msg.Media == nil && msg.Text != "" {
		text := strings.TrimSpace(msg.Text)

		// Private channel: t.me/c/<channelID>/<msgID>
		if m := reTgPrivate.FindStringSubmatch(text); len(m) == 3 {
			rawChannelID, _ := strconv.ParseInt(m[1], 10, 64)
			msgID, _ := strconv.Atoi(m[2])
			// t.me/c/ uses plain MTProto channel ID (no -100 prefix)
			// FileFromMessageInChannel cha GetChannelPeer handles both formats,
			// but stored token la canonical -100 format use karto (routes/stream.go shI consistent)
			channelIDForToken := -(rawChannelID + 1000000000000) // -100XXXXXXXXXX format

			return handlePrivateChannelURL(ctx, u, rawChannelID, channelIDForToken, msgID)
		}

		// Public channel username URL: t.me/<username>/<msgID>
		// He usernames are accessible via main bot too, but we handle them
		// the same way as private (via userbot) for consistency.
		if m := reTgPublic.FindStringSubmatch(text); len(m) == 3 {
			// Public channel — forward to log channel (existing behavior)
			// We can't easily resolve username to channelID here without an API call,
			// so we reply with instructions to forward the message directly instead.
			ctx.Reply(u, ext.ReplyTextString(
				"📢 *Public channel URL detected.*\n\n"+
					"File directly forward kara (message forward karून bot la pathva), "+
					"mhanje bot automatically streaming link denar.\n\n"+
					"Private channel sathi `https://t.me/c/<channelID>/<msgID>` format use kara.",
			), nil)
			return dispatcher.EndGroups
		}

		// Unknown text — ignore (relay handler la jau dya)
		return dispatcher.ContinueGroups
	}

	// ── Regular file message (forward-based, existing behavior) ────────────────
	supported, err := supportedMediaFilter(u.EffectiveMessage)
	if err != nil {
		return err
	}
	if !supported {
		ctx.Reply(u, ext.ReplyTextString("Sorry, this message type is unsupported."), nil)
		return dispatcher.EndGroups
	}

	update, err := utils.ForwardMessages(ctx, chatId, config.ValueOf.LogChannelID, u.EffectiveMessage.ID)
	if err != nil {
		utils.Logger.Sugar().Error(err)
		ctx.Reply(u, ext.ReplyTextString(fmt.Sprintf("Error - %s", err.Error())), nil)
		return dispatcher.EndGroups
	}
	if len(update.Updates) < 2 {
		ctx.Reply(u, ext.ReplyTextString("Error - unexpected update structure from Telegram"), nil)
		return dispatcher.EndGroups
	}
	msgIDUpdate, ok := update.Updates[0].(*tg.UpdateMessageID)
	if !ok {
		ctx.Reply(u, ext.ReplyTextString("Error - unexpected update type"), nil)
		return dispatcher.EndGroups
	}
	messageID := msgIDUpdate.ID
	newMsg, ok := update.Updates[1].(*tg.UpdateNewChannelMessage)
	if !ok {
		ctx.Reply(u, ext.ReplyTextString("Error - unexpected channel message update"), nil)
		return dispatcher.EndGroups
	}
	msg2, ok := newMsg.Message.(*tg.Message)
	if !ok {
		ctx.Reply(u, ext.ReplyTextString("Error - unexpected message type"), nil)
		return dispatcher.EndGroups
	}
	doc := msg2.Media
	file, err := utils.FileFromMedia(doc)
	if err != nil {
		ctx.Reply(u, ext.ReplyTextString(fmt.Sprintf("Error - %s", err.Error())), nil)
		return dispatcher.EndGroups
	}

	pubToken, err := tokenpkg.NewPublicToken(messageID)
	if err != nil {
		ctx.Reply(u, ext.ReplyTextString(fmt.Sprintf("Error generating stream token - %s", err.Error())), nil)
		return dispatcher.EndGroups
	}
	return sendStreamReply(ctx, u, pubToken, file.MimeType)
}

// handlePrivateChannelURL — t.me/c/<channelID>/<msgID> URL aslyavar:
// UserBot cha MainUserbotClient vaaprun (private channel access sathi)
// file metadata fetch karto. Forward to log channel hot nahi.
// Token madhe channelID store hoto — /stream/:token route userbot vaaprun serve karto.
func handlePrivateChannelURL(ctx *ext.Context, u *ext.Update, rawChannelID int64, channelIDForToken int64, msgID int) error {
	if MainUserbotClient == nil {
		ctx.Reply(u, ext.ReplyTextString("❌ Userbot running nahi — private channel streaming available nahi."), nil)
		return dispatcher.EndGroups
	}

	ctx.Reply(u, ext.ReplyTextString(fmt.Sprintf("⏳ Fetching file info from private channel `%d` message `%d`...", rawChannelID, msgID)), nil)

	fetchCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	file, err := utils.FileFromMessageInChannel(fetchCtx, MainUserbotClient, msgID, rawChannelID)
	if err != nil {
		ctx.Reply(u, ext.ReplyTextString(fmt.Sprintf(
			"❌ File fetch failed: %s\n\n"+
				"Check kara:\n"+
				"• Userbot tya channel cha member aahe ka?\n"+
				"• Channel ID ani Message ID barobar aahet ka?",
			err.Error(),
		)), nil)
		return dispatcher.EndGroups
	}

	pubToken, err := tokenpkg.NewPublicTokenForChannel(msgID, channelIDForToken)
	if err != nil {
		ctx.Reply(u, ext.ReplyTextString(fmt.Sprintf("Error generating stream token - %s", err.Error())), nil)
		return dispatcher.EndGroups
	}
	return sendStreamReply(ctx, u, pubToken, file.MimeType)
}

// sendStreamReply — token + mimeType dile ki Stream/Download buttons saha reply.
func sendStreamReply(ctx *ext.Context, u *ext.Update, pubToken string, mimeType string) error {
	link := fmt.Sprintf("%s/stream/%s", config.ValueOf.Host, pubToken)
	text := styling.Code(link)
	row := tg.KeyboardButtonRow{
		Buttons: []tg.KeyboardButtonClass{
			&tg.KeyboardButtonURL{
				Text: "Download",
				URL:  link + "?d=true",
			},
		},
	}
	if strings.Contains(mimeType, "video") || strings.Contains(mimeType, "audio") || strings.Contains(mimeType, "pdf") {
		row.Buttons = append(row.Buttons, &tg.KeyboardButtonURL{
			Text: "Stream",
			URL:  link,
		})
	}
	markup := &tg.ReplyInlineMarkup{
		Rows: []tg.KeyboardButtonRow{row},
	}
	var err error
	if strings.Contains(link, "http://localhost") {
		_, err = ctx.Reply(u, ext.ReplyTextStyledText(text), &ext.ReplyOpts{
			NoWebpage:        false,
			ReplyToMessageId: u.EffectiveMessage.ID,
		})
	} else {
		_, err = ctx.Reply(u, ext.ReplyTextStyledText(text), &ext.ReplyOpts{
			Markup:           markup,
			NoWebpage:        false,
			ReplyToMessageId: u.EffectiveMessage.ID,
		})
	}
	if err != nil {
		utils.Logger.Sugar().Error(err)
	}
	return dispatcher.EndGroups
}
