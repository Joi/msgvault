package line

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/line/linetest"
)

func TestDecodeMetadata(t *testing.T) {
	assert := assert.New(t)
	md := decodeMetadata(linetest.KeyedArchive(t, map[string]string{
		"replyOriginalMessageId": "123", "systemMessageTypeName": "ChatEvent", "ALT_TEXT": "card", "MENTION": "{}",
	}))
	assert.Equal(metadata{ReplyTo: "123", SystemType: "ChatEvent", AltText: "card"}, md)
	assert.Equal(metadata{}, decodeMetadata(nil))
	assert.Equal(metadata{}, decodeMetadata([]byte("bplist00 garbage")))
	assert.Equal(metadata{}, decodeMetadata(linetest.KeyedArchive(t, map[string]string{})))
}

func TestMessageKindAndBody(t *testing.T) {
	assert := assert.New(t)
	text := &Message{ID: "1", ContentType: 0, Text: " hi "}
	assert.Equal(kindText, text.kind())
	assert.Equal("hi", text.body())
	notice := &Message{ID: "2", ContentType: 0, Text: "Message unsent", Metadata: metadata{SystemType: "ChatMessageUnsent"}}
	assert.Equal(kindSystem, notice.kind())
	rich := &Message{ID: "3", ContentType: contentTypeRich, Text: "￼", Metadata: metadata{AltText: "alt"}}
	assert.Equal(kindText, rich.kind())
	assert.Equal("alt", rich.body())
	assert.Equal(kindOther, (&Message{ID: "4", ContentType: 9999, Text: "x"}).kind())
	assert.Equal("Nick", displayName("Nick", "Book", "Profile"))
	assert.Equal("Book", displayName(" ", "Book", "Profile"))
	assert.Equal("Profile", displayName("", "", "Profile"))
}
