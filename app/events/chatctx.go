package events

import (
	"github.com/umputun/tg-spam/app/storage"
)

// ChatContext is the per-chat runtime bundle that the listener routes on.
// One instance per ConfiguredChat in Settings.Telegram.Groups, populated by
// app/main.go startup wiring and passed into TelegramListener via the
// Chats field. Methods on listener/admin/reports accept *ChatContext to
// resolve the right SpamFilter/Locator/ban target without consulting
// listener struct fields.
//
// Group is the human-readable name or numeric chat ID used by getChatID
// at startup; PrimaryChatID and LinkedChannelID are resolved against
// Telegram at TelegramListener.Do startup.
type ChatContext struct {
	Group           string
	GID             string
	PrimaryChatID   int64
	LinkedChannelID int64

	Bot           Bot
	Locator       Locator
	ApprovedUsers *storage.ApprovedUsers
	DetectedSpam  *storage.DetectedSpam
	SpamLogger    SpamLogger
	Reports       Reports
	Warnings      Warnings
}
