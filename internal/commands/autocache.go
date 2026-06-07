package commands

import (
	"EverythingSuckz/fsb/internal/db"
	"EverythingSuckz/fsb/internal/utils"
	"strings"
	"time"

	"github.com/celestix/gotgproto/dispatcher"
	"github.com/celestix/gotgproto/dispatcher/handlers"
	"github.com/celestix/gotgproto/ext"
	"github.com/celestix/gotgproto/storage"
	"github.com/gotd/td/tg"
)

// LoadAutoCache — channel मध्ये नवीन message आल्यावर automatically
// DB च्या channel_cache मध्ये save करतो.
func (m *command) LoadAutoCache(d dispatcher.Dispatcher) {
	log := m.log.Named("autocache")
	defer log.Sugar().Info("Loaded auto-cache handler")

	d.AddHandler(handlers.NewMessage(nil, handleNewChannelMessage))
}

func handleNewChannelMessage(ctx *ext.Context, u *ext.Update) error {
	chatID := u.EffectiveChat().GetID()
	peer := ctx.PeerStorage.GetPeerById(chatID)

	// Channel नसेल तर nil return — पुढचे handlers (sendLink etc.) चालू राहतात
	if peer.Type != int(storage.TypeChannel) {
		return nil
	}

	msg := u.EffectiveMessage
	if msg == nil || msg.Media == nil {
		return dispatcher.EndGroups
	}

	// Media document किंवा photo असेल तरच cache करा
	switch msg.Media.(type) {
	case *tg.MessageMediaDocument, *tg.MessageMediaPhoto:
		// supported — continue
	default:
		return dispatcher.EndGroups
	}

	// या channel_id ला कोणता course linked आहे ते DB मधून शोधा
	course, err := db.GetCourseByChannelID(chatID)
	if err != nil || course == nil {
		return dispatcher.EndGroups
	}

	// File info काढा
	file, err := utils.FileFromMedia(msg.Media)
	if err != nil {
		return dispatcher.EndGroups
	}

	caption := strings.TrimSpace(msg.Text)
	title := caption
	if title == "" {
		title = stripExt(file.FileName)
	}

	cached := &db.CachedFile{
		MessageID: msg.ID,
		ChannelID: chatID,
		CourseID:  course.ID,
		Title:     title,
		Caption:   caption,
		FileName:  file.FileName,
		MimeType:  file.MimeType,
		FileSize:  file.FileSize,
		SortOrder: msg.ID,
		CreatedAt: time.Now(),
	}

	if err := db.UpsertCachedFile(cached); err != nil {
		utils.Logger.Sugar().Errorf(
			"[autocache] channel %d message %d save failed: %v",
			chatID, msg.ID, err,
		)
		return dispatcher.EndGroups
	}

	utils.Logger.Sugar().Infof(
		"[autocache] ✅ channel %d → course '%s' → message %d cached (%s)",
		chatID, course.Title, msg.ID, file.FileName,
	)

	return dispatcher.EndGroups
}

