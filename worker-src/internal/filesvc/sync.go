package filesvc

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SyncFolder unpacks a tar (optionally gzip-compressed) of a directory tree
// under `dest` ("" or "." = workspace root). Paths are NOT confined to the
// workspace: a crafted tarball may write anywhere in the container (the worker
// is trusted with its own filesystem). Symlinks are created as-is. When clean
// is true, dest is emptied first. Returns the file count.
func (s *Service) SyncFolder(tarball []byte, dest string, clean bool) (int, error) {
	if dest == "" {
		dest = "."
	}
	destPath, err := s.resolve(dest)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(destPath, 0o755); err != nil {
		return 0, err
	}
	if clean {
		if err := clearDirContents(destPath); err != nil {
			return 0, err
		}
	}

	r, err := tarReader(tarball)
	if err != nil {
		return 0, err
	}
	files := 0
	for {
		hdr, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return files, err
		}
		name := strings.TrimPrefix(filepath.ToSlash(hdr.Name), "/")
		if name == "" || name == "." {
			continue
		}
		// Resolve relative to the workspace root (dest may itself be nested).
		rel := filepath.ToSlash(filepath.Join(trimDot(dest), name))
		target, rerr := s.resolve(rel)
		if rerr != nil {
			return files, fmt.Errorf("entry %q: %w", hdr.Name, rerr)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return files, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777|0o400)
			if err != nil {
				return files, err
			}
			if _, err := io.Copy(f, r); err != nil {
				f.Close()
				return files, err
			}
			f.Close()
			files++
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return files, err
			}
		case tar.TypeLink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, err
			}
			src, rerr := s.resolve(filepath.Join(trimDot(dest), filepath.ToSlash(hdr.Linkname)))
			if rerr != nil {
				continue
			}
			_ = os.Remove(target)
			if err := os.Link(src, target); err != nil {
				return files, err
			}
		}
	}
	return files, nil
}

func tarReader(data []byte) (*tar.Reader, error) {
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return tar.NewReader(gz), nil
	}
	return tar.NewReader(bytes.NewReader(data)), nil
}

func clearDirContents(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func trimDot(p string) string {
	p = strings.TrimPrefix(filepath.ToSlash(p), "./")
	if p == "." {
		return ""
	}
	return p
}
