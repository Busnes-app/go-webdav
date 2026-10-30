package webdav

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/emersion/go-webdav/internal"
)

func newLocalFS(t *testing.T) (LocalFileSystem, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{filepath.Join(root, "dir", "sub"), outside} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	for p, s := range map[string]string{
		filepath.Join(root, "dir", "a.txt"):        "a",
		filepath.Join(root, "dir", "sub", "b.txt"): "b",
		filepath.Join(outside, "secret"):           "secret",
	} {
		if err := os.WriteFile(p, []byte(s), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	return LocalFileSystem(root), outside
}

func TestLocalFileSystemStaysInRoot(t *testing.T) {
	ctx := context.Background()
	local, outside := newLocalFS(t)

	if rc, err := local.Open(ctx, "/escape/secret"); err == nil {
		rc.Close()
		t.Error("Open followed a symlink out of the root")
	}
	if _, err := local.Stat(ctx, "/escape/secret"); err == nil {
		t.Error("Stat followed a symlink out of the root")
	}
	if _, err := local.ReadDir(ctx, "/escape", false); err == nil {
		t.Error("ReadDir followed a symlink out of the root")
	}
	if _, _, err := local.Create(ctx, "/escape/new", io.NopCloser(strings.NewReader("x")), &CreateOptions{}); err == nil {
		t.Error("Create wrote through a symlink out of the root")
	}
	if err := local.Mkdir(ctx, "/escape/newdir"); err == nil {
		t.Error("Mkdir created through a symlink out of the root")
	}
	if _, err := local.Move(ctx, "/escape/secret", "/stolen", &MoveOptions{}); err == nil {
		t.Error("Move took a file from outside the root")
	}
	if _, err := local.Copy(ctx, "/escape/secret", "/stolen", &CopyOptions{}); err == nil {
		t.Error("Copy read a file from outside the root")
	}
	if err := local.RemoveAll(ctx, "/escape/secret", &RemoveAllOptions{}); err == nil {
		t.Error("RemoveAll deleted through a symlink out of the root")
	}

	// ".." is cleaned against the root, never above it.
	if _, err := local.Stat(ctx, "/../outside/secret"); err == nil {
		t.Error("Stat escaped with ..")
	}

	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "secret" {
		t.Errorf("outside directory changed: %v", entries)
	}
}

func TestLocalFileSystemCopyMove(t *testing.T) {
	ctx := context.Background()
	local, _ := newLocalFS(t)

	// An in-root symlink aliases /dir under another path.
	alias := filepath.Join(string(local), "alias")
	if err := os.Symlink("dir", alias); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{
		{"/dir", "/dir"}, {"/dir", "/dir/"}, {"/dir", "/dir/sub/copy"},
		{"/dir", "/alias"}, {"/dir", "/alias/copy"}, {"/dir", "/alias/sub/copy"},
		{"/alias", "/dir/copy"}, {"/", "/x"},
	} {
		_, err := local.Copy(ctx, c[0], c[1], &CopyOptions{})
		if code := httpStatus(err); code != http.StatusForbidden {
			t.Errorf("Copy %v to %v: status %v, want 403", c[0], c[1], code)
		}
		_, err = local.Move(ctx, c[0], c[1], &MoveOptions{})
		if code := httpStatus(err); code != http.StatusForbidden {
			t.Errorf("Move %v to %v: status %v, want 403", c[0], c[1], code)
		}
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/dir/a.txt", "/dir/sub/b.txt"} {
		if _, err := local.Stat(ctx, p); err != nil {
			t.Fatalf("source damaged by refused copy or move: %v", err)
		}
	}
	for _, p := range []string{"/dir/copy", "/dir/sub/copy", "/x"} {
		if _, err := local.Stat(ctx, p); err == nil {
			t.Errorf("refused copy or move created %v", p)
		}
	}

	created, err := local.Copy(ctx, "/dir", "/copy", &CopyOptions{})
	if err != nil || !created {
		t.Fatalf("Copy = %v, %v", created, err)
	}
	if b, err := os.ReadFile(filepath.Join(string(local), "copy", "sub", "b.txt")); err != nil || string(b) != "b" {
		t.Fatalf("recursive copy: %q, %v", b, err)
	}

	if _, err := local.Move(ctx, "/missing", "/copy", &MoveOptions{}); httpStatus(err) != http.StatusNotFound {
		t.Fatalf("Move of a missing source: %v, want 404", err)
	}
	if _, err := local.Stat(ctx, "/copy"); err != nil {
		t.Fatalf("Move of a missing source removed the destination: %v", err)
	}

	created, err = local.Move(ctx, "/copy", "/moved", &MoveOptions{})
	if err != nil || !created {
		t.Fatalf("Move = %v, %v", created, err)
	}

	l, err := local.ReadDir(ctx, "/", true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, fi := range l {
		got = append(got, fi.Path)
	}
	if want := "/ /dir /dir/a.txt /dir/sub /dir/sub/b.txt /escape /moved /moved/a.txt /moved/sub /moved/sub/b.txt"; strings.Join(got, " ") != want {
		t.Errorf("ReadDir = %v, want %v", got, want)
	}
}

func httpStatus(err error) int {
	var herr *internal.HTTPError
	if errors.As(err, &herr) {
		return herr.Code
	}
	return 0
}
