package vault

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	v, err := Create(dir, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Put("chats/abc", []byte("hello secret")); err != nil {
		t.Fatal(err)
	}
	if err := v.Put("docs/x", []byte("doc")); err != nil {
		t.Fatal(err)
	}

	// Plaintext must not appear anywhere on disk, including file names.
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if bytes.Contains(b, []byte("hello secret")) || bytes.Contains([]byte(p), []byte("chats")) {
			t.Errorf("plaintext leaked in %s", p)
		}
		return nil
	})

	v2, err := Open(dir, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	got, err := v2.Get("chats/abc")
	if err != nil || string(got) != "hello secret" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if names := v2.List("chats/"); len(names) != 1 || names[0] != "chats/abc" {
		t.Fatalf("List = %v", names)
	}
	if err := v2.Delete("chats/abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := v2.Get("chats/abc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestWrongPassphrase(t *testing.T) {
	dir := t.TempDir()
	if _, err := Create(dir, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "wrong horse battery"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("got %v, want ErrWrongPassphrase", err)
	}
}

func TestTamperDetected(t *testing.T) {
	dir := t.TempDir()
	v, _ := Create(dir, "correct horse battery")
	v.Put("a", []byte("one"))
	v.Put("b", []byte("two"))
	// Swapping two ciphertext files must fail authentication.
	pa, pb := v.objectPath("a"), v.objectPath("b")
	ba, _ := os.ReadFile(pa)
	os.WriteFile(pb, ba, 0o600)
	if _, err := v.Get("b"); err == nil {
		t.Fatal("swapped object decrypted without error")
	}
}
