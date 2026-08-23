package main

import (
	"io/ioutil"
	"strings"
)

func main() {
	b, _ := ioutil.ReadFile("internal/commands/admin_stream.go")
	s := string(b)
	
	startIdx := strings.Index(s, "var logMedia tg.InputMediaClass = inputMedia")
	if startIdx == -1 {
		panic("start not found")
	}
	endStr := "if dm := inputMediaDocFromUpdates(adminResult); dm != nil {\n\t\t\tlogMedia = dm\n\t\t}"
	endIdx := strings.Index(s[startIdx:], endStr)
	if endIdx == -1 {
		panic("end not found")
	}
	endIdx = startIdx + endIdx + len(endStr)
	
	replacement := `var logMedia tg.InputMediaClass = inputMedia

		// [FIX] Send to LOG CHANNEL first (just like worker does when protect_content=True)
		if markCopied(chatID, msgID) {
			logCh, err := utils.GetLogChannelPeer(context.Background(), botClient.API(), botClient.PeerStorage)
			if err == nil {
				toPeer := &tg.InputPeerChannel{
					ChannelID:  logCh.ChannelID,
					AccessHash: logCh.AccessHash,
				}
				logResult, sendErr := botClient.API().MessagesSendMedia(context.Background(), &tg.MessagesSendMediaRequest{
					Peer:     toPeer,
					Media:    inputMedia,
					Message:  tgMsg.GetMessage(), // standard log channel message
					RandomID: rand.Int63(),
					Silent:   true,
				})
				if sendErr == nil {
					if dm := inputMediaDocFromUpdates(logResult); dm != nil {
						logMedia = dm
					}
				}
			}

			// Distribute to Log Group using the extracted Document ID
			if userbotLogGroupPeer != nil && userbotTopicID != 0 {
				ulReq := &tg.MessagesSendMediaRequest{
					Peer:     userbotLogGroupPeer,
					Media:    logMedia,
					Message:  adminCaption,
					RandomID: rand.Int63(),
					ReplyTo:  &tg.InputReplyToMessage{ReplyToMsgID: userbotTopicID},
				}
				go botClient.API().MessagesSendMedia(context.Background(), ulReq)
			}
		}

		sendCtx, sendCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer sendCancel()
		// Now distribute to the User PM using the extracted Document ID
		adminResult, err := botClient.API().MessagesSendMedia(sendCtx, &tg.MessagesSendMediaRequest{
			Peer:       botPeer,
			Media:      logMedia,
			Message:    adminCaption,
			Noforwards: true,
			RandomID:   rand.Int63(),
		})
		if err != nil {
			return fmt.Errorf("send to admin failed: %w", err)
		}

		if dm := inputMediaDocFromUpdates(adminResult); dm != nil {
			logMedia = dm
		}`

	newS := s[:startIdx] + replacement + s[endIdx:]
	
	ioutil.WriteFile("internal/commands/admin_stream.go", []byte(newS), 0644)
}
