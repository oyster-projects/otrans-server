package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
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

func TestDeleteStaleDraftsReturnsCandidateBlobs(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "otrans.db")
	dataStore, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	ctx := context.Background()
	pairing, err := dataStore.CreatePairingCode(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	paired, err := dataStore.RedeemPairingCode(ctx, pairing.Code, "Cleanup PC", "windows")
	if err != nil {
		t.Fatal(err)
	}
	digest := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	transfer, _, err := dataStore.CreateTransfer(ctx, paired.Device.ID, CreateTransfer{
		Items: []CreateItem{{Kind: "file", Name: "stale.bin", Size: 10, SHA256: digest}},
	})
	if err != nil {
		t.Fatal(err)
	}
	digests, err := dataStore.DeleteStaleDrafts(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(digests) != 1 || digests[0] != digest {
		t.Fatalf("unexpected stale digests: %#v", digests)
	}
	if _, err := dataStore.GetTransfer(ctx, transfer.ID, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale draft still exists: %v", err)
	}
}
