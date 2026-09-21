// Package filesvc implements the binary-safe file operations. Paths are NOT
// confined to the workspace root: a relative path is resolved under the
// workspace, an absolute path is used as-is, so a worker can read/write
// anywhere in its own container. On Windows the device prefix EvalSymlinks may
// return is trimmed; on unix it is a no-op.
package filesvc

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Service struct {
	root string
}

func New(root string) *Service { return &Service{root: root} }

// Root returns the workspace root.
func (s *Service) Root() string { return s.root }

// resolve maps an in-sandbox path to a real path: a relative path is joined to
// the workspace root, an absolute path is used verbatim. There is NO
// containment check — the worker may touch any path in its container. Symlinks
// are resolved when the target exists so callers see the real path.
func (s *Service) resolve(path string) (string, error) {
	p := filepath.FromSlash(path)
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.root, p)
	} else {
		p = filepath.Clean(p)
	}
	if abs, err := filepath.EvalSymlinks(p); err == nil {
		p = abs
	}
	return trimDevicePrefix(p), nil
}

func (s *Service) Read(path string) ([]byte, error) {
	p, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func (s *Service) Write(path string, data []byte) error {
	p, err := s.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// Entry is one listed path.
type Entry struct {
	Path  string
	Size  int64
	IsDir bool
}

// List stats a path. For a directory it returns the immediate children
// (sorted); for a file it returns the single entry.
func (s *Service) List(path string) (isDir bool, entries []Entry, err error) {
	p, err := s.resolve(path)
	if err != nil {
		return false, nil, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return false, nil, err
	}
	if !st.IsDir() {
		return false, []Entry{{
			Path:  s.rel(p),
			Size:  st.Size(),
			IsDir: false,
		}}, nil
	}
	dirents, err := os.ReadDir(p)
	if err != nil {
		return false, nil, err
	}
	for _, d := range dirents {
		info, serr := d.Info()
		if serr != nil {
			continue
		}
		entries = append(entries, Entry{
			Path:  s.rel(filepath.Join(p, d.Name())),
			Size:  info.Size(),
			IsDir: d.IsDir(),
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return true, entries, nil
}

// rel renders an absolute path workspace-relative with '/' separators.
func (s *Service) rel(p string) string {
	r := strings.TrimPrefix(strings.TrimPrefix(s.root, `\\?\`), `\\.\`)
	p = strings.TrimPrefix(strings.TrimPrefix(p, `\\?\`), `\\.\`)
	rel := strings.TrimPrefix(p, r)
	rel = strings.TrimPrefix(rel, string(os.PathSeparator))
	if rel == "" {
		return "."
	}
	return filepath.ToSlash(rel)
}
