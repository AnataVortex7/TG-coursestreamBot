package commands

import (
	"context"
	"reflect"
    "github.com/celestix/gotgproto"  
	"github.com/celestix/gotgproto/dispatcher"
	"go.uber.org/zap"
)

type command struct {
	log *zap.Logger
}

var MainBotClient *gotgproto.Client  // set from run.go after bot.StartClient

// GetUserbotClient — userbot चा client मिळवण्यासाठी
var GetUserbotClient func() *gotgproto.Client

var SyncChannelAdmins func(channelID int64) error

var (
	UserbotStatus            func() (running bool, firstName, lastName, username, phone string, userID int64)
	UserbotSendCode          func(phone string) (phoneHash string, err error)
	UserbotVerifyCode        func(phone, phoneHash, code, password string) (sessionStr, firstName, lastName, username, phoneOut string, userID int64, err error)
	UserbotStop              func()
	UserbotStartFromSession  func(sessionStr string)
	SendTelegramDMWithMarkup func(userID int64, text string, markup interface{}) error
)

var GetLiveUserbotClient func() *gotgproto.Client

// GetDownloadBotForChannel — bot pool (MultiTokens) मधून round-robin एक worker
// निवडतो आणि त्याच worker च्या स्वतःच्या client/PeerStorage मधून त्या channel
// साठीचा access_hash resolve करून token सोबत देतो.
//
// महत्त्वाचं: Telegram चा access_hash प्रत्येक bot/account साठी वेगळा असतो — तो
// त्या विशिष्ट bot च्या session ने त्या channel शी आधी interact केलेलं असेल
// तरच वैध असतो. दुसऱ्या bot चा access_hash वापरला की Telegram "CHANNEL_INVALID"
// देतो. त्यामुळे access_hash आणि जो bot download करणार तो एकाच worker मधून
// यायला हवेत — म्हणून हे दोन्ही एकत्र एका function मधून देतो (वेगळं
// token-only + access_hash-only काढल्यास mismatch होण्याची शक्यता असते).
//
// bot package direct import करू शकत नाही (import cycle — bot ← commands
// आधीच import करतो), म्हणून run.go हे bot.GetNextWorker() + utils.GetChannelPeer
// वर wire करतो. Pool रिकामा/wire न झालेला असेल तर ("", 0, err) देतो — caller
// ने त्यावेळी मुख्य BOT_TOKEN (आणि त्याच client चा access_hash) वर fallback करायचा.
var GetDownloadBotForChannel func(ctx context.Context, channelID int64) (botToken string, accessHash int64, err error)

// GetNextUploadBotToken — bot pool (MultiTokens + main bot) मधून round-robin
// एक bot token देतो, फक्त upload साठी (access_hash लागत नाही — worker Bot API
// ने सरळ chat_id वर पाठवतो, MTProto channel access_hash सारखं काही लागत
// नाही). सध्या फक्त silent pre-download (/predownload) साठी वापरतो — जेणेकरून
// एकाच वेळी अनेक lectures upload होताना मुख्य BOT_TOKEN वरच FLOOD_WAIT येऊ
// नये, तो load सगळ्या bots मध्ये विभागला जावा. Normal (student-facing)
// delivery मध्ये हे वापरलं जात नाही — तिथे कायम मुख्य BOT_TOKEN नेच होतं.
//
// bot package direct import करू शकत नाही (import cycle — bot ← commands
// आधीच import करतो), म्हणून run.go हे bot.GetNextWorker() वर wire करतो. Pool
// रिकामा/wire न झालेला असेल तर ("", err) देतो — caller ने मुख्य BOT_TOKEN वर
// fallback करायचा.
var GetNextUploadBotToken func() (botToken string, err error)

func Load(log *zap.Logger, dispatcher dispatcher.Dispatcher) {
	log = log.Named("commands")
	defer log.Info("Initialized all command handlers")
	Type := reflect.TypeOf(&command{log})
	Value := reflect.ValueOf(&command{log})
	for i := 0; i < Type.NumMethod(); i++ {
		Type.Method(i).Func.Call([]reflect.Value{Value, reflect.ValueOf(dispatcher)})
	}
}
