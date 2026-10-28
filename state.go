package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// stateStore owns the plugin's private files. Everything is written with a
// temp file + rename so a crash cannot leave a half-written pool.
type stateStore struct {
	dir string
}

func newStateStore(dir string) (*stateStore, error) {
	dir = filepath.Clean(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &stateStore{dir: dir}, nil
}

func (s *stateStore) path(name string) string {
	return filepath.Join(s.dir, name)
}

func (s *stateStore) readJSON(name string, out any) bool {
	raw, err := os.ReadFile(s.path(name))
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}

func (s *stateStore) writeJSON(name string, value any) error {
	if s == nil {
		return fmt.Errorf("state store is nil")
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	path := s.path(name)
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (s *stateStore) deviceID() string {
	var file struct {
		DeviceID string `json:"device_id"`
	}
	if s.readJSON("device.json", &file) {
		return file.DeviceID
	}
	return ""
}

func (s *stateStore) saveDeviceID(id string) error {
	return s.writeJSON("device.json", map[string]string{"device_id": id})
}

// logRing is a tiny in-memory diagnostic log. The plugin has no management UI,
// so this exists mainly for tests and for returning a useful message on failure.
type logRing struct {
	mu      sync.Mutex
	entries []string
	limit   int
}

func newLogRing(limit int) *logRing {
	if limit <= 0 {
		limit = 200
	}
	return &logRing{limit: limit}
}

func (l *logRing) add(level, scope, message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	line := time.Now().Format("15:04:05") + " " + level + " [" + scope + "] " + message
	l.entries = append(l.entries, line)
	if len(l.entries) > l.limit {
		l.entries = l.entries[len(l.entries)-l.limit:]
	}
}

func (l *logRing) info(scope, message string)  { l.add("INFO", scope, message) }
func (l *logRing) warn(scope, message string)  { l.add("WARN", scope, message) }
func (l *logRing) error(scope, message string) { l.add("ERROR", scope, message) }

func (l *logRing) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.entries))
	copy(out, l.entries)
	return out
}

// diag writes a line to <data_dir>/plugin.log when debug_dump is enabled.
func (a *app) diag(format string, args ...any) {
	if !a.currentConfig().DebugDump {
		return
	}
	store := a.stateStore()
	if store == nil {
		return
	}
	path := filepath.Join(store.dir, "plugin.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006-01-02 15:04:05"), fmt.Sprintf(format, args...))
}
