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

func TestDeleteExpiredTransfersCreatesGlobalDeletionEvent(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "otrans.db")
	dataStore, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer dataStore.Close()
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dataStore.now = func() time.Time { return base }
	pairing, err := dataStore.CreatePairingCode(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	paired, err := dataStore.RedeemPairingCode(ctx, pairing.Code, "Retention PC", "windows")
	if err != nil {
		t.Fatal(err)
	}
	digest := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	transfer, _, err := dataStore.CreateTransfer(ctx, paired.Device.ID, CreateTransfer{
		Items: []CreateItem{
			{Kind: "text", Text: "expires"},
			{Kind: "file", Name: "expires.bin", Size: 10, SHA256: digest},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := dataStore.PutBlob(ctx, Blob{SHA256: digest, Size: 10, Path: "bb/blob", CreatedAt: formatTime(base)}); err != nil {
		t.Fatal(err)
	}
	if _, err := dataStore.CommitTransfer(ctx, transfer.ID, paired.Device.ID); err != nil {
		t.Fatal(err)
	}

	dataStore.now = func() time.Time { return base.Add(31 * 24 * time.Hour) }
	deleted, digests, err := dataStore.DeleteExpiredTransfers(ctx, base.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 || len(digests) != 1 || digests[0] != digest {
		t.Fatalf("unexpected deletion result: deleted=%d digests=%v", deleted, digests)
	}
	stored, err := dataStore.GetTransfer(ctx, transfer.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DeletedAt == nil {
		t.Fatal("expired transfer was not globally deleted")
	}
	events, err := dataStore.ListEvents(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Type != "transfer.deleted" || events[1].TransferID != transfer.ID {
		t.Fatalf("unexpected events: %#v", events)
	}
}
