package line

import (
	"context"
	"crypto/sha1" //nolint:gosec // iOS backups name files by the SHA-1 of domain and path
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	storeFile  = "Line.sqlite"
	groupsFile = "UnifiedGroup.sqlite"
	walSuffix  = "-wal"
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
	// WALPath is the message store's write-ahead log, or "" when it has none.
	WALPath string
	db      *sql.DB
	groups  *sql.DB
	// scratch holds working copies of stores that came with a WAL file; it
	// is removed on Close.
	scratch string
}

// storeFiles is one database and its write-ahead log, when one exists.
type storeFiles struct {
	db, wal string
}

// Open resolves path to a LINE store and opens it read-only. path may be the
// Line.sqlite file, a directory holding it, or the root of an unencrypted
// iPhone backup, where the store is found through Manifest.db.
func Open(ctx context.Context, path string) (*Store, error) {
	main, groups, err := resolve(ctx, path)
	if err != nil {
		return nil, err
	}
	st := &Store{Path: main.db, GroupsPath: groups.db, WALPath: main.wal}
	if err := st.openAll(ctx, main, groups); err != nil {
		return nil, errors.Join(err, st.Close())
	}
	return st, nil
}

func (s *Store) openAll(ctx context.Context, main, groups storeFiles) (err error) {
	s.db, err = s.open(ctx, main)
	if err != nil {
		return fmt.Errorf("line: open %s: %w", main.db, err)
	}
	if err := checkStoreSchema(ctx, s.db); err != nil {
		return err
	}
	if groups.db != "" {
		s.groups, err = s.open(ctx, groups)
		if err != nil {
			return fmt.Errorf("line: open %s: %w", groups.db, err)
		}
	}
	return nil
}

func (s *Store) Close() error {
	var err error
	if s.db != nil {
		err = s.db.Close()
	}
	if s.groups != nil {
		err = errors.Join(err, s.groups.Close())
	}
	if s.scratch != "" {
		err = errors.Join(err, os.RemoveAll(s.scratch))
	}
	return err
}

// open reads a database without touching it. A store that comes with a
// write-ahead log is copied, log included, to a scratch directory and read
// there, so committed rows the log still holds are seen; SQLite's immutable
// mode, which never looks for a log, reads the original otherwise.
func (s *Store) open(ctx context.Context, files storeFiles) (*sql.DB, error) {
	if files.wal == "" {
		return openImmutable(ctx, files.db)
	}
	if s.scratch == "" {
		dir, err := os.MkdirTemp("", "msgvault-line-")
		if err != nil {
			return nil, err
		}
		s.scratch = dir
	}
	copyTo := filepath.Join(s.scratch, filepath.Base(files.db))
	if err := copyFile(files.db, copyTo); err != nil {
		return nil, err
	}
	if err := copyFile(files.wal, copyTo+walSuffix); err != nil {
		return nil, err
	}
	return openCopy(ctx, copyTo)
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return err
	}
	return dst.Close()
}

// resolve finds the message store and, when present, the group store and
// their write-ahead logs.
func resolve(ctx context.Context, path string) (main, groups storeFiles, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return main, groups, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return main, groups, fmt.Errorf("line: %w", err)
	}
	if !info.IsDir() {
		return withWAL(abs), withWAL(optionalFile(filepath.Join(filepath.Dir(abs), groupsFile))), nil
	}
	if _, err := os.Stat(filepath.Join(abs, storeFile)); err == nil {
		return withWAL(filepath.Join(abs, storeFile)), withWAL(optionalFile(filepath.Join(abs, groupsFile))), nil
	}
	if _, err := os.Stat(filepath.Join(abs, manifestFile)); err == nil {
		return resolveBackup(ctx, abs)
	}
	return main, groups, fmt.Errorf("line: %s holds neither %s nor %s", abs, storeFile, manifestFile)
}

func optionalFile(path string) string {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return path
	}
	return ""
}

// withWAL pairs a database with a non-empty write-ahead log beside it.
func withWAL(db string) storeFiles {
	if db == "" {
		return storeFiles{}
	}
	files := storeFiles{db: db}
	if info, err := os.Stat(db + walSuffix); err == nil && !info.IsDir() && info.Size() > 0 {
		files.wal = db + walSuffix
	}
	return files
}

// resolveBackup finds the LINE store inside an iPhone backup. Backup files
// are stored under the SHA-1 of "<domain>-<relative path>", in a directory
// named by the hash's first two characters. Manifest.db lists them. LINE
// keeps one PrivateStore directory per account that was signed in on the
// phone, so the backup must hold exactly one message store.
func resolveBackup(ctx context.Context, root string) (main, groups storeFiles, err error) {
	manifest, err := openImmutable(ctx, filepath.Join(root, manifestFile))
	if err != nil {
		return main, groups, fmt.Errorf("line: open backup manifest: %w", err)
	}
	defer func() { _ = manifest.Close() }()
	rows, err := manifest.QueryContext(ctx, `SELECT relativePath FROM Files WHERE domain = ? AND relativePath LIKE ?`,
		backupDomain, backupPath+"%/Messages/%")
	if err != nil {
		if strings.Contains(err.Error(), "not a database") {
			return main, groups, errors.New("line: the backup manifest is not readable; encrypted backups are not supported")
		}
		return main, groups, fmt.Errorf("line: read backup manifest: %w", err)
	}
	defer func() { _ = rows.Close() }()
	// byDir maps each account's Messages directory to the backup files of
	// the two stores and their logs, keyed by file name.
	byDir := map[string]map[string]string{}
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			return main, groups, err
		}
		name := filepath.Base(rel)
		switch name {
		case storeFile, groupsFile, storeFile + walSuffix, groupsFile + walSuffix:
		default:
			continue
		}
		sum := sha1.Sum([]byte(backupDomain + "-" + rel)) //nolint:gosec // the backup format's file naming, not a security use
		id := hex.EncodeToString(sum[:])
		dir := filepath.Dir(rel)
		if byDir[dir] == nil {
			byDir[dir] = map[string]string{}
		}
		byDir[dir][name] = filepath.Join(root, id[:2], id)
	}
	if err := rows.Err(); err != nil {
		return main, groups, err
	}
	var dirs []string
	for dir, files := range byDir {
		if files[storeFile] != "" {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	switch len(dirs) {
	case 0:
		return main, groups, fmt.Errorf("line: the backup at %s has no LINE message store", root)
	case 1:
	default:
		return main, groups, fmt.Errorf("line: the backup at %s holds %d LINE message stores, one per account signed in on the phone; copy the Messages directory of the account you want out of the backup and import that directory", root, len(dirs))
	}
	files := byDir[dirs[0]]
	main = storeFiles{db: files[storeFile]}
	if _, err := os.Stat(main.db); err != nil {
		return main, groups, fmt.Errorf("line: the backup manifest names a LINE store that is missing: %w", err)
	}
	main.wal = optionalNonEmpty(files[storeFile+walSuffix])
	groups = storeFiles{db: optionalFile(files[groupsFile])}
	if groups.db != "" {
		groups.wal = optionalNonEmpty(files[groupsFile+walSuffix])
	}
	return main, groups, nil
}

func optionalNonEmpty(path string) string {
	if path == "" {
		return ""
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Size() > 0 {
		return path
	}
	return ""
}

// openImmutable opens a SQLite file without touching it. Backup copies carry
// a WAL header but no -wal or -shm files, which a normal read-only open
// would try to create.
func openImmutable(ctx context.Context, path string) (*sql.DB, error) {
	return openDSN(ctx, path, "immutable=1&_query_only=1")
}

// openCopy opens a scratch copy read-only; SQLite applies the copied
// write-ahead log and keeps its index in the scratch directory.
func openCopy(ctx context.Context, path string) (*sql.DB, error) {
	return openDSN(ctx, path, "mode=ro&_query_only=1")
}

func openDSN(ctx context.Context, path, rawQuery string) (*sql.DB, error) {
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{Scheme: "file", Path: uriPath, RawQuery: rawQuery}).String()
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
