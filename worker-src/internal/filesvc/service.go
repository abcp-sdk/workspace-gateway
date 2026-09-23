// Package filesvc implements the binary-safe file operations. Paths are NOT
// confined to the workspace root: a relative path is resolved under the
// workspace, an absolute path is used as-is, so a worker can read/write
// anywhere in its own container. On Windows the device prefix EvalSymlinks may
// return is trimmed; on unix it is a no-op.
package filesvc

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
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
	// NOTE: `~` is NOT expanded here. It is a CLIENT-side alias for the
	// workspace root (see the control panel's paths.ts); API callers pass
	// absolute paths. A bare `~` is treated as a literal relative name.
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

// ReadWindow reads a 0-based inclusive-exclusive line window of a file.
// start<0 counts from the end (like JobOutput); end<=0 means through EOF.
// When the file has no trailing newline the final partial line still counts
// as a line. Omitting the window (start=0,end=0) reads the whole file.
// Returns the content, the file's total line count and the window actually
// served (clamped to the file).
func (s *Service) ReadWindow(path string, start, end int32) ([]byte, int32, int32, int32, error) {
	p, err := s.resolve(path)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, 0, 0, err
	}
	defer f.Close()

	// Two passes when a window is requested: count lines, then seek. A single
	// streaming pass with a ring would avoid the second read but complicates
	// negative starts; files here are sandbox-sized, not petabytes.
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	total := int32(0)
	for sc.Scan() {
		total++
	}
	if err := sc.Err(); err != nil {
		return nil, 0, 0, 0, err
	}

	s0, e0 := clampWindow(start, end, total)
	if e0 <= s0 {
		return nil, total, s0, s0, nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, 0, 0, err
	}
	sc = bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var buf bytes.Buffer
	i := int32(0)
	for sc.Scan() {
		if i >= s0 && i < e0 {
			if buf.Len() > 0 || i > s0 {
				buf.WriteByte('\n')
			}
			buf.Write(sc.Bytes())
		}
		i++
		if i >= e0 {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, 0, 0, err
	}
	return buf.Bytes(), total, s0, e0, nil
}

// clampWindow resolves the requested [start,end) against the line count.
func clampWindow(start, end, total int32) (int32, int32) {
	s := start
	if s < 0 {
		s = total + s
		if s < 0 {
			s = 0
		}
	}
	if s > total {
		s = total
	}
	e := end
	if e <= 0 || e > total {
		e = total
	}
	if e < s {
		e = s
	}
	return s, e
}

// Read reads the whole file (backward-compatible shorthand).
func (s *Service) Read(path string) ([]byte, error) {
	p, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// Write writes a file, creating parent directories. An EXISTING file keeps
// its mode (so overwriting a script does not strip the exec bit); a new file
// is 0644.
func (s *Service) Write(path string, data []byte) error {
	p, err := s.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(p); err == nil {
		mode = st.Mode().Perm()
	}
	return os.WriteFile(p, data, mode)
}

// Delete removes a file or directory tree.
func (s *Service) Delete(path string) error {
	p, err := s.resolve(path)
	if err != nil {
		return err
	}
	return os.RemoveAll(p)
}

// Move renames (falling back to copy+delete across filesystems).
func (s *Service) Move(from, to string) error {
	src, err := s.resolve(from)
	if err != nil {
		return err
	}
	dst, err := s.resolve(to)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	return copyTree(src, dst)
}

// Copy copies a file or directory tree onto `to`.
func (s *Service) Copy(from, to string) error {
	src, err := s.resolve(from)
	if err != nil {
		return err
	}
	dst, err := s.resolve(to)
	if err != nil {
		return err
	}
	return copyTree(src, dst)
}

// copyTree recursively copies src onto dst (dst's parent is created; an
// existing dst file is replaced, an existing dst dir is merged into).
func copyTree(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return copyFile(src, dst, st.Mode().Perm())
	}
	if err := os.MkdirAll(dst, st.Mode().Perm()); err != nil {
		return err
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".wcopy"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// Entry is one listed path.
type Entry struct {
	Path  string
	Size  int64
	IsDir bool
}

// List stats a path. For a directory it returns the immediate children
// (sorted); for a file it returns the single entry. When depth > 1 the tree
// is expanded breadth-first up to that many levels (capped by limit; the
// total is still reported through the returned slice's length).
func (s *Service) List(path string, depth, limit int) (isDir bool, entries []Entry, truncated bool, err error) {
	if depth <= 0 {
		depth = 1
	}
	if limit <= 0 {
		limit = 1000
	}
	if limit > 10000 {
		limit = 10000
	}
	p, err := s.resolve(path)
	if err != nil {
		return false, nil, false, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return false, nil, false, err
	}
	if !st.IsDir() {
		return false, []Entry{{
			Path:  s.rel(p),
			Size:  st.Size(),
			IsDir: false,
		}}, false, nil
	}

	// BFS level by level; children sorted within each directory.
	type qitem struct {
		dir  string
		dept int
	}
	queue := []qitem{{dir: p, dept: 1}}
	entries = make([]Entry, 0, 64)
	truncated = false
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		dirents, err := os.ReadDir(it.dir)
		if err != nil {
			continue // unreadable subdir: skip, keep listing
		}
		// ReadDir is already sorted by name.
		for _, d := range dirents {
			full := filepath.Join(it.dir, d.Name())
			info, serr := d.Info()
			if serr != nil {
				continue
			}
			if len(entries) >= limit {
				truncated = true
				return true, entries, truncated, nil
			}
			entries = append(entries, Entry{
				Path:  s.rel(full),
				Size:  info.Size(),
				IsDir: d.IsDir(),
			})
			if d.IsDir() && it.dept < depth {
				queue = append(queue, qitem{dir: full, dept: it.dept + 1})
			}
		}
	}
	return true, entries, false, nil
}

// rel renders an absolute path workspace-relative with '/' separators.
func (s *Service) rel(p string) string {
	r := strings.TrimPrefix(strings.TrimPrefix(s.root, `\\?\`), `\\.\`)
	p = strings.TrimPrefix(strings.TrimPrefix(p, `\\?\`), `\\.\`)
	if p == r {
		return "."
	}
	// Only relativize paths actually INSIDE the workspace. A path outside the
	// root (the worker may touch anywhere in its container) is returned
	// ABSOLUTE — stripping the root prefix unconditionally turned e.g. "/root"
	// into "root", which a client then wrongly re-rooted under the workspace.
	if strings.HasPrefix(p, r+string(os.PathSeparator)) {
		rel := strings.TrimPrefix(p, r)
		rel = strings.TrimPrefix(rel, string(os.PathSeparator))
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(p)
}
