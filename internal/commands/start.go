package commands

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/utils"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/storage"
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
	if len(config.ValueOf.AllowedUsers) != 0 && !utils.Contains(config.ValueOf.AllowedUsers, chatId) {
		ctx.Reply(u, ext.ReplyTextString("You are not allowed to use this bot."), nil)
		return dispatcher.EndGroups
	}
	ctx.Reply(u, ext.ReplyTextString(
		"👋 Welcome!\n\n"+
			"📥 Offline Video Download साठी:\n\n"+
			"1️⃣ तुमचा mobile/email टाका:\n"+
			"   /register 9876543210\n\n"+
			"2️⃣ Admin link केल्यावर website वर जा → course उघडा → 📥 Offline button दाबा → token मिळेल\n\n"+
			"3️⃣ Token इथे टाका:\n"+
			"   /dl 48271\n\n"+
			"❓ तुमचा Telegram ID बघायला: /myid",
	), nil)
	return dispatcher.EndGroups
}
