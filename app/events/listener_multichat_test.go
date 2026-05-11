package events

import (
	"testing"

	tbapi "github.com/OvyFlash/telegram-bot-api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTelegramListener_RoutesToMatchingChatContext verifies that isChatAllowed
// routes a fromChat ID to the matching ChatContext from byPrimary, and rejects
// unknown chat IDs.
func TestTelegramListener_RoutesToMatchingChatContext(t *testing.T) {
	c1 := &ChatContext{Group: "g1", GID: "chat1", PrimaryChatID: 101}
	c2 := &ChatContext{Group: "g2", GID: "chat2", PrimaryChatID: 202}
	l := &TelegramListener{
		Chats:     []*ChatContext{c1, c2},
		byPrimary: map[int64]*ChatContext{101: c1, 202: c2},
	}

	got, ok := l.isChatAllowed(101)
	require.True(t, ok)
	assert.Equal(t, c1, got)

	got, ok = l.isChatAllowed(202)
	require.True(t, ok)
	assert.Equal(t, c2, got)

	_, ok = l.isChatAllowed(999)
	assert.False(t, ok)
}

// TestTelegramListener_TestingIDs_SingleChat verifies TestingIDs fallback
// works in single-chat mode and is suppressed in multi-chat mode (where
// routing is ambiguous).
func TestTelegramListener_TestingIDs_SingleChat(t *testing.T) {
	c1 := &ChatContext{Group: "g1", GID: "chat1", PrimaryChatID: 101}
	l := &TelegramListener{
		Chats:      []*ChatContext{c1},
		byPrimary:  map[int64]*ChatContext{101: c1},
		TestingIDs: []int64{555},
	}
	got, ok := l.isChatAllowed(555)
	require.True(t, ok)
	assert.Equal(t, c1, got)
}

// TestTelegramListener_TestingIDs_MultiChat_Suppressed verifies that the
// TestingIDs fallback does not fire when multiple chats are configured.
func TestTelegramListener_TestingIDs_MultiChat_Suppressed(t *testing.T) {
	c1 := &ChatContext{Group: "g1", GID: "chat1", PrimaryChatID: 101}
	c2 := &ChatContext{Group: "g2", GID: "chat2", PrimaryChatID: 202}
	l := &TelegramListener{
		Chats:      []*ChatContext{c1, c2},
		byPrimary:  map[int64]*ChatContext{101: c1, 202: c2},
		TestingIDs: []int64{555},
	}
	_, ok := l.isChatAllowed(555)
	assert.False(t, ok)
}

// TestTelegramListener_IsLinkedChannel_MultiChat verifies a linked channel
// from any configured chat is recognized via the per-chat ChatContext.
func TestTelegramListener_IsLinkedChannel_MultiChat(t *testing.T) {
	c1 := &ChatContext{Group: "g1", GID: "chat1", PrimaryChatID: 101, LinkedChannelID: -1001}
	c2 := &ChatContext{Group: "g2", GID: "chat2", PrimaryChatID: 202, LinkedChannelID: -1002}
	l := &TelegramListener{
		Chats:     []*ChatContext{c1, c2},
		byPrimary: map[int64]*ChatContext{101: c1, 202: c2},
	}

	assert.True(t, l.isLinkedChannel(c1, &tbapi.Message{SenderChat: &tbapi.Chat{ID: -1001}}))
	assert.True(t, l.isLinkedChannel(c2, &tbapi.Message{SenderChat: &tbapi.Chat{ID: -1002}}))
	assert.False(t, l.isLinkedChannel(c1, &tbapi.Message{SenderChat: &tbapi.Chat{ID: -9999}}))
	assert.False(t, l.isLinkedChannel(c2, &tbapi.Message{SenderChat: &tbapi.Chat{ID: -1001}}),
		"c2 must not match c1's linked channel")
}
