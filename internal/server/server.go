package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oyster-projects/otrans-server/internal/store"
)

var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var clientTransferIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
var errStorageQuotaExceeded = errors.New("storage quota exceeded")

const defaultMaxStorageBytes int64 = 100 * 1024 * 1024 * 1024

//go:embed web/*
var webAssets embed.FS

type Config struct {
	AdminToken      string
	BlobDir         string
	MaxUploadBytes  int64
	MaxStorageBytes int64
}

type Server struct {
	config          Config
	store           *store.Store
	log             *slog.Logger
	mux             *http.ServeMux
	serverID        string
	tickets         *ticketManager
	hub             *eventHub
	peers           *peerManager
	limiter         *rateLimiter
	blobRoot        *os.Root
	storageMu       sync.Mutex
	reservedStorage int64
}

type contextKey string

const deviceContextKey contextKey = "device"

type errorResponse struct {
	Error string `json:"error"`
}

type adminTransfer struct {
	store.Transfer
	SourceDeviceName string `json:"sourceDeviceName"`
}

func New(config Config, dataStore *store.Store, logger *slog.Logger) (*Server, error) {
	if len(config.AdminToken) < 16 {
		return nil, errors.New("admin token must contain at least 16 characters")
	}
	if config.MaxUploadBytes <= 0 {
		return nil, errors.New("max upload bytes must be positive")
	}
	if config.MaxStorageBytes <= 0 {
		config.MaxStorageBytes = defaultMaxStorageBytes
	}
	if config.MaxStorageBytes < config.MaxUploadBytes {
		return nil, errors.New("max storage bytes must be greater than or equal to max upload bytes")
	}
	if err := os.MkdirAll(config.BlobDir, 0o750); err != nil {
		return nil, err
	}
	blobRoot, err := os.OpenRoot(config.BlobDir)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	serverID, err := dataStore.ServerID(context.Background())
	if err != nil {
		_ = blobRoot.Close()
		return nil, fmt.Errorf("load server identity: %w", err)
	}
	server := &Server{
		config: config, store: dataStore, log: logger, mux: http.NewServeMux(),
		serverID: serverID, tickets: newTicketManager(), hub: newEventHub(), peers: newPeerManager(),
		limiter: newRateLimiter(), blobRoot: blobRoot,
	}
	server.routes()
	return server, nil
}

func (s *Server) Close() error {
	return s.blobRoot.Close()
}

func (s *Server) Handler() http.Handler {
	return s.recoverPanic(s.logRequests(securityHeaders(s.mux)))
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.handleRoot)
	s.mux.HandleFunc("GET /admin/{$}", s.handleAdminPage)
	s.mux.HandleFunc("GET /admin/{asset}", s.handleAdminAsset)
	s.mux.HandleFunc("GET /web", s.handleClientRedirect)
	s.mux.HandleFunc("GET /web/{$}", s.handleClientPage)
	s.mux.HandleFunc("GET /web/{asset}", s.handleClientAsset)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /api/v1/server-info", s.handleServerInfo)
	s.mux.HandleFunc("POST /api/v1/admin/pair-codes", s.handleCreatePairCode)
	s.mux.HandleFunc("GET /api/v1/admin/transfers", s.handleAdminTransfers)
	s.mux.HandleFunc("GET /api/v1/admin/status", s.handleAdminStatus)
	s.mux.HandleFunc("GET /api/v1/admin/devices", s.handleAdminDevices)
	s.mux.HandleFunc("DELETE /api/v1/admin/devices/{id}", s.handleAdminRevokeDevice)
	s.mux.HandleFunc("POST /api/v1/pair", s.handlePair)
	s.mux.Handle("GET /api/v1/devices/me", s.requireDevice(http.HandlerFunc(s.handleDeviceMe)))
	s.mux.Handle("PUT /api/v1/devices/me/cursor", s.requireDevice(http.HandlerFunc(s.handleUpdateCursor)))
	s.mux.Handle("POST /api/v1/transfers", s.requireDevice(http.HandlerFunc(s.handleCreateTransfer)))
	s.mux.Handle("GET /api/v1/transfers", s.requireDevice(http.HandlerFunc(s.handleListTransfers)))
	s.mux.Handle("GET /api/v1/transfers/{id}", s.requireDevice(http.HandlerFunc(s.handleGetTransfer)))
	s.mux.Handle("POST /api/v1/transfers/{id}/commit", s.requireDevice(http.HandlerFunc(s.handleCommitTransfer)))
	s.mux.Handle("DELETE /api/v1/transfers/{id}", s.requireDevice(http.HandlerFunc(s.handleDeleteTransfer)))
	s.mux.Handle("PUT /api/v1/blobs/{digest}", s.requireDevice(http.HandlerFunc(s.handlePutBlob)))
	s.mux.Handle("GET /api/v1/blobs/{digest}", s.requireDevice(http.HandlerFunc(s.handleGetBlob)))
	s.mux.Handle("GET /api/v1/events", s.requireDevice(http.HandlerFunc(s.handleEvents)))
	s.mux.Handle("POST /api/v1/ws-ticket", s.requireDevice(http.HandlerFunc(s.handleCreateWebSocketTicket)))
	s.mux.HandleFunc("GET /api/v1/ws", s.handleWebSocket)
	s.mux.Handle("POST /api/v1/lan/announce", s.requireDevice(http.HandlerFunc(s.handlePeerAnnounce)))
	s.mux.Handle("GET /api/v1/lan/sources/{digest}", s.requireDevice(http.HandlerFunc(s.handlePeerSources)))
	s.mux.Handle("POST /api/v1/lan/tickets/redeem", s.requireDevice(http.HandlerFunc(s.handlePeerTicketRedeem)))
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/admin/", http.StatusTemporaryRedirect)
}

func (s *Server) handleAdminPage(w http.ResponseWriter, _ *http.Request) {
	s.serveWebFile(w, "admin.html", "text/html; charset=utf-8", true)
}

func (s *Server) handleAdminAsset(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("asset") {
	case "admin.css":
		s.serveWebFile(w, "admin.css", "text/css; charset=utf-8", true)
	case "admin.js":
		s.serveWebFile(w, "admin.js", "text/javascript; charset=utf-8", true)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleClientRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "./web/", http.StatusTemporaryRedirect)
}

func (s *Server) handleClientPage(w http.ResponseWriter, _ *http.Request) {
	s.serveWebFile(w, "client.html", "text/html; charset=utf-8", true)
}

func (s *Server) handleClientAsset(w http.ResponseWriter, r *http.Request) {
	switch r.PathValue("asset") {
	case "client.css":
		s.serveWebFile(w, "client.css", "text/css; charset=utf-8", true)
	case "client-crypto.js":
		s.serveWebFile(w, "client-crypto.js", "text/javascript; charset=utf-8", true)
	case "client.js":
		s.serveWebFile(w, "client.js", "text/javascript; charset=utf-8", true)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveWebFile(w http.ResponseWriter, name string, contentType string, noStore bool) {
	content, err := webAssets.ReadFile("web/" + name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "web page is unavailable")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Permissions-Policy", "clipboard-read=(self), clipboard-write=(self)")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; connect-src 'self' ws: wss:; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors http: https:")
	if noStore {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(content)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleServerInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"serverId": s.serverID, "protocolVersion": 3,
	})
}

func (s *Server) handleCreatePairCode(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRateLimit(w, r, "admin", 20, time.Minute) {
		return
	}
	if !s.isAdmin(r) {
		writeError(w, http.StatusUnauthorized, "invalid admin token")
		return
	}
	pairing, err := s.store.CreatePairingCode(r.Context(), 10*time.Minute)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, pairing)
}

func (s *Server) handleAdminTransfers(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRateLimit(w, r, "admin", 60, time.Minute) {
		return
	}
	if !s.isAdmin(r) {
		writeError(w, http.StatusUnauthorized, "invalid admin token")
		return
	}
	includeDeleted := r.URL.Query().Get("includeDeleted") == "true"
	limit := parseLimit(r.URL.Query().Get("limit"), 100, 500)
	transfers, err := s.store.ListTransfers(r.Context(), includeDeleted, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	result := make([]adminTransfer, 0, len(transfers))
	for _, transfer := range transfers {
		deviceName, deviceErr := s.store.DeviceName(r.Context(), transfer.SourceDeviceID)
		if deviceErr != nil && !errors.Is(deviceErr, store.ErrNotFound) {
			s.internalError(w, r, deviceErr)
			return
		}
		result = append(result, adminTransfer{Transfer: transfer, SourceDeviceName: deviceName})
	}
	writeJSON(w, http.StatusOK, map[string]any{"transfers": result})
}

func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRateLimit(w, r, "admin", 60, time.Minute) {
		return
	}
	if !s.isAdmin(r) {
		writeError(w, http.StatusUnauthorized, "invalid admin token")
		return
	}
	used, err := s.store.TotalBlobBytes(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activeRealtimeConnections": s.hub.count(), "goroutines": runtime.NumGoroutine(),
		"storageBytes": used, "maxStorageBytes": s.config.MaxStorageBytes,
	})
}

func (s *Server) handleAdminDevices(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRateLimit(w, r, "admin", 60, time.Minute) {
		return
	}
	if !s.isAdmin(r) {
		writeError(w, http.StatusUnauthorized, "invalid admin token")
		return
	}
	devices, err := s.store.ListDevices(r.Context(), true)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

func (s *Server) handleAdminRevokeDevice(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRateLimit(w, r, "admin", 30, time.Minute) {
		return
	}
	if !s.isAdmin(r) {
		writeError(w, http.StatusUnauthorized, "invalid admin token")
		return
	}
	deviceID := strings.TrimSpace(r.PathValue("id"))
	if deviceID == "" {
		writeError(w, http.StatusBadRequest, "device id is required")
		return
	}
	if err := s.store.RevokeDevice(r.Context(), deviceID); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "device not found")
		return
	} else if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.tickets.revokeDevice(deviceID)
	s.hub.disconnect(deviceID)
	s.peers.revokeDevice(deviceID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if !s.enforceRateLimit(w, r, "pair", 20, time.Minute) {
		return
	}
	var input struct {
		Code     string `json:"code"`
		Name     string `json:"name"`
		Platform string `json:"platform"`
	}
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	input.Platform = strings.ToLower(strings.TrimSpace(input.Platform))
	if input.Name == "" || len([]rune(input.Name)) > 80 {
		writeError(w, http.StatusBadRequest, "name must contain 1 to 80 characters")
		return
	}
	if input.Platform != "harmonyos" && input.Platform != "windows" && input.Platform != "web" {
		writeError(w, http.StatusBadRequest, "platform must be harmonyos, windows, or web")
		return
	}
	result, err := s.store.RedeemPairingCode(r.Context(), input.Code, input.Name, input.Platform)
	if errors.Is(err, store.ErrInvalidPairing) {
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (s *Server) handleDeviceMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, deviceFromContext(r.Context()))
}

func (s *Server) handleUpdateCursor(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Cursor int64 `json:"cursor"`
	}
	if err := decodeJSON(w, r, &input); err != nil || input.Cursor < 0 {
		writeError(w, http.StatusBadRequest, "cursor must be a non-negative integer")
		return
	}
	device := deviceFromContext(r.Context())
	if err := s.store.UpdateCursor(r.Context(), device.ID, input.Cursor); err != nil {
		s.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCreateTransfer(w http.ResponseWriter, r *http.Request) {
	var input store.CreateTransfer
	if err := decodeJSON(w, r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateTransfer(input, s.config.MaxUploadBytes); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	device := deviceFromContext(r.Context())
	transfer, missing, err := s.store.CreateTransfer(r.Context(), device.ID, input)
	if errors.Is(err, store.ErrLimitExceeded) {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		Transfer     store.Transfer `json:"transfer"`
		MissingBlobs []string       `json:"missingBlobs"`
	}{Transfer: transfer, MissingBlobs: missing})
}

func (s *Server) handleListTransfers(w http.ResponseWriter, r *http.Request) {
	includeDeleted := r.URL.Query().Get("includeDeleted") == "true"
	limit := parseLimit(r.URL.Query().Get("limit"), 100, 500)
	transfers, err := s.store.ListTransfers(r.Context(), includeDeleted, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transfers": transfers})
}

func (s *Server) handleGetTransfer(w http.ResponseWriter, r *http.Request) {
	transfer, err := s.store.GetTransfer(r.Context(), r.PathValue("id"), false)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "transfer not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, transfer)
}

func (s *Server) handleCommitTransfer(w http.ResponseWriter, r *http.Request) {
	device := deviceFromContext(r.Context())
	transfer, err := s.store.CommitTransfer(r.Context(), r.PathValue("id"), device.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "transfer not found")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.broadcastLatest(r.Context())
	writeJSON(w, http.StatusOK, transfer)
}

func (s *Server) handleDeleteTransfer(w http.ResponseWriter, r *http.Request) {
	digests, err := s.store.DeleteTransfer(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "transfer not found")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeError(w, http.StatusConflict, "draft transfers cannot be globally deleted")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	for _, digest := range digests {
		if gcErr := s.removeBlobIfUnused(r.Context(), digest); gcErr != nil {
			s.log.Error("failed to remove unreferenced blob", "digest", digest, "error", gcErr)
		}
	}
	s.broadcastLatest(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePutBlob(w http.ResponseWriter, r *http.Request) {
	digest := strings.ToLower(r.PathValue("digest"))
	if !digestPattern.MatchString(digest) {
		writeError(w, http.StatusBadRequest, "digest must be a lowercase SHA-256 value")
		return
	}
	device := deviceFromContext(r.Context())
	expectedSize, err := s.store.ExpectedBlobSize(r.Context(), device.ID, digest)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no draft transfer expects this blob")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if expectedSize > s.config.MaxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "blob exceeds server upload limit")
		return
	}
	if existing, getErr := s.store.GetBlob(r.Context(), digest); getErr == nil {
		if existing.Size != expectedSize {
			writeError(w, http.StatusConflict, "stored blob size does not match draft metadata")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	} else if !errors.Is(getErr, store.ErrNotFound) {
		s.internalError(w, r, getErr)
		return
	}
	releaseStorage, err := s.reserveStorage(r.Context(), expectedSize)
	if errors.Is(err, errStorageQuotaExceeded) {
		writeError(w, http.StatusInsufficientStorage, "server storage quota exceeded")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer releaseStorage()
	setReadDeadline(w, transferDeadline(expectedSize))

	temp, err := os.CreateTemp(s.config.BlobDir, "upload-*")
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	tempPath := temp.Name()
	tempName := filepath.Base(tempPath)
	defer func() { _ = s.blobRoot.Remove(tempName) }()
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temp, hasher), io.LimitReader(r.Body, s.config.MaxUploadBytes+1))
	closeErr := temp.Close()
	if copyErr != nil || closeErr != nil {
		s.internalError(w, r, errors.Join(copyErr, closeErr))
		return
	}
	if written > s.config.MaxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "blob exceeds server upload limit")
		return
	}
	if written != expectedSize {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("blob size mismatch: expected %d bytes", expectedSize))
		return
	}
	actualDigest := hex.EncodeToString(hasher.Sum(nil))
	if actualDigest != digest {
		writeError(w, http.StatusBadRequest, "blob SHA-256 mismatch")
		return
	}

	relativePath, err := contentAddressPath(digest)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.blobRoot.MkdirAll(filepath.Dir(relativePath), 0o750); err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.blobRoot.Rename(tempName, relativePath); err != nil {
		s.internalError(w, r, err)
		return
	}
	if err := s.store.PutBlob(r.Context(), store.Blob{
		SHA256: digest, Size: written, Path: relativePath, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		_ = s.blobRoot.Remove(relativePath)
		s.internalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleGetBlob(w http.ResponseWriter, r *http.Request) {
	digest := strings.ToLower(r.PathValue("digest"))
	if !digestPattern.MatchString(digest) {
		writeError(w, http.StatusBadRequest, "invalid digest")
		return
	}
	blob, err := s.store.GetBlob(r.Context(), digest)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "blob not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	file, err := s.blobRoot.Open(blob.Path)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "blob file not found")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer file.Close()
	setWriteDeadline(w, transferDeadline(blob.Size))
	downloadName := filepath.Base(strings.TrimSpace(r.URL.Query().Get("name")))
	if !safeFileName(downloadName) {
		downloadName = "download"
	}
	contentType := mime.TypeByExtension(filepath.Ext(downloadName))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": downloadName}))
	w.Header().Set("ETag", `"`+digest+`"`)
	http.ServeContent(w, r, digest, time.Time{}, file)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if after < 0 {
		after = 0
	}
	limit := parseLimit(r.URL.Query().Get("limit"), 100, 500)
	events, err := s.store.ListEvents(r.Context(), after, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) requireDevice(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.enforceRateLimit(w, r, "device-auth", 1200, time.Minute) {
			return
		}
		header := strings.TrimSpace(r.Header.Get("Authorization"))
		if !strings.HasPrefix(header, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		device, err := s.store.Authenticate(r.Context(), strings.TrimSpace(strings.TrimPrefix(header, "Bearer ")))
		if errors.Is(err, store.ErrNotFound) {
			if !s.enforceRateLimit(w, r, "invalid-device-auth", 60, time.Minute) {
				return
			}
			writeError(w, http.StatusUnauthorized, "invalid device token")
			return
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		allowed, retryAfter := s.limiter.allow("device|"+device.ID, 600, time.Minute)
		if !allowed {
			seconds := int(retryAfter.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			writeError(w, http.StatusTooManyRequests, "device request rate exceeded")
			return
		}
		ctx := context.WithValue(r.Context(), deviceContextKey, device)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) isAdmin(r *http.Request) bool {
	valid := constantTimeEqual(r.Header.Get("X-OTrans-Admin-Token"), s.config.AdminToken)
	if !valid {
		s.log.Warn("admin authentication failed", "remote", clientAddress(r), "path", r.URL.Path)
	}
	return valid
}

func (s *Server) removeBlobIfUnused(ctx context.Context, digest string) error {
	count, err := s.store.BlobReferenceCount(ctx, digest)
	if err != nil || count > 0 {
		return err
	}
	blob, err := s.store.GetBlob(ctx, digest)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.blobRoot.Remove(blob.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.store.DeleteBlobRecord(ctx, digest)
}

func (s *Server) reserveStorage(ctx context.Context, size int64) (func(), error) {
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	used, err := s.store.TotalBlobBytes(ctx)
	if err != nil {
		return nil, err
	}
	if size > s.config.MaxStorageBytes-used-s.reservedStorage {
		return nil, errStorageQuotaExceeded
	}
	s.reservedStorage += size
	return func() {
		s.storageMu.Lock()
		s.reservedStorage -= size
		s.storageMu.Unlock()
	}, nil
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "remote", clientAddress(r), "duration", time.Since(started))
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.log.Error("panic recovered", "panic", recovered, "method", r.Method, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func deviceFromContext(ctx context.Context) store.Device {
	device, _ := ctx.Value(deviceContextKey).(store.Device)
	return device
}

func validateTransfer(input store.CreateTransfer, maxUploadBytes int64) error {
	if input.ClientTransferID != "" && !clientTransferIDPattern.MatchString(input.ClientTransferID) {
		return errors.New("clientTransferId must contain 1 to 128 letters, digits, dots, underscores, colons, or hyphens")
	}
	if len([]rune(input.Title)) > 200 {
		return errors.New("title cannot exceed 200 characters")
	}
	if len(input.Items) == 0 || len(input.Items) > 64 {
		return errors.New("a transfer must contain between 1 and 64 items")
	}
	digestSizes := make(map[string]int64)
	for index, item := range input.Items {
		switch item.Kind {
		case "text":
			if item.Text == "" {
				return fmt.Errorf("items[%d].text cannot be empty", index)
			}
			if len([]byte(item.Text)) > 1024*1024 {
				return fmt.Errorf("items[%d].text exceeds 1 MiB", index)
			}
		case "file":
			if !safeFileName(item.Name) {
				return fmt.Errorf("items[%d].name is not a safe file name", index)
			}
			if item.Size < 0 || item.Size > maxUploadBytes {
				return fmt.Errorf("items[%d].size exceeds server limits", index)
			}
			if !digestPattern.MatchString(strings.ToLower(item.SHA256)) {
				return fmt.Errorf("items[%d].sha256 is invalid", index)
			}
			digest := strings.ToLower(item.SHA256)
			if previousSize, exists := digestSizes[digest]; exists && previousSize != item.Size {
				return fmt.Errorf("items[%d] declares a conflicting size for the same SHA-256", index)
			}
			digestSizes[digest] = item.Size
		default:
			return fmt.Errorf("items[%d].kind must be text or file", index)
		}
	}
	return nil
}

func safeFileName(name string) bool {
	if name == "" || name != strings.TrimSpace(name) || len([]rune(name)) > 255 ||
		filepath.Base(name) != name || strings.ContainsAny(name, `\/:*?"<>|`+"\x00") ||
		name == "." || name == ".." || strings.HasSuffix(name, ".") {
		return false
	}
	stem := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	reserved := map[string]struct{}{
		"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
		"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {}, "COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
		"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {}, "LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
	}
	_, isReserved := reserved[stem]
	return !isReserved
}

func contentAddressPath(digest string) (string, error) {
	if !digestPattern.MatchString(digest) {
		return "", errors.New("invalid blob digest")
	}
	return filepath.Join(digest[0:2], digest[2:4], digest), nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	setReadDeadline(w, 20*time.Second)
	r.Body = http.MaxBytesReader(w, r.Body, 2*1024*1024)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("request body must contain one JSON value")
	}
	return nil
}

func parseLimit(raw string, fallback int, maximum int) int {
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	if value > maximum {
		return maximum
	}
	return value
}

func constantTimeEqual(left string, right string) bool {
	leftHash := sha256.Sum256([]byte(left))
	rightHash := sha256.Sum256([]byte(right))
	return subtle.ConstantTimeCompare(leftHash[:], rightHash[:]) == 1
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
