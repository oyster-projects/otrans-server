package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestServerIDPersistsAcrossReopen(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "otrans.db")
	first, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	firstID, err := first.ServerID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondID, err := second.ServerID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if firstID == "" || firstID != secondID {
		t.Fatalf("server identity changed after reopen: %q != %q", firstID, secondID)
	}
}
