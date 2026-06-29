package bot

import (
	"EverythingSuckz/fsb/config"
	"fmt"
	"log"
	"math/rand"

	"github.com/gotd/td/tg"
)

// NotifyAllowedUsers — config.ValueOf.AllowedUsers (admin chi Telegram ID's,
// jya ALLOWED_USERS env var madhe already aahet, /addcourse,/grantaccess
// sarkhe commands chalvayla allowed) - tyana ek plain text DM pathavto.
//
// Email sobat ha extra channel — user ne /api/contact var message kela
// kiva /api/purchase var course/batch "buy" submit kela tar ithun call hoto.
//
// Limitation: jya admin ne kadhich bot la kahich (command/message) pathavla
// nahi, tyacha Telegram peer cache madhe nasto — Telegram bots la arbitrary
// user ID var pahilyanda DM pathavता yet nahi (platform restriction, bug
// nahi). Already commands chalवणारे admin sathi he nehmi kaam karel, kaaran
// te aadhich bot la message pathavlele aahet.
func NotifyAllowedUsers(text string) {
	if Bot == nil {
		return
	}
	for _, id := range config.ValueOf.AllowedUsers {
		go func(adminID int64) {
			if err := SendTelegramDM(adminID, text); err != nil {
				log.Printf("[Notify] AllowedUser %d la Telegram DM pathavta ala nahi: %v", adminID, err)
			}
		}(id)
	}
}

// SendTelegramDM — kontyahi Telegram userID la plain text DM pathavto.
// Admin (ALLOWED_USERS) sathi NotifyAllowedUsers vaprto, ani website users
// sathi (TelegramID linked asel tar) adminBroadcast / adminSendMessageToUser
// madhun direct call hoto. Peer cache aagodarच WarmAdminPeerCache /
// WarmUserPeerCache ne warm kelela aste, tyamule user ne punha message
// karaychi garaj nasते.
//
// Peer cache madhe sapadla nahi tar error return karto — caller (email
// sarkhech) best-effort म्हणून ignore/log karू shakto, request fail hot nahi.
func SendTelegramDM(userID int64, text string) error {
	if Bot == nil {
		return fmt.Errorf("bot client running nahi")
	}
	if userID == 0 {
		return fmt.Errorf("invalid telegram userID: 0")
	}
	ctx := Bot.CreateContext()

	peer := Bot.PeerStorage.GetInputPeerById(userID)
	inputPeer, ok := peer.(*tg.InputPeerUser)
	if !ok {
		return fmt.Errorf("telegram peer cache madhe nahi (user %d) - warm-up cycle wait kara kiva user la ekda /start karayla sanga", userID)
	}

	_, err := Bot.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     inputPeer,
		Message:  text,
		RandomID: rand.Int63(),
	})
	return err
}
