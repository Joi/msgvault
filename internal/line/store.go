package line

import (
	"context"
	"crypto/sha1" //nolint:gosec // iOS backups name files by the SHA-1 of domain and path
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	storeFile  = "Line.sqlite"
	groupsFile = "UnifiedGroup.sqlite"
	// manifestFile names the index of an iPhone backup made by Finder,
	// iTunes, or idevicebackup2.
	manifestFile = "Manifest.db"
	// backupDomain is the backup domain of the LINE app group container.
	backupDomain = "AppDomainGroup-group.com.linecorp.line"
	backupPath   = "Library/Application Support/PrivateStore/"
)

// Store reads a LINE iOS message store. Line.sqlite holds chats, users and
// messages; UnifiedGroup.sqlite beside it holds group names and members.
type Store struct {
	// Path is the resolved Line.sqlite file.
	Path string
	// GroupsPath is the resolved UnifiedGroup.sqlite file, or "" when absent.
	GroupsPath string
	db         *sql.DB
	groups     *sql.DB
}

// Open resolves path to a LINE store and opens it read-only. path may be the
// Line.sqlite file, a directory holding it, or the root of an unencrypted
// iPhone backup, where the store is found through Manifest.db.
func Open(ctx context.Context, path string) (*Store, error) {
	storePath, groupsPath, err := resolve(ctx, path)
	if err != nil {
		return nil, err
	}
	db, err := openImmutable(ctx, storePath)
	if err != nil {
		return nil, fmt.Errorf("line: open %s: %w", storePath, err)
	}
	if err := checkStoreSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	st := &Store{Path: storePath, GroupsPath: groupsPath, db: db}
	if groupsPath != "" {
		st.groups, err = openImmutable(ctx, groupsPath)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("line: open %s: %w", groupsPath, err)
		}
	}
	return st, nil
}

func (s *Store) Close() error {
	err := s.db.Close()
	if s.groups != nil {
		err = errors.Join(err, s.groups.Close())
	}
	return err
}

func resolve(ctx context.Context, path string) (storePath, groupsPath string, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", "", fmt.Errorf("line: %w", err)
	}
	if !info.IsDir() {
		return abs, optionalFile(filepath.Join(filepath.Dir(abs), groupsFile)), nil
	}
	if _, err := os.Stat(filepath.Join(abs, storeFile)); err == nil {
		return filepath.Join(abs, storeFile), optionalFile(filepath.Join(abs, groupsFile)), nil
	}
	if _, err := os.Stat(filepath.Join(abs, manifestFile)); err == nil {
		return resolveBackup(ctx, abs)
	}
	return "", "", fmt.Errorf("line: %s holds neither %s nor %s", abs, storeFile, manifestFile)
}

func optionalFile(path string) string {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path
	}
	return ""
}

// resolveBackup finds the LINE store inside an iPhone backup. Backup files
// are stored under the SHA-1 of "<domain>-<relative path>", in a directory
// named by the hash's first two characters. Manifest.db lists them.
func resolveBackup(ctx context.Context, root string) (storePath, groupsPath string, err error) {
	manifest, err := openImmutable(ctx, filepath.Join(root, manifestFile))
	if err != nil {
		return "", "", fmt.Errorf("line: open backup manifest: %w", err)
	}
	defer func() { _ = manifest.Close() }()
	rows, err := manifest.QueryContext(ctx, `SELECT relativePath FROM Files WHERE domain = ? AND relativePath LIKE ?`,
		backupDomain, backupPath+"%/Messages/%.sqlite")
	if err != nil {
		if strings.Contains(err.Error(), "not a database") {
			return "", "", errors.New("line: the backup manifest is not readable; encrypted backups are not supported")
		}
		return "", "", fmt.Errorf("line: read backup manifest: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			return "", "", err
		}
		sum := sha1.Sum([]byte(backupDomain + "-" + rel)) //nolint:gosec // the backup format's file naming, not a security use
		id := hex.EncodeToString(sum[:])
		file := filepath.Join(root, id[:2], id)
		switch filepath.Base(rel) {
		case storeFile:
			storePath = file
		case groupsFile:
			groupsPath = optionalFile(file)
		}
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	if storePath == "" {
		return "", "", fmt.Errorf("line: the backup at %s has no LINE message store", root)
	}
	if _, err := os.Stat(storePath); err != nil {
		return "", "", fmt.Errorf("line: the backup manifest names a LINE store that is missing: %w", err)
	}
	return storePath, groupsPath, nil
}

// openImmutable opens a SQLite file without touching it. Backup copies carry
// a WAL header but no -wal or -shm files, which a normal read-only open
// would try to create.
func openImmutable(ctx context.Context, path string) (*sql.DB, error) {
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "immutable=1&_query_only=1"}).String()
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func checkStoreSchema(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name IN ('ZMESSAGE', 'ZCHAT', 'ZUSER')`)
	if err != nil {
		return fmt.Errorf("line: not a LINE message store: %w", err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n != 3 {
		return errors.New("line: not a LINE message store: expected the ZMESSAGE, ZCHAT and ZUSER tables")
	}
	return nil
}

// OwnerMID returns the owner's LINE user id when the store holds a Keep Memo
// chat, the one chat whose user is the owner, or "" when it does not.
func (s *Store) OwnerMID(ctx context.Context) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.ZMID FROM ZCHAT c LEFT JOIN ZUSER u ON u.ZMID = c.ZMID
		WHERE c.ZTYPE = 100 AND c.ZMID LIKE 'u%' AND u.Z_PK IS NULL ORDER BY c.Z_PK`)
	if err != nil {
		return "", fmt.Errorf("line: find owner: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var mids []string
	for rows.Next() {
		var mid string
		if err := rows.Scan(&mid); err != nil {
			return "", err
		}
		mids = append(mids, mid)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(mids) != 1 {
		return "", nil
	}
	return mids[0], nil
}

// Users returns every user by ZUSER row id.
func (s *Store) Users(ctx context.Context) (map[int64]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT Z_PK, ZMID, ZNAME, ZCUSTOMNAME, ZADDRESSBOOKNAME FROM ZUSER WHERE ZMID IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("line: read users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	users := map[int64]*User{}
	for rows.Next() {
		var u User
		var name, custom, addressBook sql.NullString
		if err := rows.Scan(&u.RowID, &u.MID, &name, &custom, &addressBook); err != nil {
			return nil, err
		}
		u.Name = displayName(custom.String, addressBook.String, name.String)
		users[u.RowID] = &u
	}
	return users, rows.Err()
}

// Chats returns every chat in row order, with group titles and members from
// UnifiedGroup.sqlite when present and room members from Line.sqlite.
func (s *Store) Chats(ctx context.Context, users map[int64]*User) ([]*Chat, error) {
	members, err := s.chatMembers(ctx, users)
	if err != nil {
		return nil, err
	}
	groups, err := s.unifiedGroups(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT Z_PK, ZTYPE, ZMID FROM ZCHAT WHERE ZMID IS NOT NULL ORDER BY Z_PK`)
	if err != nil {
		return nil, fmt.Errorf("line: read chats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var chats []*Chat
	for rows.Next() {
		var c Chat
		var kind int
		if err := rows.Scan(&c.RowID, &kind, &c.MID); err != nil {
			return nil, err
		}
		c.Kind = chatKinds[kind]
		c.Members = members[c.RowID]
		if g, ok := groups[c.MID]; ok {
			c.Title = g.name
			if len(g.members) > 0 {
				c.Members = g.members
			}
		}
		chats = append(chats, &c)
	}
	return chats, rows.Err()
}

func (s *Store) chatMembers(ctx context.Context, users map[int64]*User) (map[int64][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT Z_1CHATS, Z_12MEMBERS FROM Z_1MEMBERS ORDER BY Z_1CHATS, Z_12MEMBERS`)
	if err != nil {
		return nil, fmt.Errorf("line: read chat members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	members := map[int64][]string{}
	for rows.Next() {
		var chat, user int64
		if err := rows.Scan(&chat, &user); err != nil {
			return nil, err
		}
		if u, ok := users[user]; ok {
			members[chat] = append(members[chat], u.MID)
		}
	}
	return members, rows.Err()
}

type unifiedGroup struct {
	name    string
	members []string
}

func (s *Store) unifiedGroups(ctx context.Context) (map[string]*unifiedGroup, error) {
	groups := map[string]*unifiedGroup{}
	if s.groups == nil {
		return groups, nil
	}
	rows, err := s.groups.QueryContext(ctx, `SELECT g.ZID, g.ZNAME, u.ZMID FROM ZUNIFIEDGROUP g
		LEFT JOIN Z_1MEMBERCHATUSERS l ON l.Z_1GROUPS = g.Z_PK
		LEFT JOIN ZUNIFIEDGROUPUSER u ON u.Z_PK = l.Z_2MEMBERCHATUSERS
		WHERE g.ZID IS NOT NULL ORDER BY g.Z_PK, u.Z_PK`)
	if err != nil {
		return nil, fmt.Errorf("line: read groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var name, member sql.NullString
		if err := rows.Scan(&id, &name, &member); err != nil {
			return nil, err
		}
		g, ok := groups[id]
		if !ok {
			g = &unifiedGroup{name: strings.TrimSpace(name.String)}
			groups[id] = g
		}
		if member.Valid && member.String != "" {
			g.members = append(g.members, member.String)
		}
	}
	return groups, rows.Err()
}

// Messages visits a chat's messages in time order.
func (s *Store) Messages(ctx context.Context, chatRowID int64, visit func(*Message) error) error {
	rows, err := s.db.QueryContext(ctx, `SELECT Z_PK, ZID, ZCONTENTTYPE, ZTIMESTAMP, ZSENDER, ZTEXT, ZCONTENTMETADATA
		FROM ZMESSAGE WHERE ZCHAT = ? ORDER BY ZTIMESTAMP, Z_PK`, chatRowID)
	if err != nil {
		return fmt.Errorf("line: read messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var m Message
		var id, text sql.NullString
		var contentType, millis, sender sql.NullInt64
		var blob []byte
		if err := rows.Scan(&m.RowID, &id, &contentType, &millis, &sender, &text, &blob); err != nil {
			return err
		}
		m.ID = strings.TrimSpace(id.String)
		m.ContentType = int(contentType.Int64)
		if millis.Valid {
			m.SentAt = time.UnixMilli(millis.Int64).UTC()
		}
		m.SenderRowID = sender.Int64
		m.Text = text.String
		m.Metadata = decodeMetadata(blob)
		if err := visit(&m); err != nil {
			return err
		}
	}
	return rows.Err()
}
