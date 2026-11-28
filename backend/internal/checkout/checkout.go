// Package checkout unpacks a repository tarball for a review.
package checkout

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// Extract unpacks a gzipped GitHub tarball into dir, without its top-level folder. os.Root refuses
// any path that would end up outside dir. Symlinks and other special files are skipped, so the
// checkout holds only regular files and folders. It stops with an error after maxBytes of content.
func Extract(r io.Reader, dir string, maxBytes int64) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("read tarball: %w", err)
	}
	tr := tar.NewReader(gz)
	var total int64
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tarball: %w", err)
		}
		// GitHub puts everything in one "owner-repo-sha/" folder: drop it.
		_, name, _ := strings.Cut(h.Name, "/")
		name = path.Clean(name)
		if name == "." || name == "" {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
		case tar.TypeReg:
			if total += h.Size; total > maxBytes {
				return fmt.Errorf("the repository is larger than %d MB", maxBytes>>20)
			}
			if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
				return fmt.Errorf("create %s: %w", path.Dir(name), err)
			}
			if err := writeFile(root, name, tr); err != nil {
				return fmt.Errorf("write %s: %w", name, err)
			}
		}
	}
}

func writeFile(root *os.Root, name string, r io.Reader) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
