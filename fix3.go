package main

import (
	"io/ioutil"
	"strings"
)

func main() {
	b, _ := ioutil.ReadFile("internal/commands/admin_stream.go")
	s := string(b)
	
	startIdx := strings.Index(s, "// Distribute to Log Group using the extracted Document ID")
	if startIdx == -1 {
		panic("start not found")
	}
	endStr := "go botClient.API().MessagesSendMedia(context.Background(), ulReq)\n\t\t\t}"
	endIdx := strings.Index(s[startIdx:], endStr)
	if endIdx == -1 {
		panic("end not found")
	}
	endIdx = startIdx + endIdx + len(endStr)
	
	replacement := `// Distribute to Log Group using the extracted Document ID
			if userbotLogGroupPeer != nil && userbotTopicID != 0 {
				_, isDoc := logMedia.(*tg.InputMediaDocument)
				if isDoc {
					ulReq := &tg.MessagesSendMediaRequest{
						Peer:     userbotLogGroupPeer,
						Media:    logMedia,
						Message:  adminCaption,
						RandomID: rand.Int63(),
						ReplyTo:  &tg.InputReplyToMessage{ReplyToMsgID: userbotTopicID},
					}
					go botClient.API().MessagesSendMedia(context.Background(), ulReq)
				}
			}`

	newS := s[:startIdx] + replacement + s[endIdx:]
	
	ioutil.WriteFile("internal/commands/admin_stream.go", []byte(newS), 0644)
}
