package server

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/otrans-app/otrans-fn/internal/store"
)

const (
	peerPresenceTTL  = 90 * time.Second
	peerTicketTTL    = 30 * time.Second
	maxPeerDigests   = 512
	maxPeerAddresses = 8
)

type peerPresence struct {
	deviceID  string
	name      string
	addresses []string
	digests   map[string]struct{}
	expires   time.Time
}

type peerTicket struct {
	sourceDeviceID   string
	receiverDeviceID string
	digest           string
	expires          time.Time
}

type peerSource struct {
	DeviceID   string   `json:"deviceId"`
	DeviceName string   `json:"deviceName"`
	Addresses  []string `json:"addresses"`
	Ticket     string   `json:"ticket"`
	ExpiresAt  string   `json:"expiresAt"`
}

type peerManager struct {
	mu       sync.Mutex
	presence map[string]peerPresence
	tickets  map[string]peerTicket
}

func newPeerManager() *peerManager {
	return &peerManager{
		presence: make(map[string]peerPresence),
		tickets:  make(map[string]peerTicket),
	}
}

func (m *peerManager) announce(device store.Device, addresses []string, digests []string) (time.Time, error) {
	if len(addresses) == 0 || len(addresses) > maxPeerAddresses {
		return time.Time{}, errors.New("addresses must contain 1 to 8 LAN URLs")
	}
	if len(digests) > maxPeerDigests {
		return time.Time{}, errors.New("digests must contain at most 512 entries")
	}
	normalizedAddresses := make([]string, 0, len(addresses))
	seenAddresses := make(map[string]struct{})
	for _, address := range addresses {
		normalized, err := normalizePeerAddress(address)
		if err != nil {
			return time.Time{}, err
		}
		if _, ok := seenAddresses[normalized]; ok {
			continue
		}
		seenAddresses[normalized] = struct{}{}
		normalizedAddresses = append(normalizedAddresses, normalized)
	}
	knownDigests := make(map[string]struct{}, len(digests))
	for _, value := range digests {
		digest := strings.ToLower(strings.TrimSpace(value))
		if !digestPattern.MatchString(digest) {
			return time.Time{}, errors.New("digest must be a lowercase SHA-256 value")
		}
		knownDigests[digest] = struct{}{}
	}
	expires := time.Now().UTC().Add(peerPresenceTTL)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked(time.Now().UTC())
	m.presence[device.ID] = peerPresence{
		deviceID: device.ID, name: device.Name, addresses: normalizedAddresses,
		digests: knownDigests, expires: expires,
	}
	return expires, nil
}

func (m *peerManager) sources(receiverDeviceID string, digest string) ([]peerSource, error) {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked(now)
	result := make([]peerSource, 0)
	for _, presence := range m.presence {
		if presence.deviceID == receiverDeviceID {
			continue
		}
		if _, ok := presence.digests[digest]; !ok {
			continue
		}
		token, err := newPeerTicketToken()
		if err != nil {
			return nil, err
		}
		expires := now.Add(peerTicketTTL)
		m.tickets[token] = peerTicket{
			sourceDeviceID: presence.deviceID, receiverDeviceID: receiverDeviceID,
			digest: digest, expires: expires,
		}
		result = append(result, peerSource{
			DeviceID: presence.deviceID, DeviceName: presence.name,
			Addresses: append([]string(nil), presence.addresses...), Ticket: token,
			ExpiresAt: expires.Format(time.RFC3339Nano),
		})
	}
	return result, nil
}

func (m *peerManager) redeem(sourceDeviceID string, token string, digest string) bool {
	now := time.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanupLocked(now)
	ticket, ok := m.tickets[token]
	delete(m.tickets, token)
	return ok && ticket.sourceDeviceID == sourceDeviceID && ticket.digest == digest && now.Before(ticket.expires)
}

func (m *peerManager) cleanupLocked(now time.Time) {
	for deviceID, presence := range m.presence {
		if !now.Before(presence.expires) {
			delete(m.presence, deviceID)
		}
	}
	for token, ticket := range m.tickets {
		if !now.Before(ticket.expires) {
			delete(m.tickets, token)
		}
	}
}

func normalizePeerAddress(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("peer address must be an http LAN URL")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return "", errors.New("peer address must not contain a path")
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast()) {
		return "", errors.New("peer address must use a private or link-local IP")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1024 || port > 65535 {
		return "", errors.New("peer address must include a port from 1024 to 65535")
	}
	parsed.Path = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func newPeerTicketToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *Server) handlePeerAnnounce(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Addresses []string `json:"addresses"`
		Digests   []string `json:"digests"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	expires, err := s.peers.announce(deviceFromContext(r.Context()), input.Addresses, input.Digests)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"expiresAt": expires.Format(time.RFC3339Nano)})
}

func (s *Server) handlePeerSources(w http.ResponseWriter, r *http.Request) {
	digest := strings.ToLower(strings.TrimSpace(r.PathValue("digest")))
	if !digestPattern.MatchString(digest) {
		writeError(w, http.StatusBadRequest, "digest must be a lowercase SHA-256 value")
		return
	}
	device := deviceFromContext(r.Context())
	sources, err := s.peers.sources(device.ID, digest)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}

func (s *Server) handlePeerTicketRedeem(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Ticket string `json:"ticket"`
		Digest string `json:"digest"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	input.Digest = strings.ToLower(strings.TrimSpace(input.Digest))
	device := deviceFromContext(r.Context())
	if !digestPattern.MatchString(input.Digest) || !s.peers.redeem(device.ID, input.Ticket, input.Digest) {
		writeError(w, http.StatusUnauthorized, "invalid or expired peer ticket")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
