package bot

import (
	"EverythingSuckz/fsb/config"
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
		go sendTelegramDM(id, text)
	}
}

func sendTelegramDM(userID int64, text string) {
	if Bot == nil {
		return
	}
	ctx := Bot.CreateContext()

	peer := Bot.PeerStorage.GetInputPeerById(userID)
	inputPeer, ok := peer.(*tg.InputPeerUser)
	if !ok {
		log.Printf("[Notify] AllowedUser %d cha Telegram peer cache madhe nahi - mail vachun aadhi ekda bot la message pathvava lagel", userID)
		return
	}

	_, err := Bot.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     inputPeer,
		Message:  text,
		RandomID: rand.Int63(),
	})
	if err != nil {
		log.Printf("[Notify] AllowedUser %d la Telegram DM pathavta ala nahi: %v", userID, err)
	}
}
