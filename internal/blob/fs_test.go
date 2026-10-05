package blob

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tok1e1/doc-processor/internal/domain"
)

func TestFSPutOpen(t *testing.T) {
	ctx := context.Background()
	s, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Put(ctx, "2026/10/05/doc.pdf", strings.NewReader("hello")); err != nil {
		t.Fatal(err)
	}
	r, err := s.Open(ctx, "2026/10/05/doc.pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	if string(b) != "hello" {
		t.Fatalf("got %q", b)
	}

	if _, err := s.Open(ctx, "missing.pdf"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestFSPutIsAtomic(t *testing.T) {
	root := t.TempDir()
	s, err := NewFS(root)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Put(context.Background(), "a/doc.pdf", io.MultiReader(strings.NewReader("partial"), failingReader{})); err == nil {
		t.Fatal("expected an error")
	}
	entries, err := os.ReadDir(filepath.Join(root, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed write left files behind: %v", entries)
	}
}

func TestFSRejectsPathTraversal(t *testing.T) {
	s, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "../etc/passwd", "a/../../b", "/"} {
		if err := s.Put(context.Background(), key, strings.NewReader("x")); err == nil {
			t.Errorf("key %q must be rejected", key)
		}
	}
}
