package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestFileStorageLifecycle(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("streamed-content"), 1024)
	size, err := storage.Put(context.Background(), "opaque_key-123", bytes.NewReader(want))
	if err != nil || size != int64(len(want)) {
		t.Fatalf("Put() size=%d err=%v", size, err)
	}
	object, err := storage.Open(context.Background(), "opaque_key-123")
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(object.Body)
	closeErr := object.Body.Close()
	if readErr != nil || closeErr != nil || object.Size != int64(len(want)) || !bytes.Equal(got, want) {
		t.Fatalf("Open() size=%d readErr=%v closeErr=%v contentEqual=%v", object.Size, readErr, closeErr, bytes.Equal(got, want))
	}
	if err := storage.Delete(context.Background(), "opaque_key-123"); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.Open(context.Background(), "opaque_key-123"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open() after Delete error=%v", err)
	}
}

func TestFileStorageRejectsPathsAndCleansFailedWrites(t *testing.T) {
	storage, err := NewFileStorage(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "../escape", `dir\\escape`, "user file.txt"} {
		if _, err := storage.Put(context.Background(), key, bytes.NewReader(nil)); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Put(%q) error=%v", key, err)
		}
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := storage.Put(canceled, "safe-key", bytes.NewReader([]byte("data"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Put() error=%v", err)
	}
	if _, err := storage.Open(context.Background(), "safe-key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial blob remained: %v", err)
	}
}
