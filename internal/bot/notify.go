package bot

import (
	"EverythingSuckz/fsb/config"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"time"

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
	adminIDs := config.ValueOf.AllowedUsers
	botToken := config.ValueOf.BotToken
	if len(adminIDs) == 0 {
		log.Printf("[Notify] Warning: AllowedUsers list is empty, notification skipped")
		return
	}
	for _, id := range adminIDs {
		adminID := id
		go func() {
			if err := SendTelegramDM(adminID, text); err != nil {
				log.Printf("[Notify] AllowedUser %d MTProto DM failed: %v — trying HTTP fallback", adminID, err)
				if botToken != "" {
					if hErr := sendHTTPDM(botToken, adminID, text); hErr != nil {
						log.Printf("[Notify] AllowedUser %d HTTP DM also failed: %v", adminID, hErr)
					}
				}
			}
		}()
	}
}

// SendTelegramDM — kontyahi Telegram userID la plain text DM pathavto.
// Admin (ALLOWED_USERS) sathi NotifyAllowedUsers vaprto, ani website users
// sathi (TelegramID linked asel tar) adminBroadcast / adminSendMessageToUser
// madhun direct call hoto. Peer cache aagodarच WarmAdminPeerCache /
// WarmUserPeerCache ne warm kelela aste, tyamule user ne punha message
// karaychi garaj nasते.
//
// Peer cache madhe sapadla nahi tar error return karto — caller (email
// sarkhech) best-effort म्हणून ignore/log karू shakto, request fail hot nahi.
func SendTelegramDM(userID int64, text string) error {
	if Bot == nil {
		botToken := config.ValueOf.BotToken
		if botToken != "" && userID > 0 {
			return sendHTTPDM(botToken, userID, text)
		}
		return fmt.Errorf("bot client running nahi")
	}
	if userID == 0 {
		return fmt.Errorf("invalid telegram userID: 0")
	}

	peer := Bot.PeerStorage.GetInputPeerById(userID)
	inputPeer, ok := peer.(*tg.InputPeerUser)
	if ok {
		ctx := Bot.CreateContext()
		_, err := Bot.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     inputPeer,
			Message:  text,
			RandomID: rand.Int63(),
		})
		if err == nil {
			return nil
		}
	}

	// [FALLBACK] MTProto peer cache miss or error — HTTP Bot API चा वापर करून थेट DM पाठवतो
	botToken := config.ValueOf.BotToken
	if botToken != "" {
		return sendHTTPDM(botToken, userID, text)
	}
	return fmt.Errorf("telegram peer cache madhe nahi (user %d)", userID)
}

func SendTelegramDMWithMarkup(userID int64, text string, markup tg.ReplyMarkupClass) error {
	if Bot == nil || userID == 0 {
		return SendTelegramDM(userID, text)
	}
	peer := Bot.PeerStorage.GetInputPeerById(userID)
	inputPeer, ok := peer.(*tg.InputPeerUser)
	if ok {
		ctx := Bot.CreateContext()
		_, err := Bot.API().MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:        inputPeer,
			Message:     text,
			ReplyMarkup: markup,
			RandomID:    rand.Int63(),
		})
		if err == nil {
			return nil
		}
	}
	return SendTelegramDM(userID, text)
}

func sendHTTPDM(botToken string, userID int64, text string) error {
	if botToken == "" || userID == 0 || strings.TrimSpace(text) == "" {
		return fmt.Errorf("empty botToken, userID, or text provided")
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	payload, err := json.Marshal(map[string]interface{}{
		"chat_id": userID,
		"text":    text,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP sendMessage status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// SendOTPToTelegram — website login OTP थेट user च्या Telegram chat वर पाठवतो.
// email ऐवजी हाच एकमेव OTP delivery channel आहे. Peer cache आधीच WarmUserPeerCache
// ने warm केलेला असतो (linked users साठी), त्यामुळे redeploy नंतरही लगेच काम करतो.
func SendOTPToTelegram(telegramID int64, otpPlain string) error {
	text := fmt.Sprintf(
		"🔐 *Login OTP*\n\n"+
			"तुमचा code: `%s`\n\n"+
			"⏳ 10 मिनिटांत expire होईल.\n"+
			"🚫 हा OTP कुणालाही (agent/admin सकट) share करू नका.",
		otpPlain,
	)
	return SendTelegramDM(telegramID, text)
}

// SetAttractiveBotMenuCommands — Telegram chat bar मध्ये दिसायला सुंदर Menu बटन सेट करतो
func SetAttractiveBotMenuCommands(botToken string) {
	if botToken == "" {
		return
	}
	url := fmt.Sprintf("https://api.telegram.org/bot%s/setMyCommands", botToken)
	commands := []map[string]string{
		{"command": "start", "description": "🚀 Start / Main Menu"},
		{"command": "courses", "description": "📚 Browse All Courses"},
		{"command": "userbot", "description": "⭐ Userbot Login & Settings Menu"},
		{"command": "cmd", "description": "💻 Admin Commands Menu"},
		{"command": "predownload", "description": "⚡ Fast Pre-download Course"},
		{"command": "userbotdl", "description": "📥 Download via Userbot"},
		{"command": "copychannel", "description": "🔄 Copy Channel Lectures"},
		{"command": "status", "description": "📊 Bot & Worker Status"},
		{"command": "help", "description": "❓ Help & Instructions"},
	}
	payload, err := json.Marshal(map[string]interface{}{
		"commands": commands,
		"scope":    map[string]string{"type": "default"},
	})
	if err != nil {
		return
	}

	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
		if err != nil {
			cancel()
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			cancel()
			time.Sleep(2 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		log.Printf("[BotMenu] Telegram setMyCommands response: %s", string(body))
		if strings.Contains(string(body), `"ok":true`) {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// ── setChatMenuButton — Telegram UI मध्ये Mic/Attachment Icon जवळ '≡ Menu' बटण Force दाखवण्यासाठी ──
	menuUrl := fmt.Sprintf("https://api.telegram.org/bot%s/setChatMenuButton", botToken)
	menuPayload, _ := json.Marshal(map[string]interface{}{
		"menu_button": map[string]string{
			"type": "commands",
		},
	})
	mCtx, mCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer mCancel()
	mReq, mErr := http.NewRequestWithContext(mCtx, "POST", menuUrl, bytes.NewReader(menuPayload))
	if mErr == nil {
		mReq.Header.Set("Content-Type", "application/json")
		if mResp, doErr := http.DefaultClient.Do(mReq); doErr == nil {
			mBody, _ := io.ReadAll(mResp.Body)
			mResp.Body.Close()
			log.Printf("[BotMenu] Telegram setChatMenuButton response: %s", string(mBody))
		}
	}
}
