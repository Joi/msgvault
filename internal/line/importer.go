package line

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

const syncType = "import-line"
const checkpointVersion = 1

type lineCheckpoint struct {
	Version      int    `json:"version"`
	StorePath    string `json:"store_path"`
	StoreSize    int64  `json:"store_size"`
	StoreModTime int64  `json:"store_mod_time"`
	WALSize      int64  `json:"wal_size,omitempty"`
	WALModTime   int64  `json:"wal_mod_time,omitempty"`
	NextChat     int    `json:"next_chat"`
}

type ImportOptions struct {
	// Me is the owner's LINE user id. When empty, the store's Keep Memo chat
	// names the owner.
	Me       string
	Path     string
	Limit    int
	NoResume bool
	Logger   *slog.Logger
}

type ImportSummary struct {
	SourceID             int64
	Owner                string
	OwnerFromStore       bool
	Duration             time.Duration
	ChatsProcessed       int
	ChatsSkipped         int
	MessagesAdded        int64
	MessagesSkipped      int64
	MediaSkipped         int64
	StickersSkipped      int64
	CallsSkipped         int64
	SystemSkipped        int64
	UnsentSkipped        int64
	UnsupportedSkipped   int64
	ParticipantsResolved int
	WasResumed           bool
	LimitReached         bool
}

// Import adds the store's text messages to the archive. Media, stickers,
// calls and system notices are counted and skipped: the iOS store keeps no
// media bytes, and the other kinds carry no text.
func Import(ctx context.Context, st *store.Store, opts ImportOptions) (summary *ImportSummary, retErr error) {
	start := time.Now()
	summary = &ImportSummary{}
	defer func() { summary.Duration = time.Since(start) }()
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	reader, err := Open(ctx, opts.Path)
	if err != nil {
		return summary, err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()
	info, err := os.Stat(reader.Path)
	if err != nil {
		return summary, err
	}
	owner := strings.TrimSpace(opts.Me)
	if owner == "" {
		owner, err = reader.OwnerMID(ctx)
		if err != nil {
			return summary, err
		}
		if owner == "" {
			return summary, ErrNoOwner
		}
		summary.OwnerFromStore = true
	}
	summary.Owner = owner
	source, err := st.GetOrCreateSource(SourceType, owner)
	if err != nil {
		return summary, err
	}
	summary.SourceID = source.ID
	cp := lineCheckpoint{Version: checkpointVersion, StorePath: reader.Path, StoreSize: info.Size(), StoreModTime: info.ModTime().UnixNano()}
	if reader.WALPath != "" {
		wal, err := os.Stat(reader.WALPath)
		if err != nil {
			return summary, err
		}
		cp.WALSize, cp.WALModTime = wal.Size(), wal.ModTime().UnixNano()
	}
	if !opts.NoResume {
		prev, err := st.GetLatestCheckpointedSyncByType(ctx, source.ID, syncType)
		if err != nil && !errors.Is(err, store.ErrSyncRunNotFound) {
			return summary, err
		}
		if prev != nil && prev.CursorBefore.Valid {
			var prior lineCheckpoint
			if err := json.Unmarshal([]byte(prev.CursorBefore.String), &prior); err != nil {
				return summary, fmt.Errorf("line: decode checkpoint; use --no-resume: %w", err)
			}
			if prior.Version > checkpointVersion {
				return summary, fmt.Errorf("line: checkpoint version %d is newer than supported; use --no-resume", prior.Version)
			}
			if prior.Version == checkpointVersion && prior.StorePath == cp.StorePath && prior.StoreSize == cp.StoreSize && prior.StoreModTime == cp.StoreModTime && prior.WALSize == cp.WALSize && prior.WALModTime == cp.WALModTime {
				cp.NextChat = prior.NextChat
				summary.WasResumed = true
			}
		}
	}
	syncID, err := st.StartSyncContext(ctx, source.ID, syncType)
	if err != nil {
		return summary, err
	}
	st = st.ScopedToSync(source.ID, syncID)
	defer func() {
		cleanup := context.WithoutCancel(ctx)
		retErr = errors.Join(retErr, st.ResolveDeferredMessageRepliesContext(cleanup, source.ID, MessageType, 500), st.RecomputeConversationStatsContext(cleanup, source.ID))
		var checkpoint *store.Checkpoint
		if !opts.NoResume {
			payload, err := json.Marshal(cp)
			retErr = errors.Join(retErr, err)
			if err == nil {
				checkpoint = &store.Checkpoint{PageToken: string(payload), MessagesProcessed: summary.MessagesAdded + summary.MessagesSkipped, MessagesAdded: summary.MessagesAdded}
				retErr = errors.Join(retErr, st.UpdateSyncCheckpointContext(cleanup, syncID, checkpoint))
			}
		}
		if retErr != nil {
			retErr = errors.Join(retErr, st.FailSyncContext(cleanup, syncID, retErr.Error()))
		} else if summary.LimitReached && checkpoint != nil {
			retErr = errors.Join(retErr, st.PauseSyncWithCheckpoint(cleanup, syncID, checkpoint))
		} else {
			retErr = errors.Join(retErr, st.CompleteSyncContext(cleanup, syncID, ""))
		}
	}()
	users, err := reader.Users(ctx)
	if err != nil {
		return summary, err
	}
	chats, err := reader.Chats(ctx, users)
	if err != nil {
		return summary, err
	}
	usersByMID := make(map[string]*User, len(users))
	for _, u := range users {
		usersByMID[u.MID] = u
	}
	participants := map[string]int64{}
	named := map[string]bool{}
	resolve := func(mid, name string) (int64, error) {
		if mid == "" {
			return 0, nil
		}
		if pid, ok := participants[mid]; ok && (name == "" || named[mid]) {
			return pid, nil
		}
		pid, err := st.EnsureParticipantByIdentifierContext(ctx, SourceType, mid, name)
		if err != nil {
			return 0, err
		}
		participants[mid] = pid
		named[mid] = name != ""
		return pid, nil
	}
	defer func() { summary.ParticipantsResolved = len(participants) }()
	ownerPID, err := resolve(owner, "")
	if err != nil {
		return summary, err
	}
	imp := &chatImporter{st: st, reader: reader, users: users, usersByMID: usersByMID, resolve: resolve, sourceID: source.ID, ownerMID: owner, ownerPID: ownerPID, opts: opts, summary: summary}
	for index, chat := range chats {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if index < cp.NextChat {
			continue
		}
		if chat.Kind == chatUnknown {
			summary.ChatsSkipped++
			logger.Warn("line: skipping chat of unknown type", "chat", chat.MID)
		} else {
			if err := imp.importChat(ctx, chat); err != nil {
				if errors.Is(err, errLimitReached) {
					summary.LimitReached = true
					return summary, nil
				}
				return summary, err
			}
			logger.Info("line: chat imported", "chat", chat.MID)
		}
		cp.NextChat = index + 1
		if !opts.NoResume {
			payload, err := json.Marshal(cp)
			if err != nil {
				return summary, err
			}
			if err := st.UpdateSyncCheckpointContext(context.WithoutCancel(ctx), syncID, &store.Checkpoint{PageToken: string(payload), MessagesProcessed: summary.MessagesAdded + summary.MessagesSkipped, MessagesAdded: summary.MessagesAdded}); err != nil {
				return summary, err
			}
		}
	}
	return summary, nil
}

var errLimitReached = errors.New("line: import limit reached")

type chatImporter struct {
	st         *store.Store
	reader     *Store
	users      map[int64]*User
	usersByMID map[string]*User
	resolve    func(mid, name string) (int64, error)
	sourceID   int64
	ownerMID   string
	ownerPID   int64
	opts       ImportOptions
	summary    *ImportSummary
}

func (imp *chatImporter) importChat(ctx context.Context, chat *Chat) error {
	imp.summary.ChatsProcessed++
	convType := chat.Kind.conversationType()
	title := ""
	if chat.Kind == chatGroup {
		title = chat.Title
	}
	conversationID, err := imp.st.EnsureConversationWithTypeContext(ctx, imp.sourceID, chat.MID, convType, title)
	if err != nil {
		return fmt.Errorf("line: ensure conversation %s: %w", chat.MID, err)
	}
	if err := imp.st.EnsureConversationParticipantContext(ctx, conversationID, imp.ownerPID, "member"); err != nil {
		return fmt.Errorf("line: add owner to %s: %w", chat.MID, err)
	}
	var peerPID int64
	var peerName string
	if chat.Kind == chatDirect {
		if u, ok := imp.usersByMID[chat.MID]; ok {
			peerName = u.Name
		}
		peerPID, err = imp.resolve(chat.MID, peerName)
		if err != nil {
			return err
		}
		if err := imp.st.EnsureConversationParticipantContext(ctx, conversationID, peerPID, "member"); err != nil {
			return fmt.Errorf("line: add peer to %s: %w", chat.MID, err)
		}
	}
	if chat.Kind == chatSelf {
		peerPID = imp.ownerPID
	}
	for _, mid := range chat.Members {
		if mid == imp.ownerMID {
			continue
		}
		name := ""
		if u, ok := imp.usersByMID[mid]; ok {
			name = u.Name
		}
		pid, err := imp.resolve(mid, name)
		if err != nil {
			return err
		}
		if err := imp.st.EnsureConversationParticipantContext(ctx, conversationID, pid, "member"); err != nil {
			return fmt.Errorf("line: add member to %s: %w", chat.MID, err)
		}
	}
	var keys []string
	var candidates []*Message
	if err := imp.reader.Messages(ctx, chat.RowID, func(m *Message) error {
		if !imp.supported(m) {
			return nil
		}
		copied := *m
		candidates = append(candidates, &copied)
		keys = append(keys, chat.MID+":"+m.ID)
		return nil
	}); err != nil {
		return err
	}
	existing, err := imp.st.MessageExistsBatchContext(ctx, imp.sourceID, keys)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, m := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := chat.MID + ":" + m.ID
		if _, ok := existing[key]; ok || seen[key] {
			imp.summary.MessagesSkipped++
			continue
		}
		if imp.opts.Limit > 0 && imp.summary.MessagesAdded >= int64(imp.opts.Limit) {
			return errLimitReached
		}
		seen[key] = true
		sender, senderName := imp.ownerPID, ""
		fromMe := true
		if m.SenderRowID != 0 {
			fromMe = false
			u, ok := imp.users[m.SenderRowID]
			if !ok {
				sender = 0
			} else {
				senderName = u.Name
				sender, err = imp.resolve(u.MID, u.Name)
				if err != nil {
					return err
				}
			}
		}
		members := []store.ConversationParticipantRef{{ParticipantID: imp.ownerPID, Role: "member"}}
		if sender != 0 && sender != imp.ownerPID {
			members = append(members, store.ConversationParticipantRef{ParticipantID: sender, Role: "member"})
		}
		recipients := []store.RecipientSet{}
		if sender != 0 {
			recipients = append(recipients, store.RecipientSet{Type: "from", ParticipantIDs: []int64{sender}, DisplayNames: []string{senderName}})
		}
		if peerPID != 0 {
			to, name := imp.ownerPID, ""
			if fromMe {
				to, name = peerPID, peerName
			}
			recipients = append(recipients, store.RecipientSet{Type: "to", ParticipantIDs: []int64{to}, DisplayNames: []string{name}})
		}
		body := m.body()
		var metadata *sql.NullString
		if m.Metadata.ReplyTo != "" {
			encoded, err := json.Marshal(struct {
				ReferencedMessageID string `json:"referenced_message_id"`
			}{ReferencedMessageID: chat.MID + ":" + m.Metadata.ReplyTo})
			if err != nil {
				return err
			}
			metadata = &sql.NullString{String: string(encoded), Valid: true}
		}
		snippet := textutil.PrefixRunes(body, 200)
		if _, err := imp.st.PersistMessageContext(ctx, &store.MessagePersistData{
			Message: &store.Message{SourceID: imp.sourceID, SourceMessageID: key, MessageType: MessageType,
				SentAt: sql.NullTime{Time: m.SentAt, Valid: !m.SentAt.IsZero()}, SenderID: sql.NullInt64{Int64: sender, Valid: sender != 0},
				IsFromMe: fromMe, Snippet: sql.NullString{String: snippet, Valid: snippet != ""}, SizeEstimate: int64(len(body))},
			Conversation: &store.ConversationPersistData{SourceConversationID: chat.MID, ConversationType: convType, Title: title, Participants: members, PreserveExistingParticipants: true},
			BodyText:     sql.NullString{String: body, Valid: true}, Metadata: metadata, Recipients: recipients, FTS: &store.FTSDoc{Body: body},
		}); err != nil {
			return fmt.Errorf("line: persist %s: %w", key, err)
		}
		imp.summary.MessagesAdded++
	}
	return nil
}

// supported decides whether a row becomes an archived message and counts the
// rows that do not.
func (imp *chatImporter) supported(m *Message) bool {
	if m.ID == "" {
		imp.summary.UnsentSkipped++
		return false
	}
	switch m.kind() {
	case kindText:
		if m.body() == "" {
			imp.summary.UnsupportedSkipped++
			return false
		}
		return true
	case kindMedia:
		imp.summary.MediaSkipped++
	case kindSticker:
		imp.summary.StickersSkipped++
	case kindCall:
		imp.summary.CallsSkipped++
	case kindSystem:
		imp.summary.SystemSkipped++
	case kindUnsent:
		imp.summary.UnsentSkipped++
	default:
		imp.summary.UnsupportedSkipped++
	}
	return false
}
