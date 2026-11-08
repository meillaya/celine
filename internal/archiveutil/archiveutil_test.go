package archiveutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func writeTarGz(t *testing.T, dest string, write func(*tar.Writer)) {
	t.Helper()
	file, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	write(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTarGzRoundTripSkipsNonRegular(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "nested", "file.txt"), []byte("content"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/file.txt", filepath.Join(src, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := mkfifo(filepath.Join(src, "pipe")); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "state.tar.gz")
	if err := TarGz(src, archive); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode = %o", info.Mode().Perm())
	}
	entries, err := ListTarGz(archive)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(entries, "\n")
	if strings.Contains(joined, "link.txt") {
		t.Fatalf("archive contains symlink: %v", entries)
	}
	if strings.Contains(joined, "pipe") {
		t.Fatalf("archive contains FIFO: %v", entries)
	}

	dest := t.TempDir()
	if err := ExtractTarGz(archive, dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "nested", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "content" {
		t.Fatalf("content = %q", data)
	}
}

func TestTarGzRemovesPartialArchiveOnError(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "partial.tar.gz")
	if err := TarGz(filepath.Join(dir, "missing"), archive); err == nil {
		t.Fatal("expected an error for a missing source")
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("partial archive was left behind: %v", err)
	}
}

func TestExtractRejectsTraversalAndLinks(t *testing.T) {
	cases := []struct {
		name string
		body func(*tar.Writer)
	}{
		{
			name: "parent traversal",
			body: func(tw *tar.Writer) {
				writeHeader(t, tw, &tar.Header{Name: "../outside", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
				if _, err := tw.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "absolute path",
			body: func(tw *tar.Writer) {
				writeHeader(t, tw, &tar.Header{Name: "/etc/passwd", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
				if _, err := tw.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symlink",
			body: func(tw *tar.Writer) {
				writeHeader(t, tw, &tar.Header{Name: "link", Linkname: "/etc/passwd", Mode: 0o777, Typeflag: tar.TypeSymlink})
			},
		},
		{
			name: "hardlink",
			body: func(tw *tar.Writer) {
				writeHeader(t, tw, &tar.Header{Name: "hard", Linkname: "target", Mode: 0o644, Typeflag: tar.TypeLink})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := filepath.Join(t.TempDir(), "bad.tar.gz")
			writeTarGz(t, archive, tc.body)
			err := ExtractTarGz(archive, t.TempDir())
			if err == nil {
				t.Fatal("expected extraction to fail")
			}
		})
	}
}

func TestExtractRejectsEmptyName(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "bad.tar.gz")
	writeTarGz(t, archive, func(tw *tar.Writer) {
		writeHeader(t, tw, &tar.Header{Name: "", Mode: 0o644, Size: 0, Typeflag: tar.TypeReg})
	})
	if err := ExtractTarGz(archive, t.TempDir()); err == nil {
		t.Fatal("expected extraction to fail")
	}
}

func TestFilesUnderAndCopyTreeSkipNonRegular(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	files, err := FilesUnder(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "a.txt" {
		t.Fatalf("files = %v", files)
	}
	dst := t.TempDir()
	if err := CopyTree(src, dst, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); !os.IsNotExist(err) {
		t.Fatalf("copy created a symlink: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil || string(data) != "a" {
		t.Fatalf("copied file = %q, %v", data, err)
	}
}

func TestSafeArchivePath(t *testing.T) {
	for _, name := range []string{"a/b", "./a/b", "a/../b"} {
		clean, err := safeArchivePath(name)
		if err != nil {
			t.Errorf("safeArchivePath(%q) failed: %v", name, err)
			continue
		}
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			t.Errorf("safeArchivePath(%q) escaped: %q", name, clean)
		}
	}
	for _, name := range []string{"", ".", "../x", "a/../../x", "/abs"} {
		if _, err := safeArchivePath(name); err == nil {
			t.Errorf("safeArchivePath(%q) unexpectedly succeeded", name)
		}
	}
}

func TestSHA256File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := SHA256File(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if sum != want {
		t.Fatalf("sum = %s, want %s", sum, want)
	}
}

func writeHeader(t *testing.T, tw *tar.Writer, header *tar.Header) {
	t.Helper()
	if err := tw.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
}

func mkfifo(path string) error {
	return syscall.Mkfifo(path, 0o600)
}

// TestExtractRejectsGzipGarbage covers the error path that a truncated or
// non-gzip backup must fail before writing anything.
func TestExtractRejectsGzipGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-gzip.tar.gz")
	if err := os.WriteFile(path, []byte("not gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTarGz(path, t.TempDir()); err == nil {
		t.Fatal("expected a gzip error")
	}
}

// TestTarGzDeterministic checks the same input produces identical bytes, which
// the backup drift/verification logic relies on.
func TestTarGzDeterministic(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "b"), []byte("2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(t.TempDir(), "one.tar.gz")
	second := filepath.Join(t.TempDir(), "two.tar.gz")
	if err := TarGz(src, first); err != nil {
		t.Fatal(err)
	}
	if err := TarGz(src, second); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(first)
	b, _ := os.ReadFile(second)
	if !bytes.Equal(a, b) {
		t.Fatal("TarGz output is not deterministic")
	}
}
