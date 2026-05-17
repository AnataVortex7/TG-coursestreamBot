package commands

import (
	"EverythingSuckz/fsb/internal/utils"
	"math/rand"
	"strconv"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/gotd/td/tg"
)

// LoadGetVideo registers the /getvideo command
func (m *command) LoadGetVideo(d dispatcher.Dispatcher) {
	log := m.log.Named("getvideo")
	defer log.Sugar().Info("Loaded /getvideo command")
	d.AddHandler(handlers.NewCommand("getvideo", cmdGetVideo))
}

// /getvideo <channel_id> <msg_id>
func cmdGetVideo(ctx *ext.Context, u *ext.Update) error {
	a := args(u)
	if len(a) < 2 {
		reply(ctx, u,
			"Usage: /getvideo <channel_id> <msg_id>\n\n"+
				"Example:\n/getvideo -1001234567890 42")
		return dispatcher.EndGroups
	}

	channelID, err := strconv.ParseInt(a[0], 10, 64)
	if err != nil {
		reply(ctx, u, "❌ Invalid channel_id. Example: -1001234567890")
		return dispatcher.EndGroups
	}

	msgID, err := strconv.Atoi(a[1])
	if err != nil || msgID < 1 {
		reply(ctx, u, "❌ Invalid msg_id. Must be a positive number.")
		return dispatcher.EndGroups
	}

	botClient := MainBotClient
	if botClient == nil {
		reply(ctx, u, "❌ Bot client not running.")
		return dispatcher.EndGroups
	}

	// Resolve channel peer
	channel, err := utils.GetChannelPeer(ctx, botClient.API(), botClient.PeerStorage, channelID)
	if err != nil {
		reply(ctx, u, "❌ Cannot access channel: "+err.Error()+
			"\n\nMake sure the bot is admin/member in that channel.")
		return dispatcher.EndGroups
	}

	// Fetch the specific message
	res, err := botClient.API().ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: channel,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: msgID}},
	})
	if err != nil {
		reply(ctx, u, "❌ Failed to fetch message: "+err.Error())
		return dispatcher.EndGroups
	}

	messages, ok := res.(*tg.MessagesChannelMessages)
	if !ok || len(messages.Messages) == 0 {
		reply(ctx, u, "❌ Message not found.")
		return dispatcher.EndGroups
	}

	msg, ok := messages.Messages[0].(*tg.Message)
	if !ok || msg.Media == nil {
		reply(ctx, u, "❌ No media found in that message.")
		return dispatcher.EndGroups
	}

	// msg.Media is MessageMediaClass — need to convert to InputMedia
	// Use InputMediaDocument or InputMediaPhoto depending on type
	var inputMedia tg.InputMediaClass

	switch m := msg.Media.(type) {
	case *tg.MessageMediaDocument:
		doc, ok := m.Document.(*tg.Document)
		if !ok {
			reply(ctx, u, "❌ Could not read document.")
			return dispatcher.EndGroups
		}
		inputMedia = &tg.InputMediaDocument{
			ID: &tg.InputDocument{
				ID:         doc.ID,
				AccessHash: doc.AccessHash,
				FileReference: doc.FileReference,
			},
		}
	case *tg.MessageMediaPhoto:
		photo, ok := m.Photo.(*tg.Photo)
		if !ok {
			reply(ctx, u, "❌ Could not read photo.")
			return dispatcher.EndGroups
		}
		inputMedia = &tg.InputMediaPhoto{
			ID: &tg.InputPhoto{
				ID:         photo.ID,
				AccessHash: photo.AccessHash,
				FileReference: photo.FileReference,
			},
		}
	default:
		reply(ctx, u, "❌ Unsupported media type.")
		return dispatcher.EndGroups
	}

	toPeer := ctx.PeerStorage.GetInputPeerById(u.EffectiveChat().GetID())

	_, err = botClient.API().MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer:       toPeer,
		Media:      inputMedia,
		Message:    msg.Message, // original caption (if any)
		Noforwards: true,        // protect_content: no forward, no download
		RandomID:   rand.Int63(),
	})
	if err != nil {
		reply(ctx, u, "❌ Failed to send video: "+err.Error())
		return dispatcher.EndGroups
	}

	return dispatcher.EndGroups
}
