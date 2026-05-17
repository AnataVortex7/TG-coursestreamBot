package utils

import (
	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/cache"
	"EverythingSuckz/fsb/internal/types"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"

	"github.com/celestix/gotgproto"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/storage"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

// https://stackoverflow.com/a/70802740/15807350
func Contains[T comparable](s []T, e T) bool {
	for _, v := range s {
		if v == e {
			return true
		}
	}
	return false
}

// IsClientDisconnectError checks if the error is due to client disconnecting
// e.g. user seeking in video, stopping playback, or network issues on client side
func IsClientDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "connection was aborted") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "broken pipe") ||
		strings.Contains(errStr, "forcibly closed")
}

// telegram helper functions
// TODO: move these to a separate package if they grow too large

func GetTGMessage(ctx context.Context, client *gotgproto.Client, messageID int) (*tg.Message, error) {
	return GetTGMessageInChannel(ctx, client, messageID, config.ValueOf.LogChannelID)
}

func GetTGMessageInChannel(ctx context.Context, client *gotgproto.Client, messageID int, channelID int64) (*tg.Message, error) {
	inputMessageID := tg.InputMessageClass(&tg.InputMessageID{ID: messageID})
	channel, err := GetChannelPeer(ctx, client.API(), client.PeerStorage, channelID)
	if err != nil {
		return nil, err
	}
	messageRequest := tg.ChannelsGetMessagesRequest{Channel: channel, ID: []tg.InputMessageClass{inputMessageID}}
	res, err := client.API().ChannelsGetMessages(ctx, &messageRequest)
	if err != nil {
		return nil, err
	}
	messages := res.(*tg.MessagesChannelMessages)
	message := messages.Messages[0]
	if _, ok := message.(*tg.Message); ok {
		return message.(*tg.Message), nil
	} else {
		return nil, fmt.Errorf("this file was deleted")
	}
}

func FileFromMedia(media tg.MessageMediaClass) (*types.File, error) {
	switch media := media.(type) {
	case *tg.MessageMediaDocument:
		document, ok := media.Document.AsNotEmpty()
		if !ok {
			return nil, fmt.Errorf("unexpected type %T", media)
		}
		var fileName string
		for _, attribute := range document.Attributes {
			if name, ok := attribute.(*tg.DocumentAttributeFilename); ok {
				fileName = name.FileName
				break
			}
		}
		return &types.File{
			Location:      document.AsInputDocumentFileLocation(),
			FileSize:      document.Size,
			FileName:      fileName,
			MimeType:      document.MimeType,
			ID:            document.ID,
			FileType:      "document",
			AccessHash:    document.AccessHash,
			FileReference: document.FileReference,
			ThumbSize:     "",
			DCID:          document.DCID,
		}, nil
	case *tg.MessageMediaPhoto:
		photo, ok := media.Photo.AsNotEmpty()
		if !ok {
			return nil, fmt.Errorf("unexpected type %T", media)
		}
		sizes := photo.Sizes
		if len(sizes) == 0 {
			return nil, errors.New("photo has no sizes")
		}
		photoSize := sizes[len(sizes)-1]
		size, ok := photoSize.AsNotEmpty()
		if !ok {
			return nil, errors.New("photo size is empty")
		}
		location := new(tg.InputPhotoFileLocation)
		location.ID = photo.GetID()
		location.AccessHash = photo.GetAccessHash()
		location.FileReference = photo.GetFileReference()
		location.ThumbSize = size.GetType()
		return &types.File{
			Location:      location,
			FileSize:      0, // caller should judge if this is a photo or not
			FileName:      fmt.Sprintf("photo_%d.jpg", photo.GetID()),
			MimeType:      "image/jpeg",
			ID:            photo.GetID(),
			FileType:      "photo",
			AccessHash:    photo.GetAccessHash(),
			FileReference: photo.GetFileReference(),
			ThumbSize:     size.GetType(),
			DCID:          photo.GetDCID(),
		}, nil
	}
	return nil, fmt.Errorf("unexpected type %T", media)
}

func FileFromMessage(ctx context.Context, client *gotgproto.Client, messageID int) (*types.File, error) {
	return FileFromMessageInChannel(ctx, client, messageID, config.ValueOf.LogChannelID)
}

// FileFromMessageInChannel fetches file metadata from any channel (not just LOG_CHANNEL).
// Used when each course has its own dedicated Telegram channel.
func FileFromMessageInChannel(ctx context.Context, client *gotgproto.Client, messageID int, channelID int64) (*types.File, error) {
	key := fmt.Sprintf("file:%d:%d:%d", messageID, channelID, client.Self.ID)
	log := Logger.Named("GetMessageMedia")
	var cachedMedia types.File
	err := cache.GetCache().Get(key, &cachedMedia)
	if err == nil {
		log.Debug("Using cached media message properties", zap.Int("messageID", messageID), zap.Int64("clientID", client.Self.ID))
		return &cachedMedia, nil
	}
	log.Debug("Fetching file properties from message ID", zap.Int("messageID", messageID), zap.Int64("clientID", client.Self.ID))

	channel, err := GetChannelPeer(ctx, client.API(), client.PeerStorage, channelID)
	if err != nil {
		return nil, err
	}

	inputMessageID := tg.InputMessageClass(&tg.InputMessageID{ID: messageID})
	messageRequest := tg.ChannelsGetMessagesRequest{
		Channel: &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
		ID:      []tg.InputMessageClass{inputMessageID},
	}
	res, err := client.API().ChannelsGetMessages(ctx, &messageRequest)
	if err != nil {
		return nil, err
	}
	messages := res.(*tg.MessagesChannelMessages)
	if len(messages.Messages) == 0 {
		return nil, fmt.Errorf("message %d not found in channel %d", messageID, channelID)
	}
	message, ok := messages.Messages[0].(*tg.Message)
	if !ok {
		return nil, fmt.Errorf("this file was deleted")
	}
	file, err := FileFromMedia(message.Media)
	if err != nil {
		return nil, err
	}
	err = cache.GetCache().Set(key, file, 3600)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func GetLogChannelPeer(ctx context.Context, api *tg.Client, peerStorage *storage.PeerStorage) (*tg.InputChannel, error) {
	return GetChannelPeer(ctx, api, peerStorage, config.ValueOf.LogChannelID)
}

// GetChannelPeer resolves any Telegram channel by ID.
func GetChannelPeer(ctx context.Context, api *tg.Client, peerStorage *storage.PeerStorage, channelID int64) (*tg.InputChannel, error) {
    
    // -100 prefix strip kar MTProto saathi
    mtprotoID := channelID
    if mtprotoID < 0 {
        mtprotoID = (-mtprotoID) - 1000000000000
    }

    // peer storage madhe mtprotoID ne search kar
    cachedInputPeer := peerStorage.GetInputPeerById(mtprotoID)

    switch peer := cachedInputPeer.(type) {
    case *tg.InputPeerEmpty:
        break
    case *tg.InputPeerChannel:
        return &tg.InputChannel{
            ChannelID:  peer.ChannelID,
            AccessHash: peer.AccessHash,
        }, nil
    default:
        return nil, errors.New("unexpected type of input peer")
    }

    // ChannelsGetChannels la pan mtprotoID pathav
    inputChannel := &tg.InputChannel{
        ChannelID: mtprotoID,  // ✅ correct format
    }
    channels, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{inputChannel})
    if err != nil {
        return nil, err
    }
    if len(channels.GetChats()) == 0 {
        return nil, errors.New("no channels found")
    }
    channel, ok := channels.GetChats()[0].(*tg.Channel)
    if !ok {
        return nil, errors.New("type assertion to *tg.Channel failed")
    }
    peerStorage.AddPeer(channel.GetID(), channel.AccessHash, storage.TypeChannel, "")
    return channel.AsInput(), nil
}
func ForwardMessages(ctx *ext.Context, fromChatId, toChatId int64, messageID int) (*tg.Updates, error) {
	fromPeer := ctx.PeerStorage.GetInputPeerById(fromChatId)
	if fromPeer.Zero() {
		return nil, fmt.Errorf("fromChatId: %d is not a valid peer", fromChatId)
	}
	toPeer, err := GetLogChannelPeer(ctx, ctx.Raw, ctx.PeerStorage)
	if err != nil {
		return nil, err
	}
	update, err := ctx.Raw.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
		RandomID: []int64{rand.Int63()},
		FromPeer: fromPeer,
		ID:       []int{messageID},
		ToPeer:   &tg.InputPeerChannel{ChannelID: toPeer.ChannelID, AccessHash: toPeer.AccessHash},
	})
	if err != nil {
		return nil, err
	}
	return update.(*tg.Updates), nil
}
