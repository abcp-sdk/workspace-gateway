package auth

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// fileState is the on-disk enrollment state (WORKER_STATE_FILE). Exactly one of
// Token (claimed) or Code (unclaimed/released) is set. The file is the single
// source of truth across restarts; it is written atomically and mode 0600.
type fileState struct {
	Token   string `json:"token,omitempty"`
	OwnerID string `json:"owner_id,omitempty"`
	Code    string `json:"code,omitempty"`
}

// FileStore persists enrollment state to a JSON file. Corrupt/unreadable files
// fail safe (treated as unclaimed) rather than aborting startup.
type FileStore struct {
	path string
}

// NewFileStore returns a store writing to path ("" disables persistence).
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

// LoadToken returns the persisted token + owner when the worker was claimed.
func (s *FileStore) LoadToken() (string, string) {
	st := s.read()
	return st.Token, st.OwnerID
}

// LoadCode returns the persisted one-time code (empty when none).
func (s *FileStore) LoadCode() string { return s.read().Code }

// SaveToken persists the claimed identity (overwrites any code).
func (s *FileStore) SaveToken(token, ownerID string) {
	s.write(fileState{Token: token, OwnerID: ownerID})
}

// SaveCode persists the current one-time code (unclaimed/released).
func (s *FileStore) SaveCode(code string) {
	s.write(fileState{Code: code})
}

// Clear removes the persisted state file.
func (s *FileStore) Clear() {
	if s == nil || s.path == "" {
		return
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		log.Printf("auth: clear state %s: %v", s.path, err)
	}
}

func (s *FileStore) read() fileState {
	var st fileState
	if s == nil || s.path == "" {
		return st
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("auth: read state %s: %v (treating as unclaimed)", s.path, err)
		}
		return st
	}
	if err := json.Unmarshal(data, &st); err != nil {
		log.Printf("auth: corrupt state %s: %v (treating as unclaimed)", s.path, err)
		return fileState{}
	}
	return st
}

// write persists atomically (temp file + rename) so a crash never leaves a
// partially written state file.
func (s *FileStore) write(st fileState) {
	if s == nil || s.path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		log.Printf("auth: mkdir for state %s: %v", s.path, err)
		return
	}
	data, err := json.Marshal(st)
	if err != nil {
		log.Printf("auth: marshal state: %v", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".worker-state-*")
	if err != nil {
		log.Printf("auth: temp state: %v", err)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil {
		log.Printf("auth: chmod state: %v", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		log.Printf("auth: write state: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		log.Printf("auth: close state: %v", err)
		return
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		log.Printf("auth: rename state %s: %v", s.path, err)
	}
}
