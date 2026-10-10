package gateway

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"strconv"
	"strings"
	"time"
)

// TokenEnv is the environment variable that overrides the token file.
const TokenEnv = "TELEGRAM_BOT_TOKEN"

// ResolveToken applies the token precedence: TELEGRAM_BOT_TOKEN, then
// <dataDir>/gateway.token ("" when neither is set).
func ResolveToken(dataDir string) string {
	if v := strings.TrimSpace(os.Getenv(TokenEnv)); v != "" {
		return v
	}
	return ReadTokenFile(dataDir)
}

// ReadTokenFile reads the stored token ("" when absent).
func ReadTokenFile(dataDir string) string { return loadToken(TokenPath(dataDir)) }

// TokenFilePath is the 0600 file `gateway setup` writes and the daemon reads
// when TELEGRAM_BOT_TOKEN is unset.
func TokenFilePath(dataDir string) string { return TokenPath(dataDir) }

// OpenSessions opens the chat registry for a data dir (status command).
func OpenSessions(dataDir string) (*Chats, error) {
	return NewChats(filepath.Join(dataDir, "gateway", "sessions.json"))
}

// WriteTokenFile stores the token at 0600 — the file the daemon reads when
// TELEGRAM_BOT_TOKEN is unset. It never touches config.yml or the repo.
func WriteTokenFile(dataDir, token string) error { return WriteToken(dataDir, token) }

// --- pid file ---------------------------------------------------------------

// PIDPath is the daemon's pid record (gateway/gateway.pid).
func PIDPath(dataDir string) string { return filepath.Join(dataDir, "gateway", "gateway.pid") }

// WritePID records this process as the running daemon.
func WritePID(dataDir string) error {
	if err := os.MkdirAll(filepath.Dir(PIDPath(dataDir)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(PIDPath(dataDir), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

// ReadPID reads the recorded pid (0 when absent or unreadable).
func ReadPID(dataDir string) int {
	raw, err := os.ReadFile(PIDPath(dataDir))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return pid
}

// RemovePID deletes the pid record (the daemon's own exit path).
func RemovePID(dataDir string) { _ = os.Remove(PIDPath(dataDir)) }

// FormatChatList renders an allowlist deterministically for status output.
func FormatChatList(ids []int64) string {
	if len(ids) == 0 {
		return ""
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.FormatInt(id, 10))
	}
	return strings.Join(out, ", ")
}

// Status is the daemon's own liveness record, written at startup so `xdev
// gateway status` and `xdev ps` can answer without asking the supervisor.
type Status struct {
	PID       int       `json:"pid"`
	Version   string    `json:"version,omitempty"`
	BotName   string    `json:"botName,omitempty"`
	Workspace string    `json:"workspace,omitempty"`
	Started   time.Time `json:"started"`
}

// WriteStatus records the daemon's identity.
func WriteStatus(dataDir string, st Status) error {
	if err := os.MkdirAll(filepath.Dir(StatusPath(dataDir)), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := StatusPath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, StatusPath(dataDir))
}

// ReadStatus reads the daemon record (zero value when absent).
func ReadStatus(dataDir string) Status {
	var st Status
	raw, err := os.ReadFile(StatusPath(dataDir))
	if err != nil {
		return st
	}
	_ = json.Unmarshal(raw, &st)
	return st
}

// DaemonStatus is a one-line human report of the recorded daemon state.
func DaemonStatus(dataDir string) string {
	st := ReadStatus(dataDir)
	pid := ReadPID(dataDir)
	switch {
	case pid == 0 && st.PID == 0:
		return "never started"
	case pid != 0 && st.PID != 0 && pid == st.PID:
		return fmt.Sprintf("pid %d since %s (up %s)", pid, st.Started.Format("2006-01-02 15:04"), time.Since(st.Started).Round(time.Second))
	case st.PID != 0:
		return fmt.Sprintf("last run pid %d started %s (no pid file now)", st.PID, st.Started.Format("2006-01-02 15:04"))
	default:
		return fmt.Sprintf("pid %d recorded", pid)
	}
}

// ParseChatList parses a comma/space-separated allowlist of numeric chat
// ids. A non-numeric entry is an error rather than a silently dropped one:
// dropping one entry turns a working bridge into one that refuses its owner,
// which is the exact failure a setup path must not produce.
func ParseChatList(s string) ([]int64, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == ';'
	})
	out := make([]int64, 0, len(fields))
	for _, f := range fields {
		id, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a numeric chat id", f)
		}
		out = append(out, id)
	}
	return out, nil
}
