package collab

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/FreePeak/xdev/internal/ai"
	"github.com/FreePeak/xdev/internal/config"
	"github.com/FreePeak/xdev/internal/session"
)

// ReplicaDir is where guest replicas live: ~/.xdev/collab (the directory next
// to the agent data dir, so a replica is never mistaken for one of the user's
// own sessions). XDEV_COLLAB_DIR overrides it — tests and sandboxes.
func ReplicaDir() string {
	if v := os.Getenv("XDEV_COLLAB_DIR"); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(config.DataDir()), "collab")
}

// ReplicaPath is the on-disk transcript replica for one room:
// <ReplicaDir>/<roomId>.jsonl.
func ReplicaPath(roomID string) string {
	return filepath.Join(ReplicaDir(), roomID+".jsonl")
}

// DefaultName is the display name shown to other participants.
func DefaultName() string {
	if v := os.Getenv("XDEV_COLLAB_NAME"); v != "" {
		return sanitizeName(v)
	}
	if v := os.Getenv("USER"); v != "" {
		return sanitizeName(v)
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return sanitizeName(h)
	}
	return "xdev"
}

// Messages replays session JSONL bytes — a host snapshot, or a single entry
// frame — through the session store's context builder, so the guest replica
// honors compaction and branch semantics exactly like /resume does. Bad lines
// (title slot, unknown entry types) are skipped: a replica must never fail to
// render because of one foreign line.
func Messages(data []byte) []ai.Message {
	s := session.OpenMem("", "")
	for _, line := range splitLines(data) {
		e, err := session.ParseEntry(line)
		if err != nil {
			continue
		}
		if err := s.Append(e); err != nil {
			continue
		}
	}
	if len(s.Entries()) == 0 {
		return nil
	}
	res, err := session.BuildContext(s.Entries(), s.LeafID(), session.SystemPrompt{})
	if err != nil {
		return nil
	}
	return res.Messages
}

// MirrorLines renders replayed messages for a plain-text mirror (the
// `xdev join` CLI): user prompts are prefixed, assistant text is not, and
// thinking/tool traffic is left out.
func MirrorLines(msgs []ai.Message) []string {
	var out []string
	for i := range msgs {
		m := &msgs[i]
		switch m.Role {
		case ai.RoleUser:
			if t := strings.TrimSpace(m.Text()); t != "" {
				out = append(out, "▸ "+t)
			}
		case ai.RoleAssistant:
			if t := strings.TrimSpace(m.Text()); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// NoticeText extracts the text of an event frame whose kind is
// EventKindNotice ("" for any other event payload).
func NoticeText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var n Notice
	if err := json.Unmarshal(raw, &n); err != nil {
		return ""
	}
	if n.Kind != EventKindNotice {
		return ""
	}
	return n.Text
}

// splitLines splits JSONL bytes into non-empty, CR-trimmed lines.
func splitLines(data []byte) [][]byte {
	var out [][]byte
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, []byte(line))
	}
	return out
}
