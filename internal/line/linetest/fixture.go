// Package linetest builds synthetic LINE iOS stores for tests. The tables
// carry the columns of the app's Core Data schema that the importer reads.
package linetest

import (
	"crypto/sha1" //nolint:gosec // iOS backups name files by the SHA-1 of domain and path
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	"howett.net/plist"
)

// User is one ZUSER row.
type User struct {
	MID             string
	Name            string
	CustomName      string
	AddressBookName string
}

// Message is one ZMESSAGE row. Sender "" means the owner sent it. NoID leaves
// ZID NULL, as LINE does for a send that never completed.
type Message struct {
	ID          string
	NoID        bool
	ContentType int
	Millis      int64
	Sender      string
	Text        string
	Metadata    map[string]string
}

// Chat is one ZCHAT row. Type follows ZCHAT.ZTYPE: 0 direct, 1 room, 2 group,
// 100 the owner's Keep Memo chat. Members fill Z_1MEMBERS for rooms and the
// UnifiedGroup store for groups.
type Chat struct {
	MID      string
	Type     int
	Name     string
	Members  []string
	Messages []Message
}

// Fixture describes a store. Groups are written to UnifiedGroup.sqlite unless
// NoGroupsFile is set.
type Fixture struct {
	Users        []User
	Chats        []Chat
	NoGroupsFile bool
}

const storeSchema = `
CREATE TABLE ZCHAT ( Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER, ZALERT INTEGER, ZE2EECONTENTTYPES INTEGER, ZENABLE INTEGER, ZLASTRECEIVEDMESSAGEID INTEGER, ZLIVE INTEGER, ZREADUPTOMESSAGEID INTEGER, ZREADUPTOMESSAGEIDSYNCED INTEGER, ZSESSIONID INTEGER, ZSORTORDER INTEGER, ZTYPE INTEGER, ZUNREAD INTEGER, ZMETADATA INTEGER, ZEXPIREINTERVAL FLOAT, ZLASTUPDATED TIMESTAMP, ZINPUTTEXT VARCHAR, ZINVITERMID VARCHAR, ZLASTMESSAGE VARCHAR, ZMID VARCHAR, ZSKIN VARCHAR );
CREATE TABLE Z_1MEMBERS ( Z_1CHATS INTEGER, Z_12MEMBERS INTEGER, PRIMARY KEY (Z_1CHATS, Z_12MEMBERS) );
CREATE TABLE ZMESSAGE ( Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER, ZCONTENTTYPE INTEGER, ZREADCOUNT INTEGER, ZSENDSTATUS INTEGER, ZTIMESTAMP INTEGER, ZCHAT INTEGER, ZSENDER INTEGER, ZLATITUDE FLOAT, ZLONGITUDE FLOAT, ZID VARCHAR, ZMESSAGETYPE VARCHAR, ZTEXT VARCHAR, ZCONTENTMETADATA BLOB, ZTHUMBNAIL BLOB );
CREATE TABLE ZUSER ( Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER, ZALERT INTEGER, ZBLOCKING INTEGER, ZCAPABILITIES INTEGER, ZCONTACTTYPE INTEGER, ZE2EECONTENTTYPES INTEGER, ZFAVORITEORDER INTEGER, ZFRIENDREQUESTSTATUS INTEGER, ZISFRIEND INTEGER, ZISHIDDEN INTEGER, ZISINADDRESSBOOK INTEGER, ZISRECOMMENDED INTEGER, ZISREMOVED INTEGER, ZISUNREGISTERED INTEGER, ZISVIEWED INTEGER, ZMYHOMECAPABLE INTEGER, ZUSERTYPE INTEGER, ZVIDEOCALLCAPABLE INTEGER, ZVOICECALLCAPABLE INTEGER, ZCREATEDTIME TIMESTAMP, ZSTATUSUPDATEDAT TIMESTAMP, ZADDRESSBOOKNAME VARCHAR, ZCOUNTRY VARCHAR, ZCUSTOMNAME VARCHAR, ZMID VARCHAR, ZNAME VARCHAR, ZPICTURESTATUS VARCHAR, ZPICTUREURL VARCHAR, ZPROFILEIMAGE VARCHAR, ZRECOMMENDPARAMS VARCHAR, ZSORTABLENAME VARCHAR, ZSTATUSMESSAGE VARCHAR, ZE2EEPUBLICKEYCHAIN BLOB, ZPUBLICKEYCHAIN BLOB );
CREATE INDEX Z_Message_chat_timestamp ON ZMESSAGE (ZCHAT COLLATE BINARY ASC, ZTIMESTAMP COLLATE BINARY ASC);
`

const groupsSchema = `
CREATE TABLE ZUNIFIEDGROUP ( Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER, ZALERT INTEGER, ZENABLE INTEGER, ZFAVORITEORDER INTEGER, ZISACCEPTED INTEGER, ZISADDFRIENDDISABLED INTEGER, ZISPREVENTLINKINVITATION INTEGER, ZISTICKETDISABLED INTEGER, ZISVIEWED INTEGER, ZTYPE INTEGER, ZCREATORCHATUSER INTEGER, ZCREATEDTIME TIMESTAMP, ZID VARCHAR, ZLINKINVITATION VARCHAR, ZNAME VARCHAR, ZPICTURESTATUS VARCHAR , ZMEMBERSCOUNT INTEGER, ZISAUTONAME INTEGER, ZISPENALIZED INTEGER);
CREATE TABLE Z_1MEMBERCHATUSERS ( Z_1GROUPS INTEGER, Z_2MEMBERCHATUSERS INTEGER, PRIMARY KEY (Z_1GROUPS, Z_2MEMBERCHATUSERS) );
CREATE TABLE ZUNIFIEDGROUPUSER ( Z_PK INTEGER PRIMARY KEY, Z_ENT INTEGER, Z_OPT INTEGER, ZMID VARCHAR );
`

// Write creates Line.sqlite (and UnifiedGroup.sqlite) in dir and returns dir.
func Write(t testing.TB, dir string, f Fixture) string {
	t.Helper()
	writeStore(t, filepath.Join(dir, "Line.sqlite"), f)
	if !f.NoGroupsFile {
		writeGroups(t, filepath.Join(dir, "UnifiedGroup.sqlite"), f)
	}
	return dir
}

// WriteBackup lays the fixture out as an unencrypted iPhone backup under
// root: a Manifest.db plus the two stores under their hashed names.
func WriteBackup(t testing.TB, root string, f Fixture) string {
	t.Helper()
	const domain = "AppDomainGroup-group.com.linecorp.line"
	const dir = "Library/Application Support/PrivateStore/P_u0123456789abcdef/Messages/"
	db, err := sql.Open("sqlite3", filepath.Join(root, "Manifest.db"))
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE Files (fileID TEXT PRIMARY KEY, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)`)
	require.NoError(t, err)
	place := func(name string, write func(path string)) {
		rel := dir + name
		sum := sha1.Sum([]byte(domain + "-" + rel)) //nolint:gosec // the backup format's file naming
		id := hex.EncodeToString(sum[:])
		require.NoError(t, os.MkdirAll(filepath.Join(root, id[:2]), 0o755))
		write(filepath.Join(root, id[:2], id))
		_, err := db.Exec(`INSERT INTO Files (fileID, domain, relativePath, flags) VALUES (?, ?, ?, 1)`, id, domain, rel)
		require.NoError(t, err)
	}
	place("Line.sqlite", func(path string) { writeStore(t, path, f) })
	if !f.NoGroupsFile {
		place("UnifiedGroup.sqlite", func(path string) { writeGroups(t, path, f) })
	}
	require.NoError(t, db.Close())
	return root
}

func writeStore(t testing.TB, path string, f Fixture) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.Exec(storeSchema)
	require.NoError(t, err)
	userRow := map[string]int64{}
	for i, u := range f.Users {
		row := int64(i + 1)
		userRow[u.MID] = row
		_, err := db.Exec(`INSERT INTO ZUSER (Z_PK, Z_ENT, ZMID, ZNAME, ZCUSTOMNAME, ZADDRESSBOOKNAME) VALUES (?, 12, ?, ?, ?, ?)`,
			row, u.MID, nullable(u.Name), nullable(u.CustomName), nullable(u.AddressBookName))
		require.NoError(t, err)
	}
	messageRow := int64(0)
	for i, c := range f.Chats {
		chatRow := int64(i + 1)
		_, err := db.Exec(`INSERT INTO ZCHAT (Z_PK, Z_ENT, ZTYPE, ZMID, ZENABLE) VALUES (?, 1, ?, ?, 1)`, chatRow, c.Type, c.MID)
		require.NoError(t, err)
		if c.Type != 2 {
			for _, mid := range c.Members {
				row, ok := userRow[mid]
				require.True(t, ok, "member %s is not a fixture user", mid)
				_, err := db.Exec(`INSERT INTO Z_1MEMBERS (Z_1CHATS, Z_12MEMBERS) VALUES (?, ?)`, chatRow, row)
				require.NoError(t, err)
			}
		}
		for _, m := range c.Messages {
			messageRow++
			var sender any
			if m.Sender != "" {
				row, ok := userRow[m.Sender]
				require.True(t, ok, "sender %s is not a fixture user", m.Sender)
				sender = row
			}
			var id any
			status := 1
			if !m.NoID {
				id = m.ID
			} else {
				status = 0
			}
			var blob any
			if len(m.Metadata) > 0 {
				blob = KeyedArchive(t, m.Metadata)
			}
			_, err := db.Exec(`INSERT INTO ZMESSAGE (Z_PK, Z_ENT, ZCONTENTTYPE, ZSENDSTATUS, ZTIMESTAMP, ZCHAT, ZSENDER, ZID, ZTEXT, ZCONTENTMETADATA)
				VALUES (?, 5, ?, ?, ?, ?, ?, ?, ?, ?)`, messageRow, m.ContentType, status, m.Millis, chatRow, sender, id, nullable(m.Text), blob)
			require.NoError(t, err)
		}
	}
}

func writeGroups(t testing.TB, path string, f Fixture) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.Exec(groupsSchema)
	require.NoError(t, err)
	userRow := map[string]int64{}
	groupRow := int64(0)
	for _, c := range f.Chats {
		if c.Type != 2 {
			continue
		}
		groupRow++
		_, err := db.Exec(`INSERT INTO ZUNIFIEDGROUP (Z_PK, Z_ENT, ZID, ZNAME) VALUES (?, 1, ?, ?)`, groupRow, c.MID, nullable(c.Name))
		require.NoError(t, err)
		for _, mid := range c.Members {
			row, ok := userRow[mid]
			if !ok {
				row = int64(len(userRow) + 1)
				userRow[mid] = row
				_, err := db.Exec(`INSERT INTO ZUNIFIEDGROUPUSER (Z_PK, Z_ENT, ZMID) VALUES (?, 2, ?)`, row, mid)
				require.NoError(t, err)
			}
			_, err := db.Exec(`INSERT INTO Z_1MEMBERCHATUSERS (Z_1GROUPS, Z_2MEMBERCHATUSERS) VALUES (?, ?)`, groupRow, row)
			require.NoError(t, err)
		}
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// KeyedArchive encodes a string dictionary the way NSKeyedArchiver does:
// a binary plist whose $objects table holds the keys and values, with the
// root dictionary referring to them by UID.
func KeyedArchive(t testing.TB, entries map[string]string) []byte {
	t.Helper()
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	objects := []any{"$null"}
	var keyUIDs, valueUIDs []plist.UID
	for _, k := range keys {
		objects = append(objects, k)
		keyUIDs = append(keyUIDs, plist.UID(len(objects)-1)) //nolint:gosec // small test slice
		objects = append(objects, entries[k])
		valueUIDs = append(valueUIDs, plist.UID(len(objects)-1)) //nolint:gosec // small test slice
	}
	classUID := plist.UID(len(objects) + 1) //nolint:gosec // small test slice
	objects = append(objects, map[string]any{"NS.keys": keyUIDs, "NS.objects": valueUIDs, "$class": classUID})
	rootUID := plist.UID(len(objects) - 1) //nolint:gosec // small test slice
	objects = append(objects, map[string]any{"$classname": "NSMutableDictionary", "$classes": []string{"NSMutableDictionary", "NSDictionary", "NSObject"}})
	archive := map[string]any{
		"$version":  100000,
		"$archiver": "NSKeyedArchiver",
		"$top":      map[string]any{"root": rootUID},
		"$objects":  objects,
	}
	blob, err := plist.Marshal(archive, plist.BinaryFormat)
	require.NoError(t, err)
	return blob
}
