package bot

// internal/bot/peer_warmer.go
//
// PROBLEM: gotgproto cha PeerStorage ek local sqlite file (fsb.session) madhe
// store hoто (internal/bot/client.go). Hosting platform var (Render/Railway/
// Koyeb etc.) container cha filesystem ephemeral asto — pratyek redeploy var
// navin container suru hoто, ani fsb.session खाली पासूनच navin/empty banतो.
// Yamule pratyek redeploy nantar PeerStorage la KOणत्याही user cha access_hash
// mahit nasто — Bot.PeerStorage.GetInputPeerById() admin chya ID sathi fail
// hoते, ani "/Notify] AllowedUser ... peer cache madhe nahi" warning yete.
// Adhi ekda admin ne bot la manually message pathवला ki Telegram cha update
// madhun access_hash milто ani peer cache madhe save hoто — pan tе pratyek
// redeploy var परत manually karaव lagते.
//
// FIX: Server start zhalyavar (userbot LOG_CHANNEL admin-sync zhalyavar),
// Bot (main bot, BOT_TOKEN) स्वतः LOG_CHANNEL cha member/admin असतो — tyamule
// to channel cha participant list fetch karू shakто. Ya list madhun jya
// users cha ID ALLOWED_USERS madhe match hoто, tyancha access_hash directly
// PeerStorage madhe save kela jato — admin ne manually message na pathvताही.
//
// PREREQUISITE: ALLOWED_USERS (admins) LOG_CHANNEL cha member असaayla hावेत.
// Nasतील tar he silently skip hoते (warning log hoто), manual-message-once
// fallback tasaच rahato.

import (
	"EverythingSuckz/fsb/config"
	"time"

	"github.com/celestix/gotgproto/storage"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

// WarmAdminPeerCache — LOG_CHANNEL chya members madhun ALLOWED_USERS chा
// access_hash kadhun Bot cha PeerStorage warm karto. Server start zhalyavar
// (userbot cha LOG_CHANNEL admin-sync zhalyanantar) ek vela call hoto.
func WarmAdminPeerCache(log *zap.Logger) {
	l := log.Named("PeerWarmer")
	if Bot == nil {
		l.Warn("Bot client running nahi - skip")
		return
	}
	if len(config.ValueOf.AllowedUsers) == 0 {
		return
	}

	ctx := Bot.CreateContext()

	channelInfos, err := Bot.API().ChannelsGetChannels(ctx, []tg.InputChannelClass{
		&tg.InputChannel{ChannelID: config.ValueOf.LogChannelID},
	})
	if err != nil {
		l.Warn("LOG_CHANNEL info nahi milali - Bot tya channel cha member nasel",
			zap.Error(err))
		return
	}
	if len(channelInfos.GetChats()) == 0 {
		l.Warn("LOG_CHANNEL sapadla nahi")
		return
	}
	channelChat, ok := channelInfos.GetChats()[0].(*tg.Channel)
	if !ok {
		l.Warn("LOG_CHANNEL unexpected chat type")
		return
	}
	inputChannel := channelChat.AsInput()

	participants, err := Bot.API().ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
		Channel: inputChannel,
		Filter:  &tg.ChannelParticipantsRecent{},
		Offset:  0,
		Limit:   200,
	})
	if err != nil {
		l.Warn("LOG_CHANNEL participants nahi milale", zap.Error(err))
		return
	}
	full, ok := participants.(*tg.ChannelsChannelParticipants)
	if !ok {
		return
	}

	warmed := 0
	for _, uc := range full.Users {
		usr, ok := uc.(*tg.User)
		if !ok {
			continue
		}
		for _, allowedID := range config.ValueOf.AllowedUsers {
			if usr.ID == allowedID {
				Bot.PeerStorage.AddPeer(usr.ID, usr.AccessHash, storage.TypeUser, usr.Username)
				warmed++
				break
			}
		}
	}

	if warmed > 0 {
		l.Sugar().Infof("Peer cache warmed for %d allowed admin(s) — manual message yapudhe lagणार nahi", warmed)
	} else {
		l.Warn("Konthahi ALLOWED_USERS LOG_CHANNEL cha member sapadla nahi — manual-message-once fallback lagू shakto")
	}
}

// WarmAdminPeerCacheDelayed — userbot cha LOG_CHANNEL admin-sync (jo background
// madhe chalто) purna होण्यासाठी थोडा vेळ देऊन मग warm karto. Direct call
// kelyas userbot admin sync abhi pUrna न झाल्यामुळे Bot LOG_CHANNEL cha
// member nasel he race condition टाळण्यासाठी.
func WarmAdminPeerCacheDelayed(log *zap.Logger, delay time.Duration) {
	go func() {
		time.Sleep(delay)
		WarmAdminPeerCache(log)
	}()
}
