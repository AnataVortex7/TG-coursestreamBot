// cmd/fsb/announce.go
//
// Deploy झाल्यावर सर्व admins + linked users ला एक छोटा status-update message
// पाठवतो ("bug fixed, सगळं व्यवस्थित काम करत आहे"). versionString (main.go)
// विरुद्ध DB मधला "शेवटचा announce केलेला version" match केला जातो — त्यामुळे
// फक्त खऱ्या नवीन deploy वर (versionString bump केल्यावर) एकदाच पाठवला जातो,
// crash/redeploy loop मध्ये प्रत्येक restart ला sपॅम होत नाही.

package main

import (
	"context"
	"math/rand"
	"time"

	"EverythingSuckz/fsb/config"
	"EverythingSuckz/fsb/internal/db"

	"github.com/celestix/gotgproto"
	"github.com/gotd/td/tg"
	"go.uber.org/zap"
)

const settingLastAnnouncedVersion = "last_announced_version"

// deployAnnouncementText — user/admin ला दिसणारा मेसेज.
const deployAnnouncementText = `
🎉 Bot Update!

📚 आता रोज Website वर जाण्याची गरज नाही.
तुम्ही आता Lectures **Bot** आणि **Website** दोन्ही ठिकाणी पाहू शकता.

🛡️ जोपर्यंत तुमचा Course Active आहे, तोपर्यंत Lectures Delete होणार नाहीत.

🆕 नवीन Features पाहण्यासाठी **/start** command वापरा.

💬 कृपया तुमचा Feedback नक्की पाठवा. काही नवीन Features किंवा Changes सुचवायचे असतील तर आम्हाला जरूर सांगा.

❤️ Thank you!
`;


// announceDeployIfNewVersion — versionString बदलला असेल तरच (नवीन deploy)
// broadcast पाठवतो; तोच version आधीच announce झाला असेल तर काहीच करत नाही.
func announceDeployIfNewVersion(botClient *gotgproto.Client, log *zap.Logger) {
	l := log.Named("DeployAnnounce")

	lastAnnounced := db.GetSettingString(settingLastAnnouncedVersion, "")
	if lastAnnounced == versionString {
		l.Sugar().Debugw("already announced this version, skipping", "version", versionString)
		return
	}

	sentTo := make(map[int64]bool)
	sentCount := 0

	// ── Admins आधी ──────────────────────────────────────────────────────
	for _, adminID := range config.ValueOf.AllowedUsers {
		if sendPlainAnnouncement(botClient, adminID, deployAnnouncementText) {
			sentCount++
		}
		sentTo[adminID] = true
		time.Sleep(150 * time.Millisecond) // flood-wait टाळायला थोडं थांबा
	}

	// ── मग सर्व Telegram-linked users ──────────────────────────────────
	users, err := db.GetAllLinkedUsers()
	if err != nil {
		l.Sugar().Errorw("GetAllLinkedUsers failed", "err", err)
	} else {
		for _, u := range users {
			if u.TelegramID == 0 || sentTo[u.TelegramID] {
				continue
			}
			if sendPlainAnnouncement(botClient, u.TelegramID, deployAnnouncementText) {
				sentCount++
			}
			sentTo[u.TelegramID] = true
			time.Sleep(150 * time.Millisecond)
		}
	}

	if err := db.SetSettingString(settingLastAnnouncedVersion, versionString); err != nil {
		l.Sugar().Errorw("SetSettingString failed — पुढच्या restart वर परत पाठवला जाऊ शकतो", "err", err)
	}
	l.Sugar().Infow("Deploy announcement sent", "version", versionString, "recipients", sentCount)
}

// sendPlainAnnouncement — एका chat ला साधा text message पाठवतो. Peer cache
// मध्ये नसेल (कधी संपर्क न झालेला user) तर silently skip करतो.
func sendPlainAnnouncement(botClient *gotgproto.Client, chatID int64, text string) bool {
	peer := botClient.PeerStorage.GetInputPeerById(chatID)
	if peer == nil {
		return false
	}
	sendCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := botClient.API().MessagesSendMessage(sendCtx, &tg.MessagesSendMessageRequest{
		Peer:     peer,
		Message:  text,
		RandomID: rand.Int63(),
	})
	return err == nil
}
