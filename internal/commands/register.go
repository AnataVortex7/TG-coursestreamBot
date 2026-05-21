// internal/commands/register.go
//
// /register <mobile_or_email>
//
// User हा command bot मध्ये पाठवतो → Admin ला ready-made /settg command येतो
// Admin फक्त copy-paste करतो → User linked होतो → /dl वापरता येतो
//
// Flow:
//   User:  /register 9876543210
//   Bot→User: ✅ Request पाठवली! Admin link करेल लवकरच.
//   Bot→Admin: 🔔 New Link Request
//              👤 User: 9876543210
//              📱 TG ID: 987654321
//              Copy करा: `/settg 9876543210 987654321`

package commands

import (
	"fmt"
	"math/rand"
	"strings"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/email"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/storage"
	"github.com/gotd/td/tg"
)

func (m *command) LoadRegister(d dispatcher.Dispatcher) {
	log := m.log.Named("register")
	defer log.Sugar().Info("Loaded /register command")
	d.AddHandler(handlers.NewCommand("register", cmdRegister))
}

// /register <mobile_or_email>
func cmdRegister(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()

	// फक्त private chat मध्ये
	peerInfo := ctx.PeerStorage.GetPeerById(chatID)
	if peerInfo.Type != int(storage.TypeUser) {
		reply(ctx, u, "⚠️ /register फक्त bot शी private chat मध्ये वापरा.")
		return dispatcher.EndGroups
	}

	a := args(u)
	if len(a) < 1 {
		reply(ctx, u,
			"📋 *Offline Access साठी Register करा*\n\n"+
				"तुमचा website वरील mobile number किंवा email टाका:\n\n"+
				"`/register 9876543210`\n"+
				"किंवा\n"+
				"`/register yourname@email.com`\n\n"+
				"Admin तुमचा account link करेल, मग `/dl` command वापरता येईल.")
		return dispatcher.EndGroups
	}

	identifier := strings.ToLower(strings.TrimSpace(a[0]))

	// DB मध्ये user आहे का check करा
	user, err := db.GetUserByMobileOrEmail(identifier)
	if err != nil || user == nil {
		reply(ctx, u,
			"❌ हा mobile/email website वर registered नाही.\n\n"+
				"आधी website वर account बनवा, मग /register करा.")
		return dispatcher.EndGroups
	}

	// आधीच linked आहे का?
	if user.TelegramID != 0 {
		if user.TelegramID == chatID {
			reply(ctx, u,
				"✅ तुमचा account आधीच linked आहे!\n\n"+
					"आता `/dl <TOKEN>` वापरू शकता.\n\n"+
					"Token कसा मिळवायचा:\n"+
					"1. Website वर course उघडा\n"+
					"2. Video च्या खाली 📥 *Offline* button दाबा\n"+
					"3. मिळालेला token `/dl XXXXX` असा पाठवा")
		} else {
			reply(ctx, u,
				"⚠️ हा account दुसऱ्या Telegram ID शी linked आहे.\n"+
					"Admin शी संपर्क करा.")
		}
		return dispatcher.EndGroups
	}

	// Admin ला alert पाठवा
	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client चालू नाही. थोड्या वेळाने पुन्हा करा.")
		return dispatcher.EndGroups
	}

	// Admin IDs — config च्या AllowedUsers मधून
	adminIDs := []int64(config.ValueOf.AllowedUsers)
	if len(adminIDs) == 0 {
		reply(ctx, u, "❌ Admin configure केलेला नाही.")
		return dispatcher.EndGroups
	}

	// Admin ला message पाठवा
	settgCmd := fmt.Sprintf("/settg %s %d", identifier, chatID)
	adminMsg := fmt.Sprintf(
		"🔔 New Link Request\n\n"+
			"👤 User: %s\n"+
			"📛 Name: %s\n"+
			"📱 TG ID: %d\n\n"+
			"खाली copy करा आणि send करा:\n"+
			"%s",
		identifier,
		user.FullName,
		chatID,
		settgCmd,
	)

	sentToAdmin := false
	for _, adminID := range adminIDs {
		_, err := botClient.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     &tg.InputPeerUser{UserID: adminID},
			Message:  adminMsg,
			RandomID: rand.Int63(),
		})
		if err == nil {
			sentToAdmin = true
		}
	}

	// Email पण पाठवा (ADMIN_EMAIL set असेल तर)
	if adminEmail := config.ValueOf.AdminEmail; adminEmail != "" {
		go func() {
			_ = email.SendRegistrationRequest(adminEmail, identifier, user.FullName, chatID)
		}()
		sentToAdmin = true
	}

	_ = sentToAdmin

	// User ला confirm करा
	reply(ctx, u,
		"✅ *Request पाठवली!*\n\n"+
			"Admin तुमचा account लवकरच link करेल.\n"+
			"Link झाल्यावर `/dl` command वापरता येईल.\n\n"+
			"📥 *Offline Download कसे करायचे:*\n"+
			"1. Website वर course उघडा\n"+
			"2. Video च्या खाली 📥 *Offline* button दाबा\n"+
			"3. Token मिळेल — उदा. `48271`\n"+
			"4. इथे टाका: `/dl 48271`\n"+
			"5. Bot video पाठवेल — forward व save blocked असेल\n\n"+
			"⚠️ रोज फक्त *5 downloads* — midnight IST ला reset")

	return dispatcher.EndGroups
}
