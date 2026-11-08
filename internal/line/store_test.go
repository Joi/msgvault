package line

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/line/linetest"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestOpenResolvesFileDirectoryAndBackup(t *testing.T) {
	f := sampleFixture()
	dir := linetest.Write(t, t.TempDir(), f)
	backup := linetest.WriteBackup(t, t.TempDir(), f)
	for name, path := range map[string]string{"file": filepath.Join(dir, "Line.sqlite"), "directory": dir, "backup": backup} {
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, err := Open(t.Context(), path)
			require.NoError(err)
			t.Cleanup(func() { require.NoError(st.Close()) })
			assert.NotEmpty(st.GroupsPath, "UnifiedGroup.sqlite is found beside the store")
			owner, err := st.OwnerMID(t.Context())
			require.NoError(err)
			assert.Equal(ownerMID, owner)
			users, err := st.Users(t.Context())
			require.NoError(err)
			chats, err := st.Chats(t.Context(), users)
			require.NoError(err)
			require.Len(chats, 4)
			assert.Equal("Book club", chats[2].Title)
			assert.Equal([]string{ownerMID, aliceMID, bobMID}, chats[2].Members)
			assert.Equal([]string{aliceMID, bobMID}, chats[3].Members)
		})
	}
}

func TestImportFromBackupRoot(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	backup := linetest.WriteBackup(t, t.TempDir(), sampleFixture())
	summary, err := Import(t.Context(), st, ImportOptions{Path: backup})
	require.NoError(err)
	assert.EqualValues(9, summary.MessagesAdded)
}

func TestOpenRejectsOtherFiles(t *testing.T) {
	require := require.New(t)
	empty := t.TempDir()
	_, err := Open(t.Context(), empty)
	require.ErrorContains(err, "holds neither Line.sqlite nor Manifest.db")
	_, err = Open(t.Context(), filepath.Join(empty, "missing"))
	require.Error(err)
	other := testutil.WriteFile(t, empty, "notes.txt", []byte("not a database"))
	_, err = Open(t.Context(), other)
	require.ErrorContains(err, "not a database")
	archive := testutil.NewSQLiteTestStore(t)
	var path string
	require.NoError(archive.DB().QueryRowContext(t.Context(), "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path))
	_, err = Open(t.Context(), path)
	require.ErrorContains(err, "not a LINE message store")
}

func TestOpenReadsUncheckpointedWAL(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := sampleFixture()
	f.WALChats = []linetest.Chat{{MID: bobMID, Type: 0, Members: []string{bobMID}, Messages: []linetest.Message{
		{ID: "500", Millis: 1700000050000, Sender: bobMID, Text: "only in the write-ahead log"},
	}}}
	dir := linetest.Write(t, t.TempDir(), f)
	info, err := os.Stat(filepath.Join(dir, "Line.sqlite-wal"))
	require.NoError(err)
	require.Positive(info.Size(), "the fixture must leave rows in the log")
	for name, path := range map[string]string{"directory": dir, "backup": linetest.WriteBackup(t, t.TempDir(), f)} {
		t.Run(name, func(t *testing.T) {
			st := testutil.NewTestStore(t)
			summary, err := Import(t.Context(), st, ImportOptions{Path: path})
			require.NoError(err)
			assert.EqualValues(10, summary.MessagesAdded, "nine checkpointed rows plus the one in the log")
			assert.Equal(1, count(t, st, "SELECT count(*) FROM messages WHERE source_message_id = ?", bobMID+":500"))
		})
	}
	entries, err := filepath.Glob(filepath.Join(os.TempDir(), "msgvault-line-*"))
	require.NoError(err)
	assert.Empty(entries, "scratch copies are removed on Close")
	original, err := os.ReadFile(filepath.Join(dir, "Line.sqlite-wal"))
	require.NoError(err)
	assert.Len(original, int(info.Size()), "the source log is never modified")
}

func TestOpenRejectsBackupWithSeveralAccounts(t *testing.T) {
	second := linetest.Fixture{Chats: []linetest.Chat{{MID: "u0000000000000000000000000000000f", Type: 100, Messages: []linetest.Message{{ID: "1", Millis: 1, Text: "other account"}}}}}
	backup := linetest.WriteBackup(t, t.TempDir(), sampleFixture(), second)
	_, err := Open(t.Context(), backup)
	require.ErrorContains(t, err, "holds 2 LINE message stores")
}
