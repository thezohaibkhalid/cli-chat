package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// Embed the web/ directory containing send.html & view.html
//go:embed web
var webFS embed.FS

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type endpoint struct {
	mu sync.Mutex

	// live connections (nil until attached)
	sender *websocket.Conn
	viewer *websocket.Conn

	// queued state when the counterpart isn't attached yet
	offer         *string             // last SDP offer from sender
	answer        *string             // last SDP answer from viewer
	iceFromSender []json.RawMessage   // ICE candidates to send to viewer
	iceFromViewer []json.RawMessage   // ICE candidates to send to sender
}

type server struct {
	mu       sync.Mutex
	sessions map[string]*endpoint // sid -> endpoint
}

func main() {
	s := &server{sessions: make(map[string]*endpoint)}

	// Serve embedded /v/* pages from web/
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/v/", http.StripPrefix("/v/", http.FileServer(http.FS(sub))))

	// Nice redirects without .html (optional)
	http.HandleFunc("/v/send", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v/send.html?"+r.URL.RawQuery, http.StatusFound)
	})
	http.HandleFunc("/v/view", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v/view.html?"+r.URL.RawQuery, http.StatusFound)
	})

	// WebSocket signaling
	http.HandleFunc("/ws", s.ws)

	addr := ":5001"
	log.Println("Video signaling listening on", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

type hello struct {
	Role string `json:"role"` // "sender" or "viewer"
	SID  string `json:"sid"`
}

type msg struct {
	Type string          `json:"type"`                // "offer", "answer", "ice"
	SDP  string          `json:"sdp,omitempty"`       // for offer/answer
	Cand json.RawMessage `json:"candidate,omitempty"` // for ice
}

func (s *server) ws(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// First message must be hello {role,sid}
	_, data, err := c.ReadMessage()
	if err != nil {
		_ = c.Close()
		return
	}
	var hi hello
	if err := json.Unmarshal(data, &hi); err != nil || (hi.Role != "sender" && hi.Role != "viewer") || hi.SID == "" {
		_ = c.Close()
		return
	}

	ep := s.getOrCreate(hi.SID)

	// Attach this connection
	ep.mu.Lock()
	if hi.Role == "sender" {
		if ep.sender != nil {
			_ = ep.sender.Close()
		}
		ep.sender = c
		// If viewer already sent an answer or ICE, deliver them now
		if ep.answer != nil {
			_ = ep.sender.WriteJSON(msg{Type: "answer", SDP: *ep.answer})
			ep.answer = nil
		}
		for _, cand := range ep.iceFromViewer {
			_ = ep.sender.WriteJSON(msg{Type: "ice", Cand: cand})
		}
		ep.iceFromViewer = nil
	} else { // viewer
		if ep.viewer != nil {
			_ = ep.viewer.Close()
		}
		ep.viewer = c
		// If sender already sent an offer or ICE, deliver them now
		if ep.offer != nil {
			_ = ep.viewer.WriteJSON(msg{Type: "offer", SDP: *ep.offer})
			ep.offer = nil
		}
		for _, cand := range ep.iceFromSender {
			_ = ep.viewer.WriteJSON(msg{Type: "ice", Cand: cand})
		}
		ep.iceFromSender = nil
	}
	ep.mu.Unlock()

	// Relay loop
	go func(role, sid string, conn *websocket.Conn) {
		defer func() {
			ep.mu.Lock()
			if role == "sender" && ep.sender == conn {
				ep.sender = nil
			}
			if role == "viewer" && ep.viewer == conn {
				ep.viewer = nil
			}
			ep.mu.Unlock()
			_ = conn.Close()
		}()

		for {
			var m msg
			if err := conn.ReadJSON(&m); err != nil {
				return
			}

			ep.mu.Lock()
			var dst *websocket.Conn
			if role == "sender" {
				dst = ep.viewer
			} else {
				dst = ep.sender
			}

			switch m.Type {
			case "offer":
				// only valid from sender -> viewer
				if role == "sender" {
					if dst != nil {
						_ = dst.WriteJSON(m)
					} else {
						// queue until viewer attaches
						cp := m.SDP
						ep.offer = &cp
					}
				}
			case "answer":
				// only valid from viewer -> sender
				if role == "viewer" {
					if dst != nil {
						_ = dst.WriteJSON(m)
					} else {
						// queue until sender attaches
						cp := m.SDP
						ep.answer = &cp
					}
				}
			case "ice":
				if dst != nil {
					_ = dst.WriteJSON(m)
				} else {
					// queue ICE depending on direction
					if role == "sender" {
						ep.iceFromSender = append(ep.iceFromSender, m.Cand)
					} else {
						ep.iceFromViewer = append(ep.iceFromViewer, m.Cand)
					}
				}
			default:
				// ignore
			}
			ep.mu.Unlock()
		}
	}(hi.Role, hi.SID, c)
}

func (s *server) getOrCreate(sid string) *endpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	ep := s.sessions[sid]
	if ep == nil {
		ep = &endpoint{}
		s.sessions[sid] = ep
	}
	return ep
}
