// internal/commands/relay.go
//
// Two-way relay system:
//
//   User → Bot → Admin:
//     कोणताही user (allowed नसलेला पण) bot ला text/file पाठवतो
//     → Admin ला forward होतो, header मध्ये user चे नाव + ID असते
//
//   Admin → Bot → User (reply):
//     Admin त्या forwarded message ला reply करतो
//     → Bot ते reply original user ला पाठवतो
//
// relay_map: forwardedMsgID (admin side) → senderUserID (original user)
// हे in-memory आहे — bot restart झाल्यास जुने mappings जातात

package commands

import (
	"fmt"
	"math/rand"
	"strings"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/storage"
	"github.com/gotd/td/tg"
)

func (m *command) LoadZRelay(d dispatcher.Dispatcher) {
	log := m.log.Named("relay")
	defer log.Sugar().Info("Loaded relay handler")
	// Group -1 — sendLink (group 0) च्या आधी चालतो.
	// sendLink plain text messages साठी EndGroups देतो (media नसल्यास),
	// त्यामुळे relay आधी असणं गरजेचं — नाहीतर admin चं text reply पुढे जातच नाही.
	d.AddHandlerToGroup(handlers.NewMessage(nil, handleRelay), -1)
}

func handleRelay(ctx *ext.Context, u *ext.Update) error {
	msg := u.EffectiveMessage
	if msg == nil {
		return dispatcher.ContinueGroups
	}

	// Commands (/start, /register वगैरे) व Menu Buttons — relay skip, command handlers ला जाऊ देतो
	txt := strings.TrimSpace(msg.Text)
	if strings.HasPrefix(txt, "/") || isMenuButtonText(txt) {
		return dispatcher.ContinueGroups
	}

	// Bot चे स्वतःचे messages relay skip करा
	// Bot ने admin ला पाठवलेले progress/file messages relay होऊ नयेत
	if MainBotClient != nil && u.EffectiveUser().GetID() == MainBotClient.Self.ID {
		return dispatcher.ContinueGroups
	}

	senderID := u.EffectiveUser().GetID()
	chatID    := u.EffectiveChat().GetID()

	// फक्त private chat मध्ये relay काम करतो
	peerInfo := ctx.PeerStorage.GetPeerById(chatID)
	if peerInfo.Type != int(storage.TypeUser) {
		return dispatcher.ContinueGroups
	}

	adminIDs := []int64(config.ValueOf.AllowedUsers)
	isAdmin  := utils.Contains(config.ValueOf.AllowedUsers, senderID)

	// ── Admin reply → User ───────────────────────────────────────────────────
	// Admin ने reply केला का? ReplyTo असेल तर त्या message ID वरून user शोधतो
	if isAdmin && msg.ReplyTo != nil {
		replyTo, ok := msg.ReplyTo.(*tg.MessageReplyHeader)
		if ok {
			repliedMsgID := replyTo.ReplyToMsgID
			uid, _ := db.GetRelayEntry(repliedMsgID)
			if uid != 0 {
				sendReplyToUser(ctx, u, uid, msg.Message)
				return dispatcher.EndGroups
			}
		}
		// Admin चा reply relay map मध्ये नाही — normal message, skip
		return dispatcher.ContinueGroups
	}

	// ── Allowed user → normal bot flow ──────────────────────────────────────
	if isAdmin || utils.Contains(config.ValueOf.AllowedUsers, senderID) {
		return dispatcher.ContinueGroups
	}

	// [FIX] userbotDL किंवा इतर कोणताही prompt pending असेल तर relay करायचा नाही!
	if p := getPending(senderID); p != nil {
		return dispatcher.ContinueGroups
	}

	if st := getGiftPending(senderID); st != nil {
		return dispatcher.ContinueGroups
	}

	// [FIX] userbot login flow सुरू असेल तर relay करायचा नाही!
	if ls := getUserbotLogin(senderID); ls != nil {
		return dispatcher.ContinueGroups
	}

	// ── Unknown user → Admin ला forward करा ─────────────────────────────────
	if len(adminIDs) == 0 {
		return dispatcher.EndGroups
	}

	botClient := MainBotClient
	if botClient == nil {
		return dispatcher.EndGroups
	}

	// User चे नाव बनवतो
	user    := u.EffectiveUser()
	name    := user.FirstName
	if user.LastName != "" {
		name += " " + user.LastName
	}
	if user.Username != "" {
		name += " (@" + user.Username + ")"
	}

	header := fmt.Sprintf("📩 Message from: %s\nTG ID: %d", name, senderID)

	for _, adminID := range adminIDs {
		adminPeer := ctx.PeerStorage.GetInputPeerById(adminID)
		if adminPeer == nil {
			continue
		}

		// Header message पाठवतो
		headerResult, err := botClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     adminPeer,
			Message:  header,
			RandomID: rand.Int63(),
		})
		_ = headerResult

		// Original message forward करतो
		fromPeer := ctx.PeerStorage.GetInputPeerById(senderID)
		if fromPeer == nil {
			continue
		}

		fwdResult, err := botClient.API().MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
			FromPeer: fromPeer,
			ID:       []int{msg.ID},
			ToPeer:   adminPeer,
			RandomID: []int64{rand.Int63()},
		})
		if err != nil {
			continue
		}

		// Forwarded message का ID निकालतो — reply map साठी
		if updates, ok := fwdResult.(*tg.Updates); ok {
			for _, upd := range updates.Updates {
				if newMsg, ok2 := upd.(*tg.UpdateNewMessage); ok2 {
					if m, ok3 := newMsg.Message.(*tg.Message); ok3 {
						_ = db.SaveRelayEntry(m.ID, senderID)
					}
				}
			}
		}
	}

	// User ला confirm करा
	ctx.Reply(u, ext.ReplyTextString("✅ तुमचा message admin ला पोहोचला. लवकरच reply येईल."), nil)
	return dispatcher.EndGroups
}

// sendReplyToUser — admin चे reply user ला पाठवतो (text + media दोन्ही handle)
func sendReplyToUser(ctx *ext.Context, u *ext.Update, userID int64, msg *tg.Message) {
	botClient := MainBotClient
	if botClient == nil {
		return
	}

	// ctx.PeerStorage try करतो, नाही मिळाला तर Bot.PeerStorage fallback
	userPeer := ctx.PeerStorage.GetInputPeerById(userID)
	if userPeer == nil && botClient.PeerStorage != nil {
		userPeer = botClient.PeerStorage.GetInputPeerById(userID)
	}
	if userPeer == nil {
		ctx.Reply(u, ext.ReplyTextString("❌ User peer सापडला नाही. User ने एकदा bot ला message करायला सांगा."), nil)
		return
	}

	// Media असेल तर MessagesSendMedia, नाहीतर MessagesSendMessage
	if msg.Media != nil {
		inputMedia, err := buildInputMedia(msg)
		if err == nil {
			_, _ = botClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
				Peer:     userPeer,
				Media:    inputMedia,
				Message:  msg.Message,
				RandomID: rand.Int63(),
			})
			return
		}
	}

	if msg.Message != "" {
		_, _ = botClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     userPeer,
			Message:  msg.Message,
			RandomID: rand.Int63(),
		})
	}
}

func isMenuButtonText(txt string) bool {
	return txt == "📚 Courses" || txt == "⭐ Userbot Settings" || txt == "⭐ Premium Menu" || txt == "⭐ Premium" || txt == "💻 CMD" || txt == "💻 Commands" || txt == "⚡ Pre-download" || txt == "📥 Userbot Download" || txt == "📊 Bot Status" || txt == "❓ My ID" || txt == "ℹ️ Help"
}
