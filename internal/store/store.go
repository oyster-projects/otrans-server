package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound       = errors.New("not found")
	ErrConflict       = errors.New("conflict")
	ErrInvalidPairing = errors.New("invalid or expired pairing code")
	ErrLimitExceeded  = errors.New("device has too many unfinished transfers")
)

const maxDraftTransfersPerDevice = 128

type Store struct {
	db  *sql.DB
	now func() time.Time
}

type Device struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Platform   string  `json:"platform"`
	SyncMode   string  `json:"syncMode"`
	Cursor     int64   `json:"cursor"`
	CreatedAt  string  `json:"createdAt"`
	LastSeenAt string  `json:"lastSeenAt"`
	RevokedAt  *string `json:"revokedAt,omitempty"`
}

type PairingCode struct {
	Code      string `json:"code"`
	ExpiresAt string `json:"expiresAt"`
}

type PairResult struct {
	Device Device `json:"device"`
	Token  string `json:"token"`
}

type CreateItem struct {
	Kind     string `json:"kind"`
	Text     string `json:"text,omitempty"`
	Name     string `json:"name,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Size     int64  `json:"size,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}

type CreateTransfer struct {
	ClientTransferID string       `json:"clientTransferId,omitempty"`
	Title            string       `json:"title,omitempty"`
	Items            []CreateItem `json:"items"`
}

type Item struct {
	ID         string `json:"id"`
	TransferID string `json:"transferId"`
	Kind       string `json:"kind"`
	Text       string `json:"text,omitempty"`
	Name       string `json:"name,omitempty"`
	MimeType   string `json:"mimeType,omitempty"`
	Size       int64  `json:"size,omitempty"`
	SHA256     string `json:"sha256,omitempty"`
}

type Transfer struct {
	ID             string  `json:"id"`
	SourceDeviceID string  `json:"sourceDeviceId"`
	Title          string  `json:"title,omitempty"`
	Status         string  `json:"status"`
	CreatedAt      string  `json:"createdAt"`
	CommittedAt    *string `json:"committedAt,omitempty"`
	DeletedAt      *string `json:"deletedAt,omitempty"`
	Items          []Item  `json:"items"`
}

type Event struct {
	Sequence   int64           `json:"sequence"`
	Type       string          `json:"type"`
	TransferID string          `json:"transferId"`
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  string          `json:"createdAt"`
}

type Blob struct {
	SHA256    string
	Size      int64
	Path      string
	CreatedAt string
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, now: time.Now}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
CREATE TABLE IF NOT EXISTS devices (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  platform TEXT NOT NULL,
  token_hash TEXT NOT NULL UNIQUE,
  sync_mode TEXT NOT NULL DEFAULT 'auto',
  cursor INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  revoked_at TEXT
);
CREATE TABLE IF NOT EXISTS pairing_codes (
  code_hash TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  used_at TEXT
);
CREATE TABLE IF NOT EXISTS transfers (
  id TEXT PRIMARY KEY,
  source_device_id TEXT NOT NULL REFERENCES devices(id),
  client_transfer_id TEXT,
  title TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL CHECK(status IN ('draft', 'committed')),
  created_at TEXT NOT NULL,
  committed_at TEXT,
  deleted_at TEXT
);
CREATE TABLE IF NOT EXISTS items (
  id TEXT PRIMARY KEY,
  transfer_id TEXT NOT NULL REFERENCES transfers(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK(kind IN ('text', 'file')),
  text_content TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  mime_type TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  sha256 TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_items_transfer ON items(transfer_id);
CREATE INDEX IF NOT EXISTS idx_items_sha256 ON items(sha256);
CREATE TABLE IF NOT EXISTS blobs (
  sha256 TEXT PRIMARY KEY,
  size INTEGER NOT NULL,
  path TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT,
  type TEXT NOT NULL,
  transfer_id TEXT NOT NULL,
  payload TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_transfer ON events(transfer_id);
CREATE TABLE IF NOT EXISTS server_metadata (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "transfers", "client_transfer_id", "TEXT"); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
CREATE UNIQUE INDEX IF NOT EXISTS idx_transfers_client_id
ON transfers(source_device_id, client_transfer_id)
WHERE client_transfer_id IS NOT NULL AND client_transfer_id != ''`)
	return err
}

func (s *Store) ensureColumn(ctx context.Context, table string, column string, definition string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name string
		var dataType string
		var notNull int
		var defaultValue sql.NullString
		var primaryKey int
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+definition)
	return err
}

func (s *Store) ServerID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM server_metadata WHERE key = 'server_id'`).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	id, err = newID()
	if err != nil {
		return "", err
	}
	if _, err = s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO server_metadata(key, value) VALUES('server_id', ?)`, id); err != nil {
		return "", err
	}
	err = s.db.QueryRowContext(ctx, `SELECT value FROM server_metadata WHERE key = 'server_id'`).Scan(&id)
	return id, err
}

func (s *Store) CreatePairingCode(ctx context.Context, ttl time.Duration) (PairingCode, error) {
	now := formatTime(s.now().UTC())
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM pairing_codes WHERE used_at IS NOT NULL OR julianday(expires_at) <= julianday(?)`, now); err != nil {
		return PairingCode{}, err
	}
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return PairingCode{}, err
	}
	code := strings.TrimRight(base32.StdEncoding.EncodeToString(raw), "=")
	createdAt := s.now().UTC()
	expiresAt := createdAt.Add(ttl)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pairing_codes(code_hash, created_at, expires_at) VALUES(?, ?, ?)`,
		hashSecret(code), formatTime(createdAt), formatTime(expiresAt),
	)
	if err != nil {
		return PairingCode{}, err
	}
	return PairingCode{Code: code, ExpiresAt: formatTime(expiresAt)}, nil
}

func (s *Store) RedeemPairingCode(ctx context.Context, code string, name string, platform string) (PairResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PairResult{}, err
	}
	defer tx.Rollback()

	var expiresAt string
	var usedAt sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT expires_at, used_at FROM pairing_codes WHERE code_hash = ?`,
		hashSecret(strings.ToUpper(strings.TrimSpace(code))),
	).Scan(&expiresAt, &usedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PairResult{}, ErrInvalidPairing
	}
	if err != nil {
		return PairResult{}, err
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || usedAt.Valid || !s.now().UTC().Before(expires) {
		return PairResult{}, ErrInvalidPairing
	}

	deviceID, err := newID()
	if err != nil {
		return PairResult{}, err
	}
	token, err := newToken()
	if err != nil {
		return PairResult{}, err
	}
	now := formatTime(s.now().UTC())
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO devices(id, name, platform, token_hash, created_at, last_seen_at) VALUES(?, ?, ?, ?, ?, ?)`,
		deviceID, name, platform, hashSecret(token), now, now,
	); err != nil {
		return PairResult{}, err
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE pairing_codes SET used_at = ? WHERE code_hash = ? AND used_at IS NULL`,
		now, hashSecret(strings.ToUpper(strings.TrimSpace(code))),
	); err != nil {
		return PairResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return PairResult{}, err
	}
	return PairResult{
		Device: Device{ID: deviceID, Name: name, Platform: platform, SyncMode: "auto", CreatedAt: now, LastSeenAt: now},
		Token:  token,
	}, nil
}

func (s *Store) Authenticate(ctx context.Context, token string) (Device, error) {
	var device Device
	var revoked sql.NullString
	err := s.db.QueryRowContext(ctx, `
SELECT id, name, platform, sync_mode, cursor, created_at, last_seen_at, revoked_at
FROM devices WHERE token_hash = ?`, hashSecret(token)).Scan(
		&device.ID, &device.Name, &device.Platform, &device.SyncMode, &device.Cursor,
		&device.CreatedAt, &device.LastSeenAt, &revoked,
	)
	if errors.Is(err, sql.ErrNoRows) || revoked.Valid {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE devices SET last_seen_at = ? WHERE id = ?`, formatTime(s.now().UTC()), device.ID)
	return device, nil
}

func (s *Store) ListDevices(ctx context.Context, includeRevoked bool) ([]Device, error) {
	query := `SELECT id, name, platform, sync_mode, cursor, created_at, last_seen_at, revoked_at FROM devices`
	if !includeRevoked {
		query += ` WHERE revoked_at IS NULL`
	}
	query += ` ORDER BY revoked_at IS NOT NULL, last_seen_at DESC`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	devices := make([]Device, 0)
	for rows.Next() {
		var device Device
		var revoked sql.NullString
		if err := rows.Scan(&device.ID, &device.Name, &device.Platform, &device.SyncMode, &device.Cursor,
			&device.CreatedAt, &device.LastSeenAt, &revoked); err != nil {
			return nil, err
		}
		if revoked.Valid {
			value := revoked.String
			device.RevokedAt = &value
		}
		devices = append(devices, device)
	}
	return devices, rows.Err()
}

func (s *Store) RevokeDevice(ctx context.Context, deviceID string) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE devices SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, formatTime(s.now().UTC()), deviceID)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CreateTransfer(ctx context.Context, sourceDeviceID string, input CreateTransfer) (Transfer, []string, error) {
	if input.ClientTransferID != "" {
		var existingID string
		err := s.db.QueryRowContext(ctx, `
SELECT id FROM transfers WHERE source_device_id = ? AND client_transfer_id = ?`,
			sourceDeviceID, input.ClientTransferID).Scan(&existingID)
		if err == nil {
			transfer, getErr := s.GetTransfer(ctx, existingID, true)
			if getErr != nil {
				return Transfer{}, nil, getErr
			}
			missing, missingErr := s.MissingBlobs(ctx, existingID)
			return transfer, missing, missingErr
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Transfer{}, nil, err
		}
	}
	var draftCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM transfers WHERE source_device_id = ? AND status = 'draft' AND deleted_at IS NULL`,
		sourceDeviceID).Scan(&draftCount); err != nil {
		return Transfer{}, nil, err
	}
	if draftCount >= maxDraftTransfersPerDevice {
		return Transfer{}, nil, ErrLimitExceeded
	}
	transferID, err := newID()
	if err != nil {
		return Transfer{}, nil, err
	}
	now := formatTime(s.now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Transfer{}, nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO transfers(id, source_device_id, client_transfer_id, title, status, created_at) VALUES(?, ?, ?, ?, 'draft', ?)`,
		transferID, sourceDeviceID, nullableString(input.ClientTransferID), input.Title, now,
	); err != nil {
		return Transfer{}, nil, err
	}
	for _, source := range input.Items {
		itemID, idErr := newID()
		if idErr != nil {
			return Transfer{}, nil, idErr
		}
		if _, err = tx.ExecContext(ctx, `
INSERT INTO items(id, transfer_id, kind, text_content, name, mime_type, size, sha256)
VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, itemID, transferID, source.Kind, source.Text, source.Name,
			source.MimeType, source.Size, strings.ToLower(source.SHA256)); err != nil {
			return Transfer{}, nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return Transfer{}, nil, err
	}
	transfer, err := s.GetTransfer(ctx, transferID, true)
	if err != nil {
		return Transfer{}, nil, err
	}
	missing, err := s.MissingBlobs(ctx, transferID)
	return transfer, missing, err
}

func (s *Store) MissingBlobs(ctx context.Context, transferID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT i.sha256
FROM items i
LEFT JOIN blobs b ON b.sha256 = i.sha256
WHERE i.transfer_id = ? AND i.kind = 'file' AND (b.sha256 IS NULL OR b.size != i.size)
ORDER BY i.sha256`, transferID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	missing := make([]string, 0)
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			return nil, err
		}
		missing = append(missing, digest)
	}
	return missing, rows.Err()
}

func (s *Store) ExpectedBlobSize(ctx context.Context, deviceID string, digest string) (int64, error) {
	var size int64
	err := s.db.QueryRowContext(ctx, `
SELECT i.size FROM items i
JOIN transfers t ON t.id = i.transfer_id
WHERE t.source_device_id = ? AND t.status = 'draft' AND i.kind = 'file' AND i.sha256 = ?
LIMIT 1`, deviceID, digest).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return size, err
}

func (s *Store) PutBlob(ctx context.Context, blob Blob) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO blobs(sha256, size, path, created_at) VALUES(?, ?, ?, ?)
ON CONFLICT(sha256) DO NOTHING`, blob.SHA256, blob.Size, blob.Path, blob.CreatedAt)
	return err
}

func (s *Store) GetBlob(ctx context.Context, digest string) (Blob, error) {
	var blob Blob
	err := s.db.QueryRowContext(ctx,
		`SELECT sha256, size, path, created_at FROM blobs WHERE sha256 = ?`, digest,
	).Scan(&blob.SHA256, &blob.Size, &blob.Path, &blob.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Blob{}, ErrNotFound
	}
	return blob, err
}

func (s *Store) TotalBlobBytes(ctx context.Context) (int64, error) {
	var total int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM blobs`).Scan(&total)
	return total, err
}

func (s *Store) CleanupExpiredPairingCodes(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM pairing_codes WHERE used_at IS NOT NULL OR julianday(expires_at) <= julianday(?)`,
		formatTime(s.now().UTC()))
	return err
}

func (s *Store) DeleteStaleDrafts(ctx context.Context, createdBefore time.Time) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
SELECT DISTINCT i.sha256 FROM items i
JOIN transfers t ON t.id = i.transfer_id
WHERE t.status = 'draft' AND julianday(t.created_at) < julianday(?) AND i.kind = 'file'`, formatTime(createdBefore.UTC()))
	if err != nil {
		return nil, err
	}
	digests := make([]string, 0)
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			_ = rows.Close()
			return nil, err
		}
		digests = append(digests, digest)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM transfers WHERE status = 'draft' AND julianday(created_at) < julianday(?)`, formatTime(createdBefore.UTC())); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return digests, nil
}

func (s *Store) CommitTransfer(ctx context.Context, transferID string, deviceID string) (Transfer, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Transfer{}, err
	}
	defer tx.Rollback()

	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM transfers WHERE id = ? AND source_device_id = ? AND deleted_at IS NULL`,
		transferID, deviceID,
	).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return Transfer{}, ErrNotFound
	}
	if err != nil {
		return Transfer{}, err
	}
	if status == "committed" {
		return getTransferTx(ctx, tx, transferID, true)
	}
	var missing int
	if err = tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM items i
LEFT JOIN blobs b ON b.sha256 = i.sha256
WHERE i.transfer_id = ? AND i.kind = 'file' AND (b.sha256 IS NULL OR b.size != i.size)`, transferID).Scan(&missing); err != nil {
		return Transfer{}, err
	}
	if missing > 0 {
		return Transfer{}, fmt.Errorf("%w: %d file blobs are missing", ErrConflict, missing)
	}
	committedAt := formatTime(s.now().UTC())
	if _, err = tx.ExecContext(ctx,
		`UPDATE transfers SET status = 'committed', committed_at = ? WHERE id = ?`,
		committedAt, transferID,
	); err != nil {
		return Transfer{}, err
	}
	transfer, err := getTransferTx(ctx, tx, transferID, true)
	if err != nil {
		return Transfer{}, err
	}
	payload, err := json.Marshal(transfer)
	if err != nil {
		return Transfer{}, err
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO events(type, transfer_id, payload, created_at) VALUES('transfer.created', ?, ?, ?)`,
		transferID, string(payload), committedAt,
	); err != nil {
		return Transfer{}, err
	}
	if err = tx.Commit(); err != nil {
		return Transfer{}, err
	}
	return transfer, nil
}

func (s *Store) GetTransfer(ctx context.Context, transferID string, includeDeleted bool) (Transfer, error) {
	return getTransferQuery(ctx, s.db, transferID, includeDeleted)
}

func (s *Store) DeviceName(ctx context.Context, deviceID string) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM devices WHERE id = ?`, deviceID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

func (s *Store) ListTransfers(ctx context.Context, includeDeleted bool, limit int) ([]Transfer, error) {
	query := `SELECT id FROM transfers WHERE status = 'committed'`
	if !includeDeleted {
		query += ` AND deleted_at IS NULL`
	}
	query += ` ORDER BY committed_at DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	transfers := make([]Transfer, 0, len(ids))
	for _, id := range ids {
		transfer, err := s.GetTransfer(ctx, id, true)
		if err != nil {
			return nil, err
		}
		transfers = append(transfers, transfer)
	}
	return transfers, nil
}

func (s *Store) DeleteTransfer(ctx context.Context, transferID string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var status string
	var deletedAt sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT status, deleted_at FROM transfers WHERE id = ?`, transferID,
	).Scan(&status, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != "committed" {
		return nil, ErrConflict
	}
	if deletedAt.Valid {
		return nil, nil
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT sha256 FROM items WHERE transfer_id = ? AND kind = 'file'`, transferID)
	if err != nil {
		return nil, err
	}
	digests := make([]string, 0)
	for rows.Next() {
		var digest string
		if scanErr := rows.Scan(&digest); scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		digests = append(digests, digest)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}

	now := formatTime(s.now().UTC())
	if _, err = tx.ExecContext(ctx, `UPDATE transfers SET deleted_at = ? WHERE id = ?`, now, transferID); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]string{"id": transferID, "deletedAt": now})
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO events(type, transfer_id, payload, created_at) VALUES('transfer.deleted', ?, ?, ?)`,
		transferID, string(payload), now,
	); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return digests, nil
}

func (s *Store) BlobReferenceCount(ctx context.Context, digest string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM items i
JOIN transfers t ON t.id = i.transfer_id
WHERE i.sha256 = ? AND t.deleted_at IS NULL`, digest).Scan(&count)
	return count, err
}

func (s *Store) DeleteBlobRecord(ctx context.Context, digest string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM blobs WHERE sha256 = ?`, digest)
	return err
}

func (s *Store) ListEvents(ctx context.Context, after int64, limit int) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT sequence, type, transfer_id, payload, created_at
FROM events WHERE sequence > ? ORDER BY sequence ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]Event, 0)
	for rows.Next() {
		var event Event
		var payload string
		if err := rows.Scan(&event.Sequence, &event.Type, &event.TransferID, &payload, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.Payload = json.RawMessage(payload)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) LatestEventSequence(ctx context.Context) (int64, error) {
	var sequence int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM events`).Scan(&sequence)
	return sequence, err
}

func (s *Store) UpdateCursor(ctx context.Context, deviceID string, cursor int64) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE devices SET cursor = MAX(cursor, ?) WHERE id = ?`, cursor, deviceID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func getTransferTx(ctx context.Context, tx *sql.Tx, transferID string, includeDeleted bool) (Transfer, error) {
	return getTransferQuery(ctx, tx, transferID, includeDeleted)
}

func getTransferQuery(ctx context.Context, q queryer, transferID string, includeDeleted bool) (Transfer, error) {
	query := `
SELECT id, source_device_id, title, status, created_at, committed_at, deleted_at
FROM transfers WHERE id = ?`
	if !includeDeleted {
		query += ` AND deleted_at IS NULL`
	}
	var transfer Transfer
	var committedAt sql.NullString
	var deletedAt sql.NullString
	err := q.QueryRowContext(ctx, query, transferID).Scan(
		&transfer.ID, &transfer.SourceDeviceID, &transfer.Title, &transfer.Status,
		&transfer.CreatedAt, &committedAt, &deletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Transfer{}, ErrNotFound
	}
	if err != nil {
		return Transfer{}, err
	}
	if committedAt.Valid {
		transfer.CommittedAt = &committedAt.String
	}
	if deletedAt.Valid {
		transfer.DeletedAt = &deletedAt.String
	}
	rows, err := q.QueryContext(ctx, `
SELECT id, transfer_id, kind, text_content, name, mime_type, size, sha256
FROM items WHERE transfer_id = ? ORDER BY rowid`, transferID)
	if err != nil {
		return Transfer{}, err
	}
	defer rows.Close()
	transfer.Items = make([]Item, 0)
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.ID, &item.TransferID, &item.Kind, &item.Text, &item.Name,
			&item.MimeType, &item.Size, &item.SHA256); err != nil {
			return Transfer{}, err
		}
		transfer.Items = append(transfer.Items, item)
	}
	return transfer, rows.Err()
}

func newID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

func newToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func hashSecret(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func formatTime(value time.Time) string {
	return value.Format(time.RFC3339Nano)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
