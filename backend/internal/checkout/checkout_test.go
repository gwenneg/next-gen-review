package checkout

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name, body, link string
	typ              byte
}

func tarball(t *testing.T, entries ...entry) *bytes.Buffer {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.link}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return &buf
}

func TestExtract(t *testing.T) {
	tests := []struct {
		name    string
		entries []entry
		max     int64
		err     string
		files   map[string]string // expected files under dir
	}{
		{"files without the top folder", []entry{
			{name: "o-r-abc/", typ: tar.TypeDir},
			{name: "o-r-abc/main.go", body: "package main", typ: tar.TypeReg},
			{name: "o-r-abc/sub/a.txt", body: "a", typ: tar.TypeReg},
		}, 1 << 20, "", map[string]string{"main.go": "package main", "sub/a.txt": "a"}},
		{"symlinks skipped", []entry{
			{name: "o-r-abc/link", link: "/etc/passwd", typ: tar.TypeSymlink},
			{name: "o-r-abc/a.txt", body: "a", typ: tar.TypeReg},
		}, 1 << 20, "", map[string]string{"a.txt": "a"}},
		{"path escaping the folder refused", []entry{
			{name: "o-r-abc/../../escape.txt", body: "x", typ: tar.TypeReg},
		}, 1 << 20, "escape", nil},
		{"too large", []entry{
			{name: "o-r-abc/big", body: "0123456789", typ: tar.TypeReg},
		}, 5, "larger than", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			dir := filepath.Join(parent, "src")
			err := Extract(tarball(t, tt.entries...), dir, tt.max)
			if (tt.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.err)) {
				t.Fatalf("Extract() = %v, want error containing %q", err, tt.err)
			}
			if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
				t.Error("a file was written outside the folder")
			}
			var got []string
			_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
				if !d.IsDir() {
					rel, _ := filepath.Rel(dir, p)
					got = append(got, rel)
				}
				return nil
			})
			if len(got) != len(tt.files) {
				t.Errorf("files = %v, want %v", got, tt.files)
			}
			for name, body := range tt.files {
				if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != body {
					t.Errorf("%s = %q, %v; want %q", name, b, err, body)
				}
			}
		})
	}
}
