package line

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line/linetest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const (
	ownerMID = "u0000000000000000000000000000000a"
	aliceMID = "u0000000000000000000000000000000b"
	bobMID   = "u0000000000000000000000000000000c"
	groupMID = "c0000000000000000000000000000000d"
	roomMID  = "r0000000000000000000000000000000e"
)

// sampleFixture covers every chat kind and every content kind the importer
// classifies.
func sampleFixture() linetest.Fixture {
	return linetest.Fixture{
		Users: []linetest.User{
			{MID: aliceMID, Name: "Alice Profile", CustomName: "Alice"},
			{MID: bobMID, Name: "Bob"},
		},
		Chats: []linetest.Chat{
			{MID: ownerMID, Type: 100, Messages: []linetest.Message{
				{ID: "900", ContentType: 0, Millis: 1700000000000, Text: "note to self"},
			}},
			{MID: aliceMID, Type: 0, Members: []string{aliceMID}, Messages: []linetest.Message{
				{ID: "100", ContentType: 0, Millis: 1700000001000, Sender: aliceMID, Text: "hello from alice"},
				{ID: "101", ContentType: 0, Millis: 1700000002000, Text: "hello back", Metadata: map[string]string{"replyOriginalMessageId": "100"}},
				{ID: "102", ContentType: 1, Millis: 1700000003000, Sender: aliceMID, Text: "[Photo]"},
				{ID: "103", ContentType: 7, Millis: 1700000004000, Text: "￼", Metadata: map[string]string{"STKID": "1", "STKPKGID": "2"}},
				{ID: "104", ContentType: 6, Millis: 1700000005000, Metadata: map[string]string{"TYPE": "A", "RESULT": "NORMAL", "DURATION": "12"}},
				{ID: "105", ContentType: 0, Millis: 1700000006000, Text: "Message unsent", Metadata: map[string]string{"systemMessageTypeName": "ChatMessageUnsent"}},
				{ID: "106", ContentType: 18, Millis: 1700000007000, Text: "Alice left", Metadata: map[string]string{"systemMessageTypeName": "ChatEvent"}},
				{ID: "107", ContentType: 101, Millis: 1700000008000, Metadata: map[string]string{"UNSENT": "1"}},
				{NoID: true, ContentType: 0, Millis: 1700000009000, Text: "never left the phone"},
				{ID: "108", ContentType: 17, Millis: 1700000010000, Sender: aliceMID, Text: "￼", Metadata: map[string]string{"ALT_TEXT": "rich card text", "MARKUP_JSON": "{}"}},
				{ID: "109", ContentType: 107, Millis: 1700000011000, Sender: aliceMID, Text: "see https://example.com"},
				{ID: "110", ContentType: 13, Millis: 1700000012000, Sender: aliceMID, Text: "￼", Metadata: map[string]string{"displayName": "Someone", "mid": bobMID}},
				{ID: "111", ContentType: 0, Millis: 1700000013000, Sender: aliceMID, Text: "first copy"},
				{ID: "111", ContentType: 0, Millis: 1700000014000, Sender: aliceMID, Text: "second copy of the same id"},
			}},
			{MID: groupMID, Type: 2, Name: "Book club", Members: []string{ownerMID, aliceMID, bobMID}, Messages: []linetest.Message{
				{ID: "200", ContentType: 0, Millis: 1700000020000, Sender: bobMID, Text: "group text from bob"},
				{ID: "201", ContentType: 0, Millis: 1700000021000, Text: "group text from me"},
			}},
			{MID: roomMID, Type: 1, Members: []string{aliceMID, bobMID}, Messages: []linetest.Message{
				{ID: "300", ContentType: 0, Millis: 1700000030000, Sender: aliceMID, Text: "room text"},
			}},
		},
	}
}

func count(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, st.DB().QueryRowContext(t.Context(), st.Rebind(query), args...).Scan(&n))
	return n
}

func TestImportSampleStore(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	dir := linetest.Write(t, t.TempDir(), sampleFixture())
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	assert.Equal(ownerMID, summary.Owner)
	assert.True(summary.OwnerFromStore)
	assert.Equal(4, summary.ChatsProcessed)
	assert.Zero(summary.ChatsSkipped)
	assert.EqualValues(9, summary.MessagesAdded, "note, alice hello, reply, rich, link text, first copy, bob group, me group, room")
	assert.EqualValues(1, summary.MessagesSkipped, "the duplicate id")
	assert.EqualValues(1, summary.MediaSkipped)
	assert.EqualValues(1, summary.StickersSkipped)
	assert.EqualValues(1, summary.CallsSkipped)
	assert.EqualValues(2, summary.SystemSkipped, "the unsent notice and the chat event")
	assert.EqualValues(2, summary.UnsentSkipped, "the row without an id and the unsent row")
	assert.EqualValues(1, summary.UnsupportedSkipped, "the contact card")
	assert.Equal(3, summary.ParticipantsResolved)

	var body string
	assert.Equal(9, count(t, st, "SELECT count(*) FROM messages WHERE message_type = 'line'"))
	assert.Equal(3, count(t, st, "SELECT count(*) FROM messages WHERE is_from_me = TRUE"))
	assert.Equal(1, count(t, st, "SELECT count(*) FROM messages WHERE source_message_id = ?", aliceMID+":111"))
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT b.body_text FROM messages m JOIN message_bodies b ON b.message_id = m.id WHERE m.source_message_id = ?`), aliceMID+":111").Scan(&body))
	assert.Equal("first copy", body, "the first row with an id wins")

	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT b.body_text FROM messages m JOIN message_bodies b ON b.message_id = m.id WHERE m.source_message_id = ?`), aliceMID+":108").Scan(&body))
	assert.Equal("rich card text", body, "a rich message archives its alt text, not the placeholder")

	var parent string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT p.source_message_id FROM messages m JOIN messages p ON p.id = m.reply_to_message_id WHERE m.source_message_id = ?"), aliceMID+":101").Scan(&parent))
	assert.Equal(aliceMID+":100", parent)

	var kind, title string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT conversation_type, COALESCE(title, '') FROM conversations WHERE source_conversation_id = ?"), groupMID).Scan(&kind, &title))
	assert.Equal("group_chat", kind)
	assert.Equal("Book club", title)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT conversation_type, COALESCE(title, '') FROM conversations WHERE source_conversation_id = ?"), aliceMID).Scan(&kind, &title))
	assert.Equal("direct_chat", kind)
	assert.Empty(title)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT conversation_type FROM conversations WHERE source_conversation_id = ?"), roomMID).Scan(&kind))
	assert.Equal("group_chat", kind)

	members := func(conv string) int {
		return count(t, st, `SELECT count(*) FROM conversation_participants cp JOIN conversations c ON c.id = cp.conversation_id WHERE c.source_conversation_id = ?`, conv)
	}
	assert.Equal(3, members(groupMID), "owner, alice and bob from the group roster")
	assert.Equal(3, members(roomMID), "owner plus the two room members")
	assert.Equal(2, members(aliceMID))
	assert.Equal(1, members(ownerMID))

	var name string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT p.display_name FROM participants p JOIN participant_identifiers pi ON pi.participant_id = p.id WHERE pi.identifier_type = 'line' AND pi.identifier_value = ?`), aliceMID).Scan(&name))
	assert.Equal("Alice", name, "the owner's nickname for a contact wins over the profile name")

	var sender string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT pi.identifier_value FROM participant_identifiers pi JOIN messages m ON m.sender_id = pi.participant_id WHERE m.source_message_id = ?`), groupMID+":200").Scan(&sender))
	assert.Equal(bobMID, sender)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT pi.identifier_value FROM participant_identifiers pi JOIN messages m ON m.sender_id = pi.participant_id WHERE m.source_message_id = ?`), groupMID+":201").Scan(&sender))
	assert.Equal(ownerMID, sender)
}

func TestImportIsIdempotent(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	dir := linetest.Write(t, t.TempDir(), sampleFixture())
	first, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	second, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	assert.Zero(second.MessagesAdded)
	assert.Equal(first.MessagesAdded+1, second.MessagesSkipped)
	assert.False(second.WasResumed, "a completed run leaves no chat to resume")
	assert.Equal(int(first.MessagesAdded), count(t, st, "SELECT count(*) FROM messages"))
	assert.Equal(1, count(t, st, "SELECT count(*) FROM sources"))
}

func TestImportOwnerFromFlagOrStore(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	f := sampleFixture()
	f.Chats = f.Chats[1:] // no Keep Memo chat
	dir := linetest.Write(t, t.TempDir(), f)
	_, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.ErrorIs(err, ErrNoOwner)
	assert.Zero(count(t, st, "SELECT count(*) FROM sources"))
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir, Me: " myline.id "})
	require.NoError(err)
	assert.Equal("myline.id", summary.Owner)
	assert.False(summary.OwnerFromStore)
	var identifier string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT identifier FROM sources WHERE source_type = 'line'").Scan(&identifier))
	assert.Equal("myline.id", identifier)
	assert.Equal(1, count(t, st, `SELECT count(*) FROM participant_identifiers WHERE identifier_type = 'line' AND identifier_value = ?`, "myline.id"))
}

func TestImportLimitPausesAndResumes(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	dir := linetest.Write(t, t.TempDir(), sampleFixture())
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir, Limit: 3})
	require.NoError(err)
	assert.True(summary.LimitReached)
	assert.EqualValues(3, summary.MessagesAdded)
	var status string
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1").Scan(&status))
	assert.Equal("paused", status)

	summary, err = Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	assert.True(summary.WasResumed, "the checkpoint names the completed chats")
	assert.False(summary.LimitReached)
	assert.EqualValues(6, summary.MessagesAdded)
	assert.Equal(9, count(t, st, "SELECT count(*) FROM messages"))
	require.NoError(st.DB().QueryRowContext(t.Context(), "SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1").Scan(&status))
	assert.Equal("completed", status)
}

func TestImportNoResumeRescansEveryChat(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	dir := linetest.Write(t, t.TempDir(), sampleFixture())
	_, err := Import(t.Context(), st, ImportOptions{Path: dir, Limit: 1})
	require.NoError(err)
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir, NoResume: true})
	require.NoError(err)
	assert.False(summary.WasResumed)
	assert.EqualValues(8, summary.MessagesAdded)
}

func TestImportWithoutGroupsFile(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	f := sampleFixture()
	f.NoGroupsFile = true
	dir := linetest.Write(t, t.TempDir(), f)
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	assert.EqualValues(9, summary.MessagesAdded)
	var title string
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind("SELECT COALESCE(title, '') FROM conversations WHERE source_conversation_id = ?"), groupMID).Scan(&title))
	assert.Empty(title, "group names live in UnifiedGroup.sqlite")
	assert.Equal(2, count(t, st, `SELECT count(*) FROM conversation_participants cp JOIN conversations c ON c.id = cp.conversation_id WHERE c.source_conversation_id = ?`, groupMID), "owner plus the one sender seen")
}

func TestImportSkipsUnknownChatType(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	f := sampleFixture()
	f.Chats = append(f.Chats, linetest.Chat{MID: "s0000000000000000000000000000000f", Type: 3, Messages: []linetest.Message{{ID: "400", Millis: 1700000040000, Text: "square"}}})
	dir := linetest.Write(t, t.TempDir(), f)
	summary, err := Import(t.Context(), st, ImportOptions{Path: dir})
	require.NoError(err)
	assert.Equal(1, summary.ChatsSkipped)
	assert.EqualValues(9, summary.MessagesAdded)
}
