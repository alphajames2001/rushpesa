package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// WSHub is a plain-WebSocket, one-way broadcast hub. See the note at the top
// of game.go: this deliberately does NOT speak the Engine.IO/Socket.IO
// protocol that round-debug.html's socket.io-client currently expects.
// Clients receive JSON envelopes shaped {"event": "...", "data": {...}} and
// never send anything back over this connection — all client actions
// (bet, cashout) go through the REST endpoints in game.go instead.

type WSHub struct {
	upgrader websocket.Upgrader

	mu      sync.RWMutex
	clients map[*websocket.Conn]bool
}

func NewWSHub() *WSHub {
	return &WSHub{
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			// CORS for the WS upgrade is handled here rather than via
			// go-chi/cors (which doesn't intercept the upgrade path).
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		clients: make(map[*websocket.Conn]bool),
	}
}

func (h *WSHub) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws: upgrade failed: %v", err)
		return
	}

	h.mu.Lock()
	h.clients[conn] = true
	h.mu.Unlock()

	// Drain and discard anything the client sends (pings/pongs handled by
	// gorilla internally); we only care about detecting disconnects here.
	go func() {
		defer h.removeClient(conn)
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()
}

func (h *WSHub) removeClient(conn *websocket.Conn) {
	h.mu.Lock()
	delete(h.clients, conn)
	h.mu.Unlock()
	conn.Close()
}

func (h *WSHub) Broadcast(event string, data any) {
	envelope, err := json.Marshal(map[string]any{"event": event, "data": data})
	if err != nil {
		log.Printf("ws: failed to marshal broadcast for %s: %v", event, err)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for conn := range h.clients {
		if err := conn.WriteMessage(websocket.TextMessage, envelope); err != nil {
			// Don't remove mid-broadcast (would mutate the map we're
			// iterating) — the read-loop goroutine will clean it up on
			// its next failed read.
			log.Printf("ws: write failed, client will be reaped on next read: %v", err)
		}
	}
}
