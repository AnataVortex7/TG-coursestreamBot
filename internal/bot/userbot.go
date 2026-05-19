package bot

import (
	"EverythingSuckz/fsb/config"
	"context"
	"errors"
	"math/rand"
	"strings"
	"time"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/sessionMaker"
	"github.com/celestix/gotgproto/storage"
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
	log.Info("Userbot started",
		zap.String("username", client.Self.Username),
		zap.String("FirstName", client.Self.FirstName),
		zap.String("LastName", client.Self.LastName),
	)
	if err := UserBot.AddBotsAsAdmins(); err != nil {
		log.Error("Failed to add bots as admins", zap.Error(err))
		return
	}
}

// GetClient returns the underlying gotgproto client, or nil if not running.
func (u *UserBotStruct) GetClient() *gotgproto.Client {
	return u.client
}

func (u *UserBotStruct) AddBotsAsAdmins() error {
	u.log.Info("Preparing to add bots as admins")
	ctx := u.client.CreateContext()
	channel := config.ValueOf.LogChannelID
	channelInfos, err := u.client.API().ChannelsGetChannels(
		ctx,
		[]tg.InputChannelClass{
			&tg.InputChannel{ChannelID: channel},
		},
	)
	if err != nil {
		u.log.Error("Failed to get channel info", zap.Error(err))
		return errors.New("failed to get channel info")
	}
	if len(channelInfos.GetChats()) == 0 {
		return errors.New("no channels found")
	}
	inputChannel := channelInfos.GetChats()[0].(*tg.Channel).AsInput()
	currentAdmins := []int64{}
	admins, err := u.client.API().ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
		Channel: inputChannel,
		Filter:  &tg.ChannelParticipantsAdmins{},
		Offset:  0,
		Limit:   100,
	})
	if err != nil {
		u.log.Error("Failed to get admins", zap.Error(err))
		return err
	}
	for _, admin := range admins.(*tg.ChannelsChannelParticipants).Participants {
		if user, ok := admin.(*tg.ChannelParticipantAdmin); ok {
			currentAdmins = append(currentAdmins, user.UserID)
		}
	}
	for _, bot := range Workers.Bots {
		isAdmin := false
		for _, admin := range currentAdmins {
			if admin == bot.Self.ID {
				u.log.Sugar().Infof("Bot @%s is already an admin", bot.Self.Username)
				isAdmin = true
				break
			}
		}
		if isAdmin {
			continue
		}
		botInfo, err := ctx.ResolveUsername(bot.Self.Username)
		if err != nil {
			u.log.Warn(err.Error())
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
			u.log.Sugar().Warnf("Failed to add @%s as admin", bot.Self.Username)
			u.log.Warn(err.Error())
		} else {
			u.log.Sugar().Infof("Added @%s as admin", bot.Self.Username)
		}
	}
	return nil
}

// ═══════════════════════════════════════════════════════════════════════════════
// RESTRICTED CHANNEL COPY
// ═══════════════════════════════════════════════════════════════════════════════

// CopyOptions controls caption replacement during copy.
// Find/Replace pairs: every occurrence of Find in the caption is replaced with Replace.
// Multiple pairs supported — e.g. [{"OldName", "NewName"}, {"@oldchannel", "@newchannel"}]
type CopyOptions struct {
	CaptionReplacements []CaptionReplace // caption madhe word replace
}

type CaptionReplace struct {
	Find    string
	Replace string
}

// CopyResult is returned after CopyRestrictedMessages finishes.
type CopyResult struct {
	Copied  int
	Skipped int
}

// CopyRestrictedMessages copies media messages from a restricted source channel
// to a target channel using the userbot account.
//
// Why not MessagesForwardMessages?
//   Restricted channels return CHAT_FORWARDS_RESTRICTED on forward attempts.
//   Instead we fetch the file reference via ChannelsGetMessages and re-send
//   using MessagesSendMedia — this bypasses the restriction.
//
// progressFn(copied, skipped) is called after every successful send.
// Pass nil if you don't need progress callbacks.
func (u *UserBotStruct) CopyRestrictedMessages(
	ctx context.Context,
	sourceChannelID int64,
	targetChannelID int64,
	startMsgID int,
	endMsgID int,
	opts CopyOptions,
	progressFn func(copied, skipped int),
) (CopyResult, error) {

	if u.client == nil {
		return CopyResult{}, errors.New("userbot is not running — set USER_SESSION in config")
	}

	// ── Resolve source channel ────────────────────────────────────────────────
	sourcePeer, err := u.resolveChannel(ctx, sourceChannelID)
	if err != nil {
		return CopyResult{}, err
	}

	// ── Resolve target channel ────────────────────────────────────────────────
	targetPeer, err := u.resolveChannel(ctx, targetChannelID)
	if err != nil {
		return CopyResult{}, err
	}
	targetInputPeer := &tg.InputPeerChannel{
		ChannelID:  targetPeer.ChannelID,
		AccessHash: targetPeer.AccessHash,
	}

	const (
		batchSize    = 100
		floodRetries = 5
	)

	result := CopyResult{}

	for batchStart := startMsgID; batchStart <= endMsgID; batchStart += batchSize {
		batchEnd := batchStart + batchSize - 1
		if batchEnd > endMsgID {
			batchEnd = endMsgID
		}

		ids := make([]tg.InputMessageClass, 0, batchEnd-batchStart+1)
		for id := batchStart; id <= batchEnd; id++ {
			ids = append(ids, &tg.InputMessageID{ID: id})
		}

		// ── Fetch batch with flood-wait retry ─────────────────────────────────
		var res tg.MessagesMessagesClass
		for attempt := 0; attempt < floodRetries; attempt++ {
			res, err = u.client.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
				Channel: sourcePeer,
				ID:      ids,
			})
			if err == nil {
				break
			}
			if wait := floodWaitSeconds(err.Error()); wait > 0 {
				time.Sleep(time.Duration(wait+2) * time.Second)
				continue
			}
			return result, err
		}
		if err != nil {
			return result, err
		}

		msgs, ok := res.(*tg.MessagesChannelMessages)
		if !ok {
			result.Skipped += batchEnd - batchStart + 1
			continue
		}

		// ── Send each message ──────────────────────────────────────────────────
		for _, rawMsg := range msgs.Messages {
			msg, ok := rawMsg.(*tg.Message)
			if !ok || msg.Media == nil {
				result.Skipped++
				continue
			}

			inputMedia, err := mediaToInput(msg.Media)
			if err != nil {
				// Non-media or unsupported type — skip silently
				result.Skipped++
				continue
			}

			caption := applyReplacements(msg.Message, opts.CaptionReplacements)

			// Retry send with flood-wait handling
			var sendErr error
			for attempt := 0; attempt < floodRetries; attempt++ {
				_, sendErr = u.client.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
					Peer:     targetInputPeer,
					Media:    inputMedia,
					Message:  caption,
					RandomID: rand.Int63(), // nolint: gosec
				})
				if sendErr == nil {
					break
				}
				if wait := floodWaitSeconds(sendErr.Error()); wait > 0 {
					time.Sleep(time.Duration(wait+2) * time.Second)
					continue
				}
				break
			}
			if sendErr != nil {
				result.Skipped++
				continue
			}

			result.Copied++
			if progressFn != nil {
				progressFn(result.Copied, result.Skipped)
			}

			// Telegram rate limiting: short pause between sends
			time.Sleep(500 * time.Millisecond)
		}
	}

	return result, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

// resolveChannel resolves a channel ID (with or without -100 prefix) to
// an InputChannel using the userbot's peer storage, then ChannelsGetChannels.
func (u *UserBotStruct) resolveChannel(ctx context.Context, channelID int64) (*tg.InputChannel, error) {
	mtprotoID := channelID
	if mtprotoID < 0 {
		mtprotoID = (-mtprotoID) - 1_000_000_000_000
	}

	// Try peer storage first
	ps, ok := u.client.PeerStorage.(*storage.PeerStorage)
	if ok {
		cached := ps.GetInputPeerById(mtprotoID)
		if peer, ok := cached.(*tg.InputPeerChannel); ok {
			return &tg.InputChannel{
				ChannelID:  peer.ChannelID,
				AccessHash: peer.AccessHash,
			}, nil
		}
	}

	// Fallback: ChannelsGetChannels
	channels, err := u.client.API().ChannelsGetChannels(ctx, []tg.InputChannelClass{
		&tg.InputChannel{ChannelID: mtprotoID},
	})
	if err != nil {
		return nil, err
	}
	if len(channels.GetChats()) == 0 {
		return nil, errors.New("channel not found")
	}
	ch, ok := channels.GetChats()[0].(*tg.Channel)
	if !ok {
		return nil, errors.New("unexpected type from ChannelsGetChannels")
	}
	return ch.AsInput(), nil
}

// mediaToInput converts a received MessageMedia to an InputMedia suitable
// for MessagesSendMedia.  It preserves the file reference so the file is
// NOT re-uploaded — Telegram reuses the existing server-side copy.
func mediaToInput(media tg.MessageMediaClass) (tg.InputMediaClass, error) {
	switch m := media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			return nil, errors.New("empty document")
		}
		return &tg.InputMediaDocument{
			ID: &tg.InputDocument{
				ID:            doc.ID,
				AccessHash:    doc.AccessHash,
				FileReference: doc.FileReference,
			},
		}, nil

	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			return nil, errors.New("empty photo")
		}
		return &tg.InputMediaPhoto{
			ID: &tg.InputPhoto{
				ID:            photo.ID,
				AccessHash:    photo.AccessHash,
				FileReference: photo.FileReference,
			},
		}, nil

	default:
		return nil, errors.New("unsupported media type")
	}
}

// applyReplacements applies all Find→Replace pairs to the caption string.
func applyReplacements(caption string, pairs []CaptionReplace) string {
	for _, p := range pairs {
		if p.Find != "" {
			caption = strings.ReplaceAll(caption, p.Find, p.Replace)
		}
	}
	return caption
}

// floodWaitSeconds parses FLOOD_WAIT_N from an error string.
// Returns 0 if the error is not a flood-wait.
func floodWaitSeconds(errStr string) int {
	const prefix = "FLOOD_WAIT_"
	idx := strings.Index(errStr, prefix)
	if idx < 0 {
		return 0
	}
	rest := errStr[idx+len(prefix):]
	n := 0
	for _, ch := range rest {
		if ch >= '0' && ch <= '9' {
			n = n*10 + int(ch-'0')
		} else {
			break
		}
	}
	return n
}
