package server_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/otrans-app/otrans-fn/internal/server"
	"github.com/otrans-app/otrans-fn/internal/store"
)

const testAdminToken = "test-admin-token-long-enough"

type testClient struct {
	t     *testing.T
	base  string
	token string
}

func TestTransferLifecycle(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(tempDir, "otrans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })

	api, err := server.New(server.Config{
		AdminToken: testAdminToken, BlobDir: filepath.Join(tempDir, "blobs"), MaxUploadBytes: 1024 * 1024,
	}, dataStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(api.Handler())
	t.Cleanup(httpServer.Close)
	client := &testClient{t: t, base: httpServer.URL}

	pairCodeResponse := client.request(http.MethodPost, "/api/v1/admin/pair-codes", nil, map[string]string{
		"X-OTrans-Admin-Token": testAdminToken,
	})
	assertStatus(t, pairCodeResponse, http.StatusCreated)
	var pairCode store.PairingCode
	decodeResponse(t, pairCodeResponse, &pairCode)

	pairResponse := client.requestJSON(http.MethodPost, "/api/v1/pair", map[string]string{
		"code": pairCode.Code, "name": "Test PC", "platform": "windows",
	})
	assertStatus(t, pairResponse, http.StatusCreated)
	var paired store.PairResult
	decodeResponse(t, pairResponse, &paired)
	client.token = paired.Token

	fileContent := []byte("hello from OTrans")
	digestBytes := sha256.Sum256(fileContent)
	digest := hex.EncodeToString(digestBytes[:])
	createResponse := client.requestJSON(http.MethodPost, "/api/v1/transfers", store.CreateTransfer{
		ClientTransferID: "windows-test-1", Title: "First transfer",
		Items: []store.CreateItem{
			{Kind: "text", Text: "clipboard text", MimeType: "text/plain"},
			{Kind: "file", Name: "sample.txt", MimeType: "text/plain", Size: int64(len(fileContent)), SHA256: digest},
		},
	})
	assertStatus(t, createResponse, http.StatusCreated)
	var created struct {
		Transfer     store.Transfer `json:"transfer"`
		MissingBlobs []string       `json:"missingBlobs"`
	}
	decodeResponse(t, createResponse, &created)
	if len(created.MissingBlobs) != 1 || created.MissingBlobs[0] != digest {
		t.Fatalf("unexpected missing blobs: %#v", created.MissingBlobs)
	}
	duplicateCreateResponse := client.requestJSON(http.MethodPost, "/api/v1/transfers", store.CreateTransfer{
		ClientTransferID: "windows-test-1", Title: "First transfer",
		Items: []store.CreateItem{{Kind: "text", Text: "this retry payload is ignored", MimeType: "text/plain"}},
	})
	assertStatus(t, duplicateCreateResponse, http.StatusCreated)
	var duplicateCreated struct {
		Transfer store.Transfer `json:"transfer"`
	}
	decodeResponse(t, duplicateCreateResponse, &duplicateCreated)
	if duplicateCreated.Transfer.ID != created.Transfer.ID {
		t.Fatalf("idempotent create returned a different transfer: %s != %s", duplicateCreated.Transfer.ID, created.Transfer.ID)
	}

	putResponse := client.request(http.MethodPut, "/api/v1/blobs/"+digest, bytes.NewReader(fileContent), nil)
	assertStatus(t, putResponse, http.StatusCreated)
	putResponse.Body.Close()

	commitResponse := client.request(http.MethodPost, "/api/v1/transfers/"+created.Transfer.ID+"/commit", nil, nil)
	assertStatus(t, commitResponse, http.StatusOK)
	var committed store.Transfer
	decodeResponse(t, commitResponse, &committed)
	if committed.Status != "committed" || committed.CommittedAt == nil {
		t.Fatalf("transfer was not committed: %#v", committed)
	}
	repeatedCommitResponse := client.request(http.MethodPost, "/api/v1/transfers/"+created.Transfer.ID+"/commit", nil, nil)
	assertStatus(t, repeatedCommitResponse, http.StatusOK)
	repeatedCommitResponse.Body.Close()

	eventsResponse := client.request(http.MethodGet, "/api/v1/events?after=0", nil, nil)
	assertStatus(t, eventsResponse, http.StatusOK)
	var firstEvents struct {
		Events []store.Event `json:"events"`
	}
	decodeResponse(t, eventsResponse, &firstEvents)
	if len(firstEvents.Events) != 1 || firstEvents.Events[0].Type != "transfer.created" {
		t.Fatalf("unexpected create events: %#v", firstEvents.Events)
	}

	downloadResponse := client.request(http.MethodGet, "/api/v1/blobs/"+digest+"?name=sample.txt", nil, nil)
	assertStatus(t, downloadResponse, http.StatusOK)
	downloaded, err := io.ReadAll(downloadResponse.Body)
	downloadResponse.Body.Close()
	if err != nil || !bytes.Equal(downloaded, fileContent) {
		t.Fatalf("download mismatch: %q, %v", downloaded, err)
	}

	deleteResponse := client.request(http.MethodDelete, "/api/v1/transfers/"+created.Transfer.ID, nil, nil)
	assertStatus(t, deleteResponse, http.StatusNoContent)
	deleteResponse.Body.Close()

	deletedEventsResponse := client.request(http.MethodGet, "/api/v1/events?after=1", nil, nil)
	assertStatus(t, deletedEventsResponse, http.StatusOK)
	var deletedEvents struct {
		Events []store.Event `json:"events"`
	}
	decodeResponse(t, deletedEventsResponse, &deletedEvents)
	if len(deletedEvents.Events) != 1 || deletedEvents.Events[0].Type != "transfer.deleted" {
		t.Fatalf("unexpected delete events: %#v", deletedEvents.Events)
	}

	missingBlobResponse := client.request(http.MethodGet, "/api/v1/blobs/"+digest, nil, nil)
	assertStatus(t, missingBlobResponse, http.StatusNotFound)
	missingBlobResponse.Body.Close()

	cursorResponse := client.requestJSON(http.MethodPut, "/api/v1/devices/me/cursor", map[string]int64{"cursor": 2})
	assertStatus(t, cursorResponse, http.StatusNoContent)
	cursorResponse.Body.Close()
	meResponse := client.request(http.MethodGet, "/api/v1/devices/me", nil, nil)
	assertStatus(t, meResponse, http.StatusOK)
	var me store.Device
	decodeResponse(t, meResponse, &me)
	if me.Cursor != 2 {
		t.Fatalf("cursor was not persisted: %d", me.Cursor)
	}
}

func TestWebSocketSignalsAvailableEvents(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(tempDir, "otrans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	api, err := server.New(server.Config{
		AdminToken: testAdminToken, BlobDir: filepath.Join(tempDir, "blobs"), MaxUploadBytes: 1024,
	}, dataStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(api.Handler())
	t.Cleanup(httpServer.Close)
	client := pairTestDevice(t, httpServer.URL, "Realtime PC", "windows")

	ticketResponse := client.request(http.MethodPost, "/api/v1/ws-ticket", nil, nil)
	assertStatus(t, ticketResponse, http.StatusCreated)
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	decodeResponse(t, ticketResponse, &ticket)
	connection, _, err := websocket.DefaultDialer.Dial(
		strings.Replace(httpServer.URL, "http://", "ws://", 1)+"/api/v1/ws?ticket="+ticket.Ticket, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	var message struct {
		Type           string `json:"type"`
		LatestSequence int64  `json:"latestSequence"`
	}
	if err := connection.ReadJSON(&message); err != nil {
		t.Fatal(err)
	}
	if message.Type != "events.available" || message.LatestSequence != 0 {
		t.Fatalf("unexpected initial WebSocket message: %#v", message)
	}

	createResponse := client.requestJSON(http.MethodPost, "/api/v1/transfers", store.CreateTransfer{
		ClientTransferID: "ws-test-1",
		Items:            []store.CreateItem{{Kind: "text", Text: "live", MimeType: "text/plain"}},
	})
	assertStatus(t, createResponse, http.StatusCreated)
	var created struct {
		Transfer store.Transfer `json:"transfer"`
	}
	decodeResponse(t, createResponse, &created)
	commitResponse := client.request(http.MethodPost, "/api/v1/transfers/"+created.Transfer.ID+"/commit", nil, nil)
	assertStatus(t, commitResponse, http.StatusOK)
	commitResponse.Body.Close()
	if err := connection.ReadJSON(&message); err != nil {
		t.Fatal(err)
	}
	if message.LatestSequence != 1 {
		t.Fatalf("WebSocket did not announce the committed event: %#v", message)
	}
}

func TestAdminPage(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(tempDir, "otrans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	api, err := server.New(server.Config{
		AdminToken: testAdminToken, BlobDir: filepath.Join(tempDir, "blobs"), MaxUploadBytes: 1024,
	}, dataStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	rootRequest := httptest.NewRequest(http.MethodGet, "/", nil)
	rootResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(rootResponse, rootRequest)
	if rootResponse.Code != http.StatusTemporaryRedirect || rootResponse.Header().Get("Location") != "/admin/" {
		t.Fatalf("unexpected root redirect: %d %q", rootResponse.Code, rootResponse.Header().Get("Location"))
	}

	pageRequest := httptest.NewRequest(http.MethodGet, "/admin/", nil)
	pageResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(pageResponse, pageRequest)
	if pageResponse.Code != http.StatusOK {
		t.Fatalf("admin page returned %d", pageResponse.Code)
	}
	if !strings.Contains(pageResponse.Body.String(), "OTrans 中转站") {
		t.Fatal("admin page does not contain its title")
	}
	if pageResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("admin page must not be cached")
	}

	assetRequest := httptest.NewRequest(http.MethodGet, "/admin/admin.js", nil)
	assetResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(assetResponse, assetRequest)
	if assetResponse.Code != http.StatusOK || assetResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("admin script must not be cached: %d %q", assetResponse.Code, assetResponse.Header().Get("Cache-Control"))
	}

	clientPageRequest := httptest.NewRequest(http.MethodGet, "/web/", nil)
	clientPageResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(clientPageResponse, clientPageRequest)
	if clientPageResponse.Code != http.StatusOK || !strings.Contains(clientPageResponse.Body.String(), "OTrans 网页版") {
		t.Fatalf("web client page returned %d: %s", clientPageResponse.Code, clientPageResponse.Body.String())
	}
	if !strings.Contains(clientPageResponse.Header().Get("Content-Security-Policy"), "ws:") {
		t.Fatal("web client CSP must allow WebSocket connections")
	}
	clientAssetRequest := httptest.NewRequest(http.MethodGet, "/web/client.js", nil)
	clientAssetResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(clientAssetResponse, clientAssetRequest)
	if clientAssetResponse.Code != http.StatusOK || clientAssetResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unexpected web client script response: %d %q", clientAssetResponse.Code, clientAssetResponse.Header().Get("Cache-Control"))
	}

	unauthorizedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/admin/transfers", nil)
	unauthorizedResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(unauthorizedResponse, unauthorizedRequest)
	if unauthorizedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("admin transfer list without token returned %d", unauthorizedResponse.Code)
	}

	authorizedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/admin/transfers", nil)
	authorizedRequest.Header.Set("X-OTrans-Admin-Token", testAdminToken)
	authorizedResponse := httptest.NewRecorder()
	api.Handler().ServeHTTP(authorizedResponse, authorizedRequest)
	if authorizedResponse.Code != http.StatusOK || !strings.Contains(authorizedResponse.Body.String(), `"transfers":[]`) {
		t.Fatalf("unexpected admin transfer response: %d %s", authorizedResponse.Code, authorizedResponse.Body.String())
	}
}

func TestWebPlatformCanPair(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(tempDir, "otrans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	api, err := server.New(server.Config{
		AdminToken: testAdminToken, BlobDir: filepath.Join(tempDir, "blobs"), MaxUploadBytes: 1024,
	}, dataStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(api.Handler())
	t.Cleanup(httpServer.Close)
	client := pairTestDevice(t, httpServer.URL, "Browser", "web")
	response := client.request(http.MethodGet, "/api/v1/devices/me", nil, nil)
	assertStatus(t, response, http.StatusOK)
	var paired store.Device
	decodeResponse(t, response, &paired)
	if paired.Platform != "web" {
		t.Fatalf("unexpected platform: %q", paired.Platform)
	}
}

func TestRejectsWrongBlobDigest(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	dataStore, err := store.Open(filepath.Join(tempDir, "otrans.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dataStore.Close() })
	api, err := server.New(server.Config{
		AdminToken: testAdminToken, BlobDir: filepath.Join(tempDir, "blobs"), MaxUploadBytes: 1024,
	}, dataStore, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(api.Handler())
	t.Cleanup(httpServer.Close)
	client := pairTestDevice(t, httpServer.URL, "Harmony phone", "harmonyos")

	declared := sha256.Sum256([]byte("declared"))
	digest := hex.EncodeToString(declared[:])
	response := client.requestJSON(http.MethodPost, "/api/v1/transfers", store.CreateTransfer{
		Items: []store.CreateItem{{Kind: "file", Name: "bad.bin", Size: 6, SHA256: digest}},
	})
	assertStatus(t, response, http.StatusCreated)
	response.Body.Close()

	putResponse := client.request(http.MethodPut, "/api/v1/blobs/"+digest, bytes.NewReader([]byte("actual")), nil)
	assertStatus(t, putResponse, http.StatusBadRequest)
	putResponse.Body.Close()
}

func pairTestDevice(t *testing.T, base string, name string, platform string) *testClient {
	t.Helper()
	client := &testClient{t: t, base: base}
	response := client.request(http.MethodPost, "/api/v1/admin/pair-codes", nil, map[string]string{
		"X-OTrans-Admin-Token": testAdminToken,
	})
	assertStatus(t, response, http.StatusCreated)
	var pairCode store.PairingCode
	decodeResponse(t, response, &pairCode)
	pairResponse := client.requestJSON(http.MethodPost, "/api/v1/pair", map[string]string{
		"code": pairCode.Code, "name": name, "platform": platform,
	})
	assertStatus(t, pairResponse, http.StatusCreated)
	var result store.PairResult
	decodeResponse(t, pairResponse, &result)
	client.token = result.Token
	return client
}

func (c *testClient) requestJSON(method string, path string, value any) *http.Response {
	c.t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		c.t.Fatal(err)
	}
	return c.request(method, path, bytes.NewReader(body), map[string]string{"Content-Type": "application/json"})
}

func (c *testClient) request(method string, path string, body io.Reader, headers map[string]string) *http.Response {
	c.t.Helper()
	request, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		c.t.Fatal(err)
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		c.t.Fatal(err)
	}
	return response
}

func assertStatus(t *testing.T, response *http.Response, expected int) {
	t.Helper()
	if response.StatusCode != expected {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("expected HTTP %d, received %d: %s", expected, response.StatusCode, body)
	}
}

func decodeResponse(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}
