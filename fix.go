package main

import (
	"io/ioutil"
	"strings"
)

func main() {
	b, _ := ioutil.ReadFile("internal/commands/admin_stream.go")
	s := string(b)
	
	startIdx := strings.Index(s, "sendCtx, sendCancel := context.WithTimeout(context.Background(), 10*time.Minute)")
	if startIdx == -1 {
		panic("start not found")
	}
	endStr := "}()\n\t\t}"
	endIdx := strings.Index(s[startIdx:], endStr)
	if endIdx == -1 {
		panic("end not found")
	}
	endIdx = startIdx + endIdx + len(endStr)
	
	replacement := `var logMedia tg.InputMediaClass = inputMedia

		// [FIX] Send to Log Group FIRST, because sending to PM doesn't return UpdateNewMessage for Bots,
		// so we can't extract the Document ID if we send to PM first.
		if userbotLogGroupPeer != nil && userbotTopicID != 0 {
			if markCopied(chatID, msgID) {
				ulReq := &tg.MessagesSendMediaRequest{
					Peer:     userbotLogGroupPeer,
					Media:    inputMedia,
					Message:  adminCaption,
					RandomID: rand.Int63(),
					ReplyTo:  &tg.InputReplyToMessage{ReplyToMsgID: userbotTopicID},
				}
				logResult, err := botClient.API().MessagesSendMedia(context.Background(), ulReq)
				if err == nil {
					if dm := inputMediaDocFromUpdates(logResult); dm != nil {
						logMedia = dm
					}
					go copyToLogChannelUploaded(tgMsg, logMedia)
				} else {
					// Fallback to sending original media to main log channel if log group fails
					go copyToLogChannelUploaded(tgMsg, inputMedia)
				}
			}
		}

		sendCtx, sendCancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer sendCancel()
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
