package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var (
	ErrUnknownSession   = errors.New("unknown session")
	ErrAmbiguousSession = errors.New("no session specified and a default could not be chosen")
	ErrNotLoggedIn      = errors.New("session is not logged in")
	ErrSessionExists    = errors.New("session already exists")
)

// Client is one WhatsApp connection (one phone number / linked device).
type Client struct {
	Name string
	WA   *whatsmeow.Client

	mu sync.Mutex
	qr string // latest pairing QR code; empty once logged in
}

func (c *Client) setQR(code string) {
	c.mu.Lock()
	c.qr = code
	c.mu.Unlock()
}

func (c *Client) QR() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.qr
}

// SessionInfo is the JSON view of a client's state.
type SessionInfo struct {
	Name      string `json:"name"`
	JID       string `json:"jid,omitempty"`
	Connected bool   `json:"connected"`
	LoggedIn  bool   `json:"logged_in"`
	HasQR     bool   `json:"has_qr"`
}

// Manager owns the shared whatsmeow store and every named client.
type Manager struct {
	container      *sqlstore.Container
	log            waLog.Logger
	defaultSession string
	metaPath       string

	mu      sync.RWMutex
	clients map[string]*Client // keyed by friendly name
	names   map[string]string  // jid string -> friendly name (persisted)
}

// NewManager opens the sqlite-backed store and restores every previously paired
// session (each auto-logs-in from its stored credentials).
func NewManager(ctx context.Context, storePath, metaPath, defaultSession string, log waLog.Logger) (*Manager, error) {
	dsn := fmt.Sprintf("file:%s?_foreign_keys=on", storePath)
	container, err := sqlstore.New(ctx, "sqlite3", dsn, log.Sub("store"))
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	m := &Manager{
		container:      container,
		log:            log,
		defaultSession: defaultSession,
		metaPath:       metaPath,
		clients:        map[string]*Client{},
		names:          map[string]string{},
	}
	m.loadMeta()
	if err := m.restoreAll(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// restoreAll reconnects every device already in the store.
func (m *Manager) restoreAll(ctx context.Context) error {
	devices, err := m.container.GetAllDevices(ctx)
	if err != nil {
		return fmt.Errorf("list devices: %w", err)
	}
	for _, d := range devices {
		jid := ""
		if d.ID != nil {
			jid = d.ID.String()
		}
		name := m.names[jid]
		if name == "" && d.ID != nil {
			name = d.ID.User // fall back to the phone number
		}
		if name == "" {
			continue
		}
		c := m.newClient(name, d)
		m.mu.Lock()
		m.clients[name] = c
		m.mu.Unlock()
		go func(c *Client) {
			if err := c.WA.Connect(); err != nil {
				m.log.Errorf("reconnect %s: %v", c.Name, err)
			}
		}(c)
		m.log.Infof("restored session %q (%s)", name, jid)
	}
	return nil
}

func (m *Manager) newClient(name string, device *store.Device) *Client {
	wa := whatsmeow.NewClient(device, m.log.Sub(name))
	wa.EnableAutoReconnect = true
	c := &Client{Name: name, WA: wa}
	wa.AddEventHandler(func(evt interface{}) {
		switch e := evt.(type) {
		case *events.Connected:
			m.log.Infof("session %q connected", name)
		case *events.LoggedOut:
			m.log.Warnf("session %q logged out (reason %v) — removing", name, e.Reason)
			m.forget(name)
		}
	})
	return c
}

// Pair creates a new device for `name` and drives the QR login. The QR is printed
// to the server terminal and exposed via the HTTP QR endpoints until the phone
// scans it (or it times out).
func (m *Manager) Pair(ctx context.Context, name string) (*Client, error) {
	m.mu.Lock()
	if existing, ok := m.clients[name]; ok {
		m.mu.Unlock()
		if existing.WA.IsLoggedIn() {
			return existing, ErrSessionExists
		}
		// A half-paired session lingering — drop it and start fresh.
		m.removeLocked(name)
		m.mu.Lock()
	}
	device := m.container.NewDevice()
	c := m.newClient(name, device)
	m.clients[name] = c
	m.mu.Unlock()

	qrChan, err := c.WA.GetQRChannel(context.Background())
	if err != nil {
		m.forget(name)
		return nil, fmt.Errorf("qr channel: %w", err)
	}
	if err := c.WA.Connect(); err != nil {
		m.forget(name)
		return nil, fmt.Errorf("connect: %w", err)
	}

	go func() {
		for item := range qrChan {
			switch item.Event {
			case "code":
				c.setQR(item.Code)
				fmt.Printf("\n[pair] Scan this QR with WhatsApp → Linked devices, for session %q:\n", name)
				qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stdout)
			case "success":
				c.setQR("")
				if c.WA.Store.ID != nil {
					m.bindName(name, c.WA.Store.ID.String())
				}
				m.log.Infof("session %q paired successfully", name)
			default: // timeout / error / etc.
				c.setQR("")
				m.log.Warnf("session %q pairing ended: %s", name, item.Event)
			}
		}
	}()
	return c, nil
}

// Send delivers a text message. session may be empty (uses the default / the only
// logged-in session). phone is the international number in digits (e.g. 201001234567).
func (m *Manager) Send(ctx context.Context, session, phone, message string) (string, error) {
	c, err := m.resolve(session)
	if err != nil {
		return "", err
	}
	if !c.WA.IsLoggedIn() {
		return "", ErrNotLoggedIn
	}
	jid := types.NewJID(onlyDigits(phone), types.DefaultUserServer)
	resp, err := c.WA.SendMessage(ctx, jid, &waProto.Message{
		Conversation: proto.String(message),
	})
	if err != nil {
		return "", err
	}
	return resp.ID, nil
}

// resolve picks the client to send from.
func (m *Manager) resolve(session string) (*Client, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if session != "" {
		c, ok := m.clients[session]
		if !ok {
			return nil, ErrUnknownSession
		}
		return c, nil
	}
	if m.defaultSession != "" {
		c, ok := m.clients[m.defaultSession]
		if !ok {
			return nil, ErrUnknownSession
		}
		return c, nil
	}
	// Exactly one logged-in client → unambiguous default.
	var only *Client
	count := 0
	for _, c := range m.clients {
		if c.WA.IsLoggedIn() {
			only = c
			count++
		}
	}
	if count == 1 {
		return only, nil
	}
	return nil, ErrAmbiguousSession
}

// List returns a status snapshot of every session.
func (m *Manager) List() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SessionInfo, 0, len(m.clients))
	for name, c := range m.clients {
		jid := ""
		if c.WA.Store.ID != nil {
			jid = c.WA.Store.ID.String()
		}
		out = append(out, SessionInfo{
			Name:      name,
			JID:       jid,
			Connected: c.WA.IsConnected(),
			LoggedIn:  c.WA.IsLoggedIn(),
			HasQR:     c.QR() != "",
		})
	}
	return out
}

func (m *Manager) Get(name string) (*Client, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	c, ok := m.clients[name]
	return c, ok
}

// Logout unlinks the device and forgets the session.
func (m *Manager) Logout(ctx context.Context, name string) error {
	c, ok := m.Get(name)
	if !ok {
		return ErrUnknownSession
	}
	if c.WA.IsLoggedIn() {
		if err := c.WA.Logout(ctx); err != nil {
			m.log.Warnf("logout %s: %v", name, err)
		}
	}
	m.forget(name)
	return nil
}

// forget disconnects and removes a session from memory + metadata.
func (m *Manager) forget(name string) {
	m.mu.Lock()
	m.removeLocked(name)
	m.mu.Unlock()
}

func (m *Manager) removeLocked(name string) {
	if c, ok := m.clients[name]; ok {
		c.WA.Disconnect()
		delete(m.clients, name)
	}
	for jid, n := range m.names {
		if n == name {
			delete(m.names, jid)
		}
	}
	m.saveMeta()
}

// DisconnectAll cleanly drops every connection (graceful shutdown).
func (m *Manager) DisconnectAll() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range m.clients {
		c.WA.Disconnect()
	}
}

// ── name ⇄ jid metadata (a tiny JSON sidecar) ───────────────────────────────

func (m *Manager) bindName(name, jid string) {
	m.mu.Lock()
	m.names[jid] = name
	m.saveMeta()
	m.mu.Unlock()
}

func (m *Manager) loadMeta() {
	b, err := os.ReadFile(m.metaPath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &m.names)
}

// saveMeta must be called with m.mu held (write or read is fine — single writer
// paths already hold the lock).
func (m *Manager) saveMeta() {
	b, err := json.MarshalIndent(m.names, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(m.metaPath, b, 0o600); err != nil {
		m.log.Warnf("save meta: %v", err)
	}
}

func onlyDigits(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			out = append(out, s[i])
		}
	}
	return string(out)
}
