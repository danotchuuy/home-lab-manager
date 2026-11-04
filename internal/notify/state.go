package notify

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// state is what the service remembers across restarts: when each event
// was last sent (by Key), and the history.
type state struct {
	Sent    map[string]time.Time `json:"sent"`
	History []Record             `json:"history"`
}

// forgetSentAfter is how long a sent-once Key is remembered.
const forgetSentAfter = 90 * 24 * time.Hour

func loadState(path string, log *slog.Logger) state {
	st := state{Sent: map[string]time.Time{}}
	if path == "" {
		return st
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Warn("read notification state", "err", err)
		}
		return st
	}
	if err := json.Unmarshal(data, &st); err != nil {
		log.Warn("notification state is unreadable; starting afresh", "err", err)
		return state{Sent: map[string]time.Time{}}
	}
	if st.Sent == nil {
		st.Sent = map[string]time.Time{}
	}
	if len(st.History) > historySize {
		st.History = st.History[len(st.History)-historySize:]
	}
	return st
}

// saveState writes the state, dropping sent Keys too old to matter.
func (s *Service) saveState() {
	if s.opts.StatePath == "" {
		return
	}
	s.mu.Lock()
	now := s.opts.Now()
	for k, at := range s.state.Sent {
		if now.Sub(at) > forgetSentAfter {
			delete(s.state.Sent, k)
		}
	}
	data, err := json.Marshal(s.state)
	s.mu.Unlock()
	if err != nil {
		s.opts.Log.Warn("save notification state", "err", err)
		return
	}
	if err := writeFileAtomic(s.opts.StatePath, data); err != nil {
		s.opts.Log.Warn("save notification state", "err", err)
	}
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
