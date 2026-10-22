package bot

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"go.uber.org/zap"
)

type UserBotStruct struct {
	log    *zap.Logger
	cmu    sync.RWMutex // client pointer cha lock (Start/Stop/GetClient ekach veli yetat)
	client *gotgproto.Client
}

var UserBot *UserBotStruct = &UserBotStruct{}

// deadSessionCodes — Telegram ne session kayamcha maarla (user ne logout kela /
// account ban / key revoke). Faqt yachvelich DB madhla session delete karaycha.
// AUTH_KEY_DUPLICATED yat NAHI — to redeploy overlap mule yeto, session delete
// kelyas user cha login ugachach jato. To retry-able aahe.
var deadSessionCodes = []string{
	"AUTH_KEY_UNREGISTERED",
	"SESSION_REVOKED",
	"SESSION_EXPIRED",
	"USER_DEACTIVATED",
	"USER_DEACTIVATED_BAN",
}

// IsDeadSessionErr — session permanently invalid aahe ka.
func IsDeadSessionErr(err error) bool {
	if err == nil {
		return false
	}
	up := strings.ToUpper(err.Error())
	for _, c := range deadSessionCodes {
		if tgerr.Is(err, c) || strings.Contains(up, c) {
			return true
		}
	}
	return false
}

// IsDuplicatedErr — same session dusrya instance/connection varun vaparla jato aahe.
func IsDuplicatedErr(err error) bool {
	if err == nil {
		return false
	}
	return tgerr.Is(err, "AUTH_KEY_DUPLICATED") || strings.Contains(strings.ToUpper(err.Error()), "AUTH_KEY_DUPLICATED")
}

// connectSessionWithRetry — session string varun client start karto.
//   - dead session  => (nil, err, dead=true) — caller DB madhun delete karel
//   - AUTH_KEY_DUPLICATED / network => backoff var retry (jaruna instance band
//     honyachi vaat baghto), session KADHI delete hot nahi.
//   - NoAutoAuth=true: session invalid asel tar terminal phone prompt var
//     goroutine kayamcha atakat nahi.
func connectSessionWithRetry(log *zap.Logger, sessionStr string) (client *gotgproto.Client, dead bool, err error) {
	const maxAttempts = 6
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		apiID, apiHash := db.SessionAPICreds(sessionStr)
		client, err = gotgproto.NewClient(
			apiID,
			apiHash,
			gotgproto.ClientTypePhone(""),
			&gotgproto.ClientOpts{
				Session:          sessionMaker.PyrogramSession(sessionStr),
				DisableCopyright: true,
				NoAutoAuth:       true,
			},
		)
		if err == nil {
			return client, false, nil
		}
		if IsDeadSessionErr(err) {
			return nil, true, err
		}
		wait := time.Duration(attempt) * 10 * time.Second
		if IsDuplicatedErr(err) {
			log.Warn("AUTH_KEY_DUPLICATED — session dusrikade vaparla jat aahe, session jatan karto, retry",
				zap.Int("attempt", attempt), zap.Duration("wait", wait))
		} else {
			log.Warn("Userbot connect error — retry", zap.Int("attempt", attempt), zap.Duration("wait", wait), zap.Error(err))
		}
		if attempt < maxAttempts {
			time.Sleep(wait)
		}
	}
	return nil, false, err
}

func StartUserBot(l *zap.Logger) {
	log := l.Named("USERBOT")
	if config.ValueOf.UserSession == "" {
		log.Warn("User session is empty")
		return
	}
	log.Sugar().Infoln("Starting userbot")
	client, dead, err := connectSessionWithRetry(log, config.ValueOf.UserSession)
	if err != nil {
		log.Error("Failed to start userbot", zap.Error(err))
		if dead {
			log.Warn("Revoked/expired userbot session confirmed by Telegram — clearing session from DB")
			_ = db.DeleteUserBotSession()
			config.ValueOf.UserSession = ""
		}
		return
	}
	UserBot.log = log
	UserBot.setClient(client)
	log.Info("Userbot started", zap.String("username", client.Self.Username), zap.String("FirstName", client.Self.FirstName), zap.String("LastName", client.Self.LastName))
	if err := UserBot.AddBotsAsAdmins(); err != nil {
		log.Error("Failed to add bots as admins", zap.Error(err))
		return
	}
}

// setClient — navin client set karto; jaruna asel tar to properly STOP karto
// (nahitar jaruna connection chalu rahun same auth key doghat vaparla jato).
func (u *UserBotStruct) setClient(c *gotgproto.Client) {
	u.cmu.Lock()
	old := u.client
	u.client = c
	u.cmu.Unlock()
	if old != nil && old != c {
		old.Stop()
	}
}

// GetClient returns the underlying gotgproto client, or nil if the userbot is not running.
func (u *UserBotStruct) GetClient() *gotgproto.Client {
	u.cmu.RLock()
	defer u.cmu.RUnlock()
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
	PromoteWorkerBotsUsingMainBot(channelID, u.log)
	cl := u.GetClient()
	if cl == nil {
		u.log.Warn("AddBotsAsAdminsForChannel: userbot client running nahi - skip", zap.Int64("channel_id", channelID))
		return errors.New("userbot not running")
	}
	u.log.Sugar().Infof("Channel %d: userbot admin status check kartoy", channelID)
	ctx := cl.CreateContext()

	// DB/admin-panel madhe course channel ID Bot-API format madhe save hoто
	// (-100xxxxxxxxxx, @userinfobot deto tasach). MTProto cha ChannelsGetChannels
	// la matra raw channel ID (-100 prefix shivay) lagto, nahitar "no channels
	// found" yet — LOG_CHANNEL hyaआधीच config.go madhe stripInt() ne strip hoто
	// mhanun te direct chalte, course channels sathi ithech strip karावे lagते.
	mtprotoID := channelID
	if mtprotoID < 0 {
		mtprotoID = (-mtprotoID) - 1000000000000
	}

	channelInfos, err := cl.API().ChannelsGetChannels(
		ctx,
		[]tg.InputChannelClass{
			&tg.InputChannel{
				ChannelID: mtprotoID,
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
	selfParticipant, err := cl.API().ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{
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
	admins, err := cl.API().ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
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
		_, err = cl.API().ChannelsEditAdmin(
			cl.CreateContext().Context,
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

// AddBotsAsAdmins - userbot start/restart (redeploy) zalyavar automatic call hoto
// (StartUserBot, StartUserBotFromDB, StartFromSession).
//
// Flow:
//  1. LOG_CHANNEL sathi sync (purvi cha behavior, jasach aahe).
//  2. DB madhle SAGLE active courses fetch karto (db.ListAllCourses) ani
//     pratyek course chya channel_id sathi pn AddBotsAsAdminsForChannel call
//     karto. Yamule redeploy/restart झाल्यावर LOG_CHANNEL + sagle course
//     channels — sarvatra sagle bots (env varun navin add zalele multi-token
//     bots dharun) admin aahet ka recheck hoто, nasel tar admin banवले jातात.
//     Already admin asलेले bots skip hotat (AddBotsAsAdminsForChannel madhe
//     to check already aahe).
//  3. Pratyek channel nantar chota delay (flood-wait टाळण्यासाठी), kaaran
//     khup courses asतील tar Telegram कडून FLOOD_WAIT yeu shakto.
func (u *UserBotStruct) AddBotsAsAdmins() error {
	if err := u.AddBotsAsAdminsForChannel(config.ValueOf.LogChannelID); err != nil {
		u.log.Warn("AddBotsAsAdmins: LOG_CHANNEL sync madhe issue", zap.Error(err))
	}

	courses, err := db.ListAllCourses()
	if err != nil {
		u.log.Error("AddBotsAsAdmins: courses list nahi milali", zap.Error(err))
		return err
	}

	for _, course := range courses {
		if course.ChannelID == 0 {
			continue
		}
		if err := u.AddBotsAsAdminsForChannel(course.ChannelID); err != nil {
			u.log.Warn("AddBotsAsAdmins: course channel sync madhe issue",
				zap.Int64("channel_id", course.ChannelID),
				zap.String("course", course.Title),
				zap.Error(err))
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return nil
}

func PromoteWorkerBotsUsingMainBot(channelID int64, log *zap.Logger) {
	if Workers == nil || len(Workers.Bots) <= 1 {
		return
	}
	mainBot := Workers.Bots[0]
	if mainBot == nil || mainBot.Client == nil {
		return
	}

	ctx := mainBot.Client.CreateContext()

	mtprotoID := channelID
	if mtprotoID < 0 {
		mtprotoID = (-mtprotoID) - 1000000000000
	}

	channelInfos, err := mainBot.Client.API().ChannelsGetChannels(
		ctx,
		[]tg.InputChannelClass{
			&tg.InputChannel{
				ChannelID: mtprotoID,
			},
		},
	)
	if err != nil || len(channelInfos.GetChats()) == 0 {
		return
	}

	channelChat, ok := channelInfos.GetChats()[0].(*tg.Channel)
	if !ok {
		return
	}
	inputChannel := channelChat.AsInput()

	selfParticipant, err := mainBot.Client.API().ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{
		Channel:     inputChannel,
		Participant: &tg.InputPeerSelf{},
	})
	if err != nil {
		return
	}

	canAddAdmins := false
	switch p := selfParticipant.Participant.(type) {
	case *tg.ChannelParticipantCreator:
		canAddAdmins = true
	case *tg.ChannelParticipantAdmin:
		canAddAdmins = p.AdminRights.AddAdmins
	}

	if !canAddAdmins {
		return
	}

	currentAdmins := map[int64]bool{}
	admins, err := mainBot.Client.API().ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
		Channel: inputChannel,
		Filter:  &tg.ChannelParticipantsAdmins{},
		Offset:  0,
		Limit:   100,
	})
	if err == nil {
		if adminList, ok := admins.(*tg.ChannelsChannelParticipants); ok {
			for _, admin := range adminList.Participants {
				switch a := admin.(type) {
				case *tg.ChannelParticipantAdmin:
					currentAdmins[a.UserID] = true
				case *tg.ChannelParticipantCreator:
					currentAdmins[a.UserID] = true
				}
			}
		}
	}

	for i := 1; i < len(Workers.Bots); i++ {
		bot := Workers.Bots[i]
		if currentAdmins[bot.Self.ID] {
			continue
		}

		botInfo, err := ctx.ResolveUsername(bot.Self.Username)
		if err != nil {
			continue
		}

		_, err = mainBot.Client.API().ChannelsEditAdmin(
			ctx.Context,
			&tg.ChannelsEditAdminRequest{
				Channel: inputChannel,
				UserID:  botInfo.GetInputUser(),
				AdminRights: tg.ChatAdminRights{
					PostMessages: true,
					EditMessages: true,
					DeleteMessages: true,
				},
				Rank: "worker",
			},
		)
		if err == nil {
			log.Sugar().Infof("MainBot promoted worker @%s as admin in channel %d", bot.Self.Username, channelID)
		}
	}
}
