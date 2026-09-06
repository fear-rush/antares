package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/enowdev/antares/internal/config"
)

// richSentStore remembers message_id -> source markdown for rich sends.
// Telegram does not echo rich content back in reply_to_message, so without
// this index a reply to a rich message arrives with no quotable text. It is
// the single source of truth for that index: best-effort, dependency-free,
// every operation degrades to a no-op or empty so it can never break a send.

const (
	richSentMaxEntries = 1000
	richSentMaxChars   = 2000
)

type richSentEntry struct {
	Text string `json:"t"`
	TS   int64  `json:"ts"`
}

var richSent = struct {
	sync.Mutex
	byID map[string]richSentEntry
}{byID: map[string]richSentEntry{}}

func richSentKey(chatID, messageID string) string { return chatID + ":" + messageID }

func richSentPath() string {
	return filepath.Join(config.Path("state"), "rich_sent_index.json")
}

// richSentLoad hydrates the in-memory index from disk. Called once at adapter
// start; a corrupt or missing file just means an empty index.
func richSentLoad() {
	path := richSentPath()
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var data map[string]richSentEntry
	if err := json.Unmarshal(b, &data); err != nil || data == nil {
		return
	}
	richSent.Lock()
	defer richSent.Unlock()
	for k, v := range data {
		if v.Text != "" {
			richSent.byID[k] = v
		}
	}
}

// richSentRecord persists text for (chatID, messageID). No-op on any failure.
func richSentRecord(chatID, messageID, text string) {
	if chatID == "" || messageID == "" || text == "" {
		return
	}
	if len(text) > richSentMaxChars {
		text = text[:richSentMaxChars]
	}
	richSent.Lock()
	richSent.byID[richSentKey(chatID, messageID)] = richSentEntry{Text: text, TS: time.Now().Unix()}
	for len(richSent.byID) > richSentMaxEntries {
		oldest, oldestTS := "", int64(0)
		first := true
		for k, v := range richSent.byID {
			if first || v.TS < oldestTS {
				oldest, oldestTS, first = k, v.TS, false
			}
		}
		if oldest == "" {
			break
		}
		delete(richSent.byID, oldest)
	}
	snapshot := make(map[string]richSentEntry, len(richSent.byID))
	for k, v := range richSent.byID {
		snapshot[k] = v
	}
	richSent.Unlock()

	path := richSentPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	b, err := json.Marshal(snapshot)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "rich_sent_*.tmp")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	_ = os.Rename(tmpName, path)
}

// richSentLookup returns stored text for (chatID, messageID), or "".
func richSentLookup(chatID, messageID string) string {
	if chatID == "" || messageID == "" {
		return ""
	}
	richSent.Lock()
	defer richSent.Unlock()
	return richSent.byID[richSentKey(chatID, messageID)].Text
}
