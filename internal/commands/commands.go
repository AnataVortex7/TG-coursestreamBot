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

// MainUserbotClient — userbot चा client, bot package import न करता.
// run.go मध्ये bot.StartUserBotFromDB नंतर set होतो.
var MainUserbotClient *gotgproto.Client

// SyncChannelAdmins — course channel madhe sagle bots (main bot + worker bots)
// automatic admin karayla call hoto (cmdAddCourse madhun). bot package madhe
// run.go ne set kela jato (import cycle टाळायला): commands ← bot already
// import karte, mhanun commands ne bot direct import karu shakat nahi.
// Nil असेल (userbot kadhi load nahi zala) tar caller ne skip karaycha.
var SyncChannelAdmins func(channelID int64) error

// ─── Userbot login/logout/status hooks (menu.go वापरतो) ────────────────────
// bot package direct import करू शकत नाही (वरचंच import-cycle कारण), म्हणून
// run.go हे सगळे bot.UserBot.* वर wire करतो.
var (
	UserbotStatus           func() (running bool, firstName, lastName, username, phone string, userID int64)
	UserbotSendCode         func(phone string) (phoneHash string, err error)
	UserbotVerifyCode       func(phone, phoneHash, code, password string) (sessionStr, firstName, lastName, username, phoneOut string, userID int64, err error)
	UserbotStop             func()
	UserbotStartFromSession func(sessionStr string)
)

// GetLiveUserbotClient — MainUserbotClient (वर) फक्त startup वेळचा snapshot आहे;
// नंतर login/logout (button menu) झाल्यास तो stale राहतो. हा bridge नेहमी
// bot.UserBot चा सध्याचा live client देतो (नसेल तर nil).
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

func Load(log *zap.Logger, dispatcher dispatcher.Dispatcher) {
	log = log.Named("commands")
	defer log.Info("Initialized all command handlers")
	Type := reflect.TypeOf(&command{log})
	Value := reflect.ValueOf(&command{log})
	for i := 0; i < Type.NumMethod(); i++ {
		Type.Method(i).Func.Call([]reflect.Value{Value, reflect.ValueOf(dispatcher)})
	}
}
