package bot

import (
	"EverythingSuckz/fsb/config"
	"errors"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

type UserBotStruct struct {
	log    *zap.Logger
	client *gotgproto.Client
}

var UserBot *UserBotStruct = &UserBotStruct{}

func StartUserBot(l *zap.Logger) {
	log := l.Named("USERBOT")
	if config.ValueOf.UserSession == "" {
		log.Warn("User session is empty")
		return
	}
	log.Sugar().Infoln("Starting userbot")
	client, err := gotgproto.NewClient(
		int(config.ValueOf.ApiID),
		config.ValueOf.ApiHash,
		gotgproto.ClientTypePhone(""),
		&gotgproto.ClientOpts{
			Session:          sessionMaker.PyrogramSession(config.ValueOf.UserSession),
			DisableCopyright: true,
		},
	)
	if err != nil {
		log.Error("Failed to start userbot", zap.Error(err))
		return
	}
	UserBot.log = log
	UserBot.client = client
	log.Info("Userbot started", zap.String("username", client.Self.Username), zap.String("FirstName", client.Self.FirstName), zap.String("LastName", client.Self.LastName))
	if err := UserBot.AddBotsAsAdmins(); err != nil {
		log.Error("Failed to add bots as admins", zap.Error(err))
		return
	}
}

// GetClient returns the underlying gotgproto client, or nil if the userbot is not running.
func (u *UserBotStruct) GetClient() *gotgproto.Client {
	return u.client
}

// AddBotsAsAdminsForChannel pratyek course channel sathi vaparla jato.
//
// Flow:
//  1. Userbot cha swatahcha participant status tya channel madhe check karto.
//     - Userbot creator/owner asel, KIVA admin asel "add admins" rights sobat
//       -> sagle bots (main bot + multi-token worker bots, Workers.Bots) loop
//       karun je already admin nahit tyanna admin banवto. Je already admin
//       aahet tyanna skip karun log madhe "already admin" print karto.
//     - Userbot admin NASEL (ya channel access nasel) tar -> kahihi modify na
//       karta, channel ID warning level log madhe print karto, ani return.
func (u *UserBotStruct) AddBotsAsAdminsForChannel(channelID int64) error {
	if u.client == nil {
		u.log.Warn("AddBotsAsAdminsForChannel: userbot client running nahi - skip", zap.Int64("channel_id", channelID))
		return errors.New("userbot not running")
	}
	u.log.Sugar().Infof("Channel %d: userbot admin status check kartoy", channelID)
	ctx := u.client.CreateContext()

	channelInfos, err := u.client.API().ChannelsGetChannels(
		ctx,
		[]tg.InputChannelClass{
			&tg.InputChannel{
				ChannelID: channelID,
			},
		},
	)
	if err != nil {
		u.log.Error("AddBotsAsAdminsForChannel: channel info nahi milali", zap.Int64("channel_id", channelID), zap.Error(err))
		return errors.New("failed to get channel info")
	}
	if len(channelInfos.GetChats()) == 0 {
		return errors.New("no channels found")
	}
	channelChat, ok := channelInfos.GetChats()[0].(*tg.Channel)
	if !ok {
		return errors.New("unexpected chat type")
	}
	inputChannel := channelChat.AsInput()

	// Step 1: Userbot swatah admin/owner aahe ka check karto.
	selfParticipant, err := u.client.API().ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{
		Channel:     inputChannel,
		Participant: &tg.InputPeerSelf{},
	})
	if err != nil {
		u.log.Warn("CHANNEL ADMIN NAHI (userbot ya channel cha member/admin nahi) - bots manually admin karावे lagतील",
			zap.Int64("channel_id", channelID), zap.Error(err))
		return nil
	}

	canAddAdmins := false
	switch p := selfParticipant.Participant.(type) {
	case *tg.ChannelParticipantCreator:
		canAddAdmins = true
	case *tg.ChannelParticipantAdmin:
		canAddAdmins = p.AdminRights.AddAdmins
	}

	if !canAddAdmins {
		u.log.Warn("CHANNEL ADMIN NAHI (userbot admin nahi / 'add admins' right nahi) - bots manually admin karावे lagतील",
			zap.Int64("channel_id", channelID))
		return nil
	}

	u.log.Sugar().Infof("Channel %d: userbot admin/owner aahe - bots admin karaychya process suru", channelID)

	// Step 2: Already admin konte bots aahet te check karto.
	currentAdmins := []int64{}
	admins, err := u.client.API().ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
		Channel: inputChannel,
		Filter:  &tg.ChannelParticipantsAdmins{},
		Offset:  0,
		Limit:   100,
	})
	if err != nil {
		u.log.Error("AddBotsAsAdminsForChannel: admins list nahi milali", zap.Int64("channel_id", channelID), zap.Error(err))
		return err
	}
	if adminList, ok := admins.(*tg.ChannelsChannelParticipants); ok {
		for _, admin := range adminList.Participants {
			switch a := admin.(type) {
			case *tg.ChannelParticipantAdmin:
				currentAdmins = append(currentAdmins, a.UserID)
			case *tg.ChannelParticipantCreator:
				currentAdmins = append(currentAdmins, a.UserID)
			}
		}
	}

	// Step 3: Pratyek worker bot (main bot + multi-token bots) loop.
	for _, bot := range Workers.Bots {
		isAdmin := false
		for _, admin := range currentAdmins {
			if admin == bot.Self.ID {
				isAdmin = true
				break
			}
		}
		if isAdmin {
			u.log.Sugar().Infof("Channel %d: @%s already admin", channelID, bot.Self.Username)
			continue
		}

		botInfo, err := ctx.ResolveUsername(bot.Self.Username)
		if err != nil {
			u.log.Warn("AddBotsAsAdminsForChannel: username resolve nahi zala", zap.String("bot", bot.Self.Username), zap.Error(err))
			continue
		}
		_, err = u.client.API().ChannelsEditAdmin(
			u.client.CreateContext().Context,
			&tg.ChannelsEditAdminRequest{
				Channel: inputChannel,
				UserID:  botInfo.GetInputUser(),
				AdminRights: tg.ChatAdminRights{
					PostMessages: true,
				},
				Rank: "admin",
			},
		)
		if err != nil {
			u.log.Warn("AddBotsAsAdminsForChannel: bot admin karta nahi aala", zap.Int64("channel_id", channelID), zap.String("bot", bot.Self.Username), zap.Error(err))
			continue
		}
		u.log.Sugar().Infof("Channel %d: @%s admin banवla", channelID, bot.Self.Username)
	}
	return nil
}

// AddBotsAsAdmins - purvi cha behavior: fakt LOG_CHANNEL sathi chalто.
// Userbot start/restart zalyavar automatic call hoto (StartUserBot, StartUserBotFromDB).
func (u *UserBotStruct) AddBotsAsAdmins() error {
	return u.AddBotsAsAdminsForChannel(config.ValueOf.LogChannelID)
}
