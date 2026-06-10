package live

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unicode"

	"github.com/valbaudo/awf/agent"
)

const (
	SessionSchema            = "awf-live-session-v1"
	PhaseIntentRecorded      = TurnPhase("intent_recorded")
	PhaseProviderTurnStarted = TurnPhase("provider_turn_started")
)

var (
	ErrLiveHomeDrift          = errors.New("agent/live: live home drift")
	ErrUnsafeRoot             = errors.New("agent/live: unsafe root")
	ErrInvalidSessionKey      = errors.New("agent/live: invalid session key")
	ErrSessionDrift           = errors.New("agent/live: session drift")
	ErrActiveTurnNotClearable = errors.New("agent/live: active turn not clearable")
	ErrActiveTurnMismatch     = errors.New("agent/live: active turn mismatch")
	ErrLiveLeaseConflict      = agent.ErrLiveLeaseConflict
	ErrLiveLeaseStaleOwned    = agent.ErrLiveLeaseStaleOwned
)

type Root struct {
	Path string
	Pin  HomePin
}

type HomePin struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

type TurnPhase string

type ActiveTurn struct {
	Phase          TurnPhase `json:"phase"`
	RunID          string    `json:"run_id"`
	NodePath       string    `json:"node_path"`
	CurrentEpoch   int       `json:"current_epoch"`
	NextEpoch      int       `json:"next_epoch"`
	PromptDigest   string    `json:"prompt_digest"`
	LeaseID        string    `json:"lease_id"`
	ProviderTurnID string    `json:"provider_turn_id,omitempty"`
}

type CommittedTurn struct {
	RunID          string `json:"run_id"`
	NodePath       string `json:"node_path"`
	Epoch          int    `json:"epoch"`
	ProviderTurnID string `json:"provider_turn_id"`
	CommittedUnix  int64  `json:"committed_unix"`
}

type SessionRecord struct {
	Schema                 string         `json:"schema"`
	AdapterRef             string         `json:"adapter_ref"`
	SessionKey             string         `json:"session_key"`
	CWD                    string         `json:"cwd,omitempty"`
	CanonicalCWD           string         `json:"canonical_cwd"`
	ProviderSessionID      string         `json:"provider_session_id,omitempty"`
	TmuxSession            string         `json:"tmux_session,omitempty"`
	TranscriptPath         string         `json:"transcript_path,omitempty"`
	OwnerRunID             string         `json:"owner_run_id,omitempty"`
	LastSeenUnix           int64          `json:"last_seen_unix,omitempty"`
	AdapterVersion         string         `json:"adapter_version,omitempty"`
	ProviderBinary         string         `json:"provider_binary,omitempty"`
	ProviderProtocolSchema string         `json:"provider_protocol_schema,omitempty"`
	ActiveTurn             *ActiveTurn    `json:"active_turn,omitempty"`
	LastCommittedTurn      *CommittedTurn `json:"last_committed_turn,omitempty"`
}

func OpenRoot(stateDir string, env map[string]string) (Root, error) {
	rootPath := ""
	if env != nil {
		rootPath = env["AWF_LIVE_HOME"]
	}
	if rootPath == "" {
		rootPath = filepath.Join(stateDir, "live")
	}
	abs, err := filepath.Abs(rootPath)
	if err != nil {
		return Root{}, wrapIO("resolve live root", err)
	}
	if err := rejectSymlinkComponents(abs); err != nil {
		return Root{}, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return Root{}, wrapIO("create live root", err)
	}
	if err := rejectSymlinkComponents(abs); err != nil {
		return Root{}, err
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Root{}, wrapIO("canonicalize live root", err)
	}
	info, err := os.Stat(canon)
	if err != nil {
		return Root{}, wrapIO("stat live root", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return Root{}, fmt.Errorf("%w: %s", ErrUnsafeRoot, canon)
	}
	if runtime.GOOS != "windows" && !ownedByCurrentUser(info) {
		return Root{}, fmt.Errorf("%w: %s", ErrUnsafeRoot, canon)
	}
	pin := homePin(canon)
	return Root{Path: canon, Pin: pin}, nil
}

func CheckHomePin(pin HomePin, stateDir string, env map[string]string) error {
	root, err := OpenRoot(stateDir, env)
	if err != nil {
		return err
	}
	if root.Pin.Path != pin.Path || root.Pin.Digest != pin.Digest {
		return fmt.Errorf("%w: pinned %s current %s", ErrLiveHomeDrift, pin.Path, root.Pin.Path)
	}
	return nil
}

func ValidateSessionKey(key string) error {
	if key == "" || key == "." || key == ".." {
		return fmt.Errorf("%w: %q", ErrInvalidSessionKey, key)
	}
	for _, r := range key {
		if r == '/' || r == '\\' || unicode.IsSpace(r) {
			return fmt.Errorf("%w: %q", ErrInvalidSessionKey, key)
		}
	}
	return nil
}

func CheckSessionDrift(existing, next SessionRecord) error {
	if existing.AdapterRef != next.AdapterRef ||
		existing.SessionKey != next.SessionKey ||
		existing.CanonicalCWD != next.CanonicalCWD ||
		existing.ProviderBinary != next.ProviderBinary ||
		existing.ProviderProtocolSchema != next.ProviderProtocolSchema ||
		existing.Schema != SessionSchema ||
		next.Schema != SessionSchema {
		return ErrSessionDrift
	}
	return nil
}

func SessionRecordPath(root Root, adapterRef, sessionKey string) string {
	return filepath.Join(root.Path, "sessions", escapePathComponent(adapterRef), escapePathComponent(sessionKey)+".json")
}

func WriteSessionRecord(root Root, rec SessionRecord) error {
	if err := ValidateSessionKey(rec.SessionKey); err != nil {
		return err
	}
	rec.Schema = SessionSchema
	path := SessionRecordPath(root, rec.AdapterRef, rec.SessionKey)
	if existing, err := ReadSessionRecord(root, rec.AdapterRef, rec.SessionKey); err == nil {
		if err := CheckSessionDrift(existing, rec); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeJSONAtomic(path, rec)
}

func ReadSessionRecord(root Root, adapterRef, sessionKey string) (SessionRecord, error) {
	if err := ValidateSessionKey(sessionKey); err != nil {
		return SessionRecord{}, err
	}
	path := SessionRecordPath(root, adapterRef, sessionKey)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SessionRecord{}, os.ErrNotExist
		}
		return SessionRecord{}, wrapIO("read session record", err)
	}
	var rec SessionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return SessionRecord{}, wrapIO("decode session record", err)
	}
	if rec.Schema != SessionSchema {
		return SessionRecord{}, ErrSessionDrift
	}
	return rec, nil
}

func RecordTurnIntent(root Root, adapterRef, sessionKey string, turn ActiveTurn) error {
	rec, err := ReadSessionRecord(root, adapterRef, sessionKey)
	if err != nil {
		return err
	}
	rec.ActiveTurn = &turn
	return WriteSessionRecord(root, rec)
}

func ClearActiveTurnIfSafe(root Root, adapterRef, sessionKey string) error {
	rec, err := ReadSessionRecord(root, adapterRef, sessionKey)
	if err != nil {
		return err
	}
	if rec.ActiveTurn == nil {
		return nil
	}
	if rec.ActiveTurn.Phase != PhaseIntentRecorded || rec.ActiveTurn.ProviderTurnID != "" {
		return ErrActiveTurnNotClearable
	}
	rec.ActiveTurn = nil
	return WriteSessionRecord(root, rec)
}

func homePin(path string) HomePin {
	sum := sha256.Sum256([]byte(path))
	return HomePin{Path: path, Digest: "sha256:" + hex.EncodeToString(sum[:])}
}

func rejectSymlinkComponents(path string) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	rest := strings.TrimPrefix(clean, volume)
	cur := volume
	if filepath.IsAbs(rest) {
		cur += string(os.PathSeparator)
		rest = strings.TrimPrefix(rest, string(os.PathSeparator))
	}
	for _, part := range strings.Split(rest, string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return wrapIO("lstat path component", err)
		}
		if runtime.GOOS == "darwin" && cur == string(os.PathSeparator)+"var" && info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink component %s", ErrUnsafeRoot, cur)
		}
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return int(stat.Uid) == os.Getuid()
}

func writeJSONAtomic(path string, value any) error {
	dir := filepath.Dir(path)
	if err := ensurePrivateDir(dir); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return wrapIO("create temp file", err)
	}
	tmpName := file.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return wrapIO("chmod temp file", err)
	}
	enc := json.NewEncoder(file)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		_ = file.Close()
		return wrapIO("encode json", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return wrapIO("sync temp file", err)
	}
	if err := file.Close(); err != nil {
		return wrapIO("close temp file", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return wrapIO("rename temp file", err)
	}
	keep = true
	if err := syncDir(dir); err != nil {
		return err
	}
	return nil
}

func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return wrapIO("open directory", err)
	}
	defer func() {
		_ = file.Close()
	}()
	if err := file.Sync(); err != nil {
		return wrapIO("sync directory", err)
	}
	return nil
}

func ensurePrivateDir(dir string) error {
	if err := rejectSymlinkComponents(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return wrapIO("create parent directory", err)
	}
	if err := rejectSymlinkComponents(dir); err != nil {
		return err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return wrapIO("stat parent directory", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%w: %s", ErrUnsafeRoot, dir)
	}
	if runtime.GOOS != "windows" && !ownedByCurrentUser(info) {
		return fmt.Errorf("%w: %s", ErrUnsafeRoot, dir)
	}
	return nil
}

func escapePathComponent(s string) string {
	if s == "" {
		return "_"
	}
	if s == "." {
		return "%2E"
	}
	if s == ".." {
		return "%2E%2E"
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
			continue
		}
		_, _ = fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func wrapIO(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %s: %w", agent.ErrLiveRegistryIO, op, err)
}
