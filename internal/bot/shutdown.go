package bot

import "EverythingSuckz/fsb/internal/commands"

// ShutdownAll — redeploy (SIGTERM) var sagle Telegram clients thambvto,
// jene karun jaruna instance cha auth key navin instance shi bhidat nahi.
func ShutdownAll() {
	UserBot.Stop()
	commands.StopAllUserbotClients()
	if Workers != nil {
		for _, w := range Workers.Bots {
			if w != nil && w.Client != nil {
				w.Client.Stop()
			}
		}
	}
	if Bot != nil {
		Bot.Stop()
	}
}
