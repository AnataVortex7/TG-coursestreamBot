// internal/commands/watermark_deliver.go
//
// Offline-download (student) videos साठी — direct copy-forward ऐवजी, video
// download करून त्यावर user चा mobile number FFmpeg ने burn-in watermark
// म्हणून लावतो, मग तोच watermarked file user ला पाठवतो. यामुळे content
// screen-record / re-share झाला तरी user ची identity video मध्येच राहते.
//
// हे संपूर्ण काम (download + FFmpeg burn + upload) आता **फक्त** watermark
// worker/Colab वरच होतं — main server चा यात काहीही सहभाग नाही. Main server
// फक्त channel_id + message_id + एक download-bot token job मध्ये पाठवतो;
// worker स्वतःच्या Pyrogram session ने थेट त्या channel मधून file fetch
// करतो (कुठलाही byte main server मधून जात नाही — pipe/bandwidth load शून्य).
// Download-bot token bot-pool (MultiTokens) मधून round-robin निवडला जातो,
// जेणेकरून अनेक watermark worker/Colab instances एकाच वेळी active असतील तर
// प्रत्येकाचा download वेगळ्या bot वरून होतो. Upload/delivery कायम मुख्य
// BOT_TOKEN नेच होते.
//
// (टीप: /userbotdl admin flow अजूनही जुनाच file_url/pipe मार्ग वापरतो —
// तो इथे बदललेला नाही, फक्त हा student offline-download मार्ग बदलला आहे.)
//
// Watermark worker/Colab कुणीच online नसेल (किंवा job मध्येच fail/timeout
// झाला) तर — watermark टाळून, original video जसाच्या तसा (unwatermarked)
// student ला forward केला जातो.
package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"EverythingSuckz/fsb/internal/utils"
	"EverythingSuckz/fsb/internal/wmqueue"

	"github.com/celestix/gotgproto"
	"github.com/gotd/td/tg"
)

// ═══════════════════════════════════════════════════════════════════════════
// Colab routing — online असेल तर तिकडे, नाहीतर local fallback
// ═══════════════════════════════════════════════════════════════════════════

// deliverVideoToUser — मुख्य entry point. Watermark worker/Colab खरंच online
// असेल (KOYEB_URL set + wmqueue.IsColabOnline()) तरच तिकडे job पाठवतो.
// Main server वर आता FFmpeg कधीच चालत नाही — worker offline असेल किंवा job
// मध्येच fail/timeout झाला तर watermark टाळून, original video जसाच्या तसा
// (unwatermarked) student ला पाठवायला सांगतो (skipWatermark=true).
//
// silent — true असेल तर (admin silent pre-download साठी) worker फक्त
// LOG_CHANNEL ला video पाठवतो, student ला काहीच पाठवत नाही (studentChatID
// फक्त progress-tracking साठी वापरला जातो, प्रत्यक्ष delivery नाही).
//
// Returns:
//
//	uploadedFile     — (यापुढे कायम nil — local upload path आता अस्तित्वात नाही)
//	sentMessageID    — colab path असेल तर आधीच पाठवलेल्या message चा ID (48hr auto-delete साठी). silent मोड मध्ये नेहमी 0.
//	logChannelMsgID  — worker ने LOG_CHANNEL ला पाठवलेल्या master copy चा msg ID (0 = नाही मिळाला —
//	                    DeliveredCache fast-path साठी caller ने वापरायचा, logChatID दिलेला नसेल तर नेहमी 0)
//	viaColab         — true असेल तर worker ने आधीच थेट student ला पाठवलंय (silent मोड मध्ये "स्वतःच काम पूर्ण केलं" असा अर्थ), caller ने परत पाठवायची गरज नाही
//	skipWatermark    — true असेल तर caller ने plain (unwatermarked) copy-forward करायचा (silent मोड मध्ये लागू नाही — silent+worker-offline असेल तर पूर्ण delivery skip होते, काहीच पाठवलं जात नाही)
func deliverVideoToUser(
	botClient *gotgproto.Client,
	channelID int64,
	messageID int,
	fileName string,
	watermarkText string,
	studentChatID int64,
	progressMsgID int,
	caption string,
	logChatID int64, // 0 = LOG_CHANNEL backup नको (offline-download flow साठी, स्पॅम टाळायला)
	logAccessHash int64,
	silent bool, // [NEW] true = फक्त LOG_CHANNEL ला, student ला काहीच नाही
	editProgress func(string),
) (uploadedFile tg.InputFileClass, sentMessageID int, logChannelMsgID int, viaColab bool, skipWatermark bool, err error) {

	if silent && logChatID == 0 {
		// Silent pre-download साठी LOG_CHANNEL compulsory आहे — तोच एकमेव
		// destination आहे. नसेल तर काहीच करण्यासारखं नाही.
		return nil, 0, 0, false, false, errors.New("silent pre-download साठी LOG_CHANNEL configure असायलाच हवा")
	}

	if os.Getenv("KOYEB_URL") != "" && wmqueue.IsColabOnline() {
		// [NEW] bulk — फक्त /predownload च्या silent pre-download batch साठीच
		// true (silent==true तिथूनच येतो) — त्यामुळे फक्त त्याच jobs वर
		// per-worker cap (wmqueue.MaxJobsPerWorker) लागू होतो. Normal/live
		// lecture request (silent==false) कायम cap-free, fully parallel राहतो.
		msgID, logMsgID, colabErr := deliverViaColab(
			botClient, channelID, messageID, fileName, watermarkText,
			studentChatID, progressMsgID, caption, logChatID, logAccessHash, silent, editProgress,
		)
		if colabErr == nil {
			return nil, msgID, logMsgID, true, false, nil
		}
		if silent {
			// Silent मोडमध्ये worker fail झाला तर काहीच पाठवायचं नाही (student
			// ला unwatermarked पाठवणं इथे चुकीचं ठरेल — तो अजून काही मागतच
			// नाहीये). Caller ला स्पष्ट error कळवतो, तो फक्त log करून सोडून देईल.
			return nil, 0, 0, false, false, colabErr
		}
		utils.Logger.Sugar().Errorw("watermark worker delivery failed", "err", colabErr)
		return nil, 0, 0, false, false, fmt.Errorf("watermark worker delivery failed: %w", colabErr)
	}

	if silent {
		// Worker offline असेल तर silent pre-download साठी काहीच करण्यासारखं
		// नाही (student ला अनपेक्षित unwatermarked पाठवणं चुकीचं ठरेल).
		return nil, 0, 0, false, false, errors.New("watermark worker offline आहे — silent pre-download आत्ता शक्य नाही")
	}

	// कोणताही watermark worker/Colab online नाही — unwatermarked copy पाठवणे आता पूर्णपणे बंद केले आहे.
	return nil, 0, 0, false, false, errors.New("watermark server currently offline — please try again later")
}

// deliverViaColab — main server इथे कुठलाही file download/pipe करत नाही.
// फक्त channelID + messageID + एक round-robin download-bot token wmqueue वर
// job म्हणून submit करतो. Worker स्वतःच्या Pyrogram session ने (त्याच
// download-bot token ने) थेट त्या channel मधून file fetch करतो, FFmpeg ने
// watermark burn करतो, आणि मुख्य BOT_TOKEN ने (upload साठी, job मध्ये वेगळा
// पाठवलेला) थेट studentChatID ला पाठवतो (silent असेल तर फक्त LOG_CHANNEL ला).
// Main server ला processed file कधीच परत येत नाही — फक्त success/fail, sent
// message ID, आणि (असल्यास) log-channel master copy चा msg ID कळतो.
func deliverViaColab(
	botClient *gotgproto.Client,
	channelID int64,
	messageID int,
	fileName string,
	watermarkText string,
	studentChatID int64,
	progressMsgID int,
	caption string,
	logChatID int64,
	logAccessHash int64,
	silent bool,
	editProgress func(string),
) (sentMessageID int, logChannelMsgID int, err error) {

	if fileName == "" {
		fileName = "video.mp4"
	}

	// Download-bot token आणि त्याच bot चा access_hash — दोन्ही एकाच worker
	// मधून एकत्र (mismatch टाळायला, कारण access_hash प्रत्येक bot साठी वेगळा
	// असतो). Pool रिकामा/wire न झालेला/error असेल तर मुख्य botClient + मुख्य
	// BOT_TOKEN वरच fallback होतो (download आणि upload एकच bot वापरतील).
	downloadBotToken := os.Getenv("BOT_TOKEN")
	var channelAccessHash int64
	resolved := false
	if GetDownloadBotForChannel != nil {
		hashCtx, hashCancel := context.WithTimeout(context.Background(), 15*time.Second)
		tok, hash, dlErr := GetDownloadBotForChannel(hashCtx, channelID)
		hashCancel()
		if dlErr == nil && tok != "" {
			downloadBotToken = tok
			channelAccessHash = hash
			resolved = true
		} else {
			utils.Logger.Sugar().Warnw("download-bot resolve failed for watermark job — main bot वर fallback", "channelID", channelID, "err", dlErr)
		}
	}
	if !resolved {
		// Fallback: मुख्य botClient च्याच peer storage मधून access_hash काढतो
		// (तोच bot download साठी वापरणार असल्यामुळे mismatch होत नाही).
		hashCtx, hashCancel := context.WithTimeout(context.Background(), 15*time.Second)
		inputChannel, chErr := utils.GetChannelPeer(hashCtx, botClient.API(), botClient.PeerStorage, channelID)
		hashCancel()
		if chErr == nil && inputChannel != nil {
			channelAccessHash = inputChannel.AccessHash
		} else {
			utils.Logger.Sugar().Warnw("channel access_hash resolve failed for watermark job", "channelID", channelID, "err", chErr)
		}
	}

	uploadBotToken := os.Getenv("BOT_TOKEN")

	editProgress("📤 Uploading...\n☁️ पाठवत आहोत...")

	jobID, err := wmqueue.SubmitJob(
		"", // fileURL — direct-download mode मध्ये लागत नाही
		fileName,
		0, // fileSize — worker स्वतः source message वरून काढेल
		watermarkText,
		uploadBotToken,
		studentChatID, // adminChatID param — progress-edit इथेच होतो (student ची चॅट)
		progressMsgID,
		studentChatID, // sendToChatID — student ला थेट पाठवायचा (silent असेल तर worker वापरणारच नाही)
		0,             // sendToAccessHash — private chat, worker च्या entity cache वर (admin flow सारखाच पॅटर्न)
		0,             // sendToTopicID
		logChatID,     // logChatID — caller ठरवतो (offline-download: 0/स्पॅम टाळायला, bot Watch: LOG_CHANNEL)
		logAccessHash, // logAccessHash
		caption,
		messageID,
		!silent, // protectContent — silent pre-download मध्ये student ला पाठवतच नाही, त्यामुळे protect करायचं काही नाही
		silent,  // logOnly — [NEW] silent pre-download चा गाभा
		channelID,
		channelAccessHash,
		downloadBotToken,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("submit job failed: %w", err)
	}

	// [NEW] कुणी (उदा. /predownload) OnJobSubmitted hook set केला असेल तर
	// त्याला कळवतो — यामुळे तो त्याच्या स्वतःच्या msgID→jobID मॅपिंग ठेवून,
	// नंतर wmqueue.ProgressMessage(jobID) वापरून live stage/percent वाचू शकतो.
	NotifyJobSubmitted(messageID, jobID)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer waitCancel()

	success, jobErr, sentMsgID, logMsgID := wmqueue.WaitForDone(waitCtx, jobID)
	if !success {
		if jobErr == "" {
			jobErr = "colab job timeout/failed"
		}
		return 0, 0, errors.New(jobErr)
	}
	return sentMsgID, logMsgID, nil
}
