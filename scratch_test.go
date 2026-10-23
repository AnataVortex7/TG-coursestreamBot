//go:build ignore

package main

import (
	"context"
	"fmt"
	"github.com/gotd/td/tg"
)

func main() {
	req := &tg.ChannelsCreateForumTopicRequest{
		Channel: &tg.InputChannel{ChannelID: 1, AccessHash: 1},
		Title:   "test",
	}
	fmt.Printf("%T\n", req)
}
