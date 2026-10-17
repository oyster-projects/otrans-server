package server

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const webSocketTicketTTL = 30 * time.Second

type webSocketTicket struct {
	deviceID string
	expires  time.Time
}

type ticketManager struct {
	mu      sync.Mutex
	tickets map[string]webSocketTicket
}

func newTicketManager() *ticketManager {
	return &ticketManager{tickets: make(map[string]webSocketTicket)}
}

func (m *ticketManager) issue(deviceID string) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().UTC().Add(webSocketTicketTTL)
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, ticket := range m.tickets {
		if !time.Now().UTC().Before(ticket.expires) {
			delete(m.tickets, key)
		}
	}
	m.tickets[token] = webSocketTicket{deviceID: deviceID, expires: expires}
	return token, expires, nil
}

func (m *ticketManager) redeem(token string) (webSocketTicket, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ticket, ok := m.tickets[token]
	delete(m.tickets, token)
	if !ok || !time.Now().UTC().Before(ticket.expires) {
		return webSocketTicket{}, false
	}
	return ticket, true
}

type webSocketClient struct {
	updates chan int64
}

type eventHub struct {
	mu      sync.Mutex
	clients map[*webSocketClient]struct{}
}

func newEventHub() *eventHub {
	return &eventHub{clients: make(map[*webSocketClient]struct{})}
}

func (h *eventHub) register(client *webSocketClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[client] = struct{}{}
}

func (h *eventHub) unregister(client *webSocketClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, client)
}

func (h *eventHub) broadcast(sequence int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for client := range h.clients {
		select {
		case client.updates <- sequence:
		default:
			select {
			case <-client.updates:
			default:
			}
			select {
			case client.updates <- sequence:
			default:
			}
		}
	}
}

func (s *Server) handleCreateWebSocketTicket(w http.ResponseWriter, r *http.Request) {
	device := deviceFromContext(r.Context())
	ticket, expires, err := s.tickets.issue(device.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{
		"ticket": ticket, "expiresAt": expires.Format(time.RFC3339Nano),
	})
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ticket, ok := s.tickets.redeem(r.URL.Query().Get("ticket"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid or expired WebSocket ticket")
		return
	}
	upgrader := websocket.Upgrader{
		CheckOrigin: func(request *http.Request) bool {
			origin := request.Header.Get("Origin")
			if origin == "" {
				return true
			}
			parsed, err := url.Parse(origin)
			return err == nil && parsed.Host == request.Host
		},
	}
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer connection.Close()

	client := &webSocketClient{updates: make(chan int64, 1)}
	s.hub.register(client)
	defer s.hub.unregister(client)
	latest, err := s.store.LatestEventSequence(r.Context())
	if err != nil {
		s.log.Error("failed to read latest event sequence", "deviceId", ticket.deviceID, "error", err)
		return
	}
	client.updates <- latest

	stopWriter := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer connection.Close()
		s.webSocketWriteLoop(connection, client, stopWriter)
	}()

	connection.SetReadLimit(8 * 1024)
	_ = connection.SetReadDeadline(time.Now().Add(75 * time.Second))
	connection.SetPongHandler(func(string) error {
		return connection.SetReadDeadline(time.Now().Add(75 * time.Second))
	})
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			break
		}
	}
	close(stopWriter)
	<-writerDone
}

func (s *Server) webSocketWriteLoop(connection *websocket.Conn, client *webSocketClient, stop <-chan struct{}) {
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case sequence := <-client.updates:
			_ = connection.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := connection.WriteJSON(map[string]any{
				"type": "events.available", "latestSequence": sequence,
			}); err != nil {
				return
			}
		case <-ping.C:
			_ = connection.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := connection.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		case <-stop:
			return
		}
	}
}

func (s *Server) broadcastLatest(ctx context.Context) {
	sequence, err := s.store.LatestEventSequence(ctx)
	if err != nil {
		s.log.Error("failed to broadcast event availability", "error", err)
		return
	}
	s.hub.broadcast(sequence)
}
