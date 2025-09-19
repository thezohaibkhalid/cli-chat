package main

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

const (
	addr       = ":5000" // TCP chat port
	dbDSN      = "file:chat.db?_pragma=busy_timeout(5000)"
	bilalUser  = "bilal"
	zohaibUser = "zohaib"

	// ANSI colors
	reset  = "\x1b[0m"
	green  = "\x1b[32m" // bilal
	cyan   = "\x1b[36m" // zohaib
	yellow = "\x1b[33m" // system
)

type userConn struct {
	name string
	conn net.Conn
	w    *bufio.Writer
}

type chatServer struct {
	db *sql.DB

	mu      sync.Mutex
	clients map[string]*userConn // username -> active connection

	// video requests: callee -> requester (who asked for callee's camera)
	videoReq map[string]string
}

func main() {
	log.SetFlags(log.LstdFlags|log.Lshortfile)

	db, err := sql.Open("sqlite", dbDSN)
	if err != nil { log.Fatal(err) }
	if err := migrate(db); err != nil { log.Fatal(err) }
	if err := seedUsers(db); err != nil { log.Fatal(err) }

	s := &chatServer{
		db:       db,
		clients:  make(map[string]*userConn),
		videoReq: make(map[string]string),
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil { log.Fatal(err) }
	log.Println("Chat server listening on", addr)

	for {
		c, err := ln.Accept()
		if err != nil { continue }
		go s.handle(c)
	}
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS users(
  username TEXT PRIMARY KEY,
  password_hash BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS messages(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  sender TEXT NOT NULL,
  recipient TEXT NOT NULL,
  text TEXT NOT NULL,
  ts DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  delivered INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_messages_recipient_delivered
  ON messages(recipient, delivered, ts);
`)
	return err
}

func seedUsers(db *sql.DB) error {
	type u struct{ name, pass string }
	defaults := []u{
		{bilalUser,  "ChangeMeBilal1!"},
		{zohaibUser, "ChangeMeZohaib1!"},
	}
	for _, d := range defaults {
		var exists int
		_ = db.QueryRow(`SELECT 1 FROM users WHERE username=?`, d.name).Scan(&exists)
		if exists == 1 { continue }
		h, _ := bcrypt.GenerateFromPassword([]byte(d.pass), bcrypt.DefaultCost)
		if _, err := db.Exec(`INSERT INTO users(username, password_hash) VALUES(?,?)`, d.name, h); err != nil {
			return err
		}
		log.Printf("Seeded user %s with default password (please change)\n", d.name)
	}
	return nil
}

func (s *chatServer) handle(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewScanner(conn)
	w := bufio.NewWriter(conn)

	writeLine(w, yellow, "Welcome to VM Chat!")
	writeLine(w, yellow, "Login with:  login <username> <password>")
	writeLine(w, yellow, "Users: bilal, zohaib")
	writeLine(w, yellow, "Commands: /quit, /history [N], /video, /acceptvideo, /declinevideo")
	write(w, yellow, ">> ")

	var username string
	for r.Scan() {
		line := strings.TrimSpace(r.Text())
		if username == "" {
			if strings.HasPrefix(line, "login ") {
				parts := strings.Fields(line)
				if len(parts) < 3 {
					writeLine(w, yellow, "Usage: login <username> <password>")
					write(w, yellow, ">> ")
					continue
				}
				u, p := parts[1], strings.Join(parts[2:], " ")
				if u != bilalUser && u != zohaibUser {
					writeLine(w, yellow, "Only bilal and zohaib are allowed.")
					write(w, yellow, ">> ")
					continue
				}
				if !s.checkPassword(u, p) {
					writeLine(w, yellow, "Invalid credentials.")
					write(w, yellow, ">> ")
					continue
				}
				username = u
				s.attach(username, conn, w)
				writeLine(w, yellow, "Logged in as "+username+". Type your message. /quit to exit.")
				s.deliverUndelivered(username)
				s.systemBroadcast(username, fmt.Sprintf("%s joined.", username))
				writePrompt(w, username)
				continue
			}
			writeLine(w, yellow, "Please login first:  login <username> <password>")
			write(w, yellow, ">> ")
			continue
		}

		// After login
		if line == "/quit" {
			break
		}

		if strings.HasPrefix(line, "/history") {
			parts := strings.Fields(line)
			n := 50
			if len(parts) == 2 { if v, err := strconv.Atoi(parts[1]); err==nil && v>0 && v<=1000 { n = v } }
			s.printHistory(w, n)
			writePrompt(w, username)
			continue
		}

		// Video commands
		switch line {
		case "/video":
			s.handleVideoRequest(username)
			writePrompt(w, username)
			continue
		case "/acceptvideo":
			s.handleVideoAccept(username)
			writePrompt(w, username)
			continue
		case "/declinevideo":
			s.handleVideoDecline(username)
			writePrompt(w, username)
			continue
		}

		// Regular message
		if err := s.sendToPeer(username, line); err != nil {
			writeLine(w, yellow, "Peer is offline (message queued).")
		}
		writePrompt(w, username)
	}

	// disconnect
	if username != "" {
		s.detach(username)
		s.systemBroadcast(username, fmt.Sprintf("%s left.", username))
	}
}

func (s *chatServer) checkPassword(username, password string) bool {
	var hash []byte
	err := s.db.QueryRow(`SELECT password_hash FROM users WHERE username=?`, username).Scan(&hash)
	if err != nil { return false }
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}

func (s *chatServer) attach(username string, conn net.Conn, w *bufio.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.clients[username]; old != nil { old.conn.Close() }
	s.clients[username] = &userConn{name: username, conn: conn, w: w}
}

func (s *chatServer) detach(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, username)
	delete(s.videoReq, username) // clear pending prompts for this user
}

func (s *chatServer) peerOf(u string) string {
	if u == bilalUser { return zohaibUser }
	return bilalUser
}

func (s *chatServer) sendToPeer(from, text string) error {
	peer := s.peerOf(from)

	// persist first
	res, err := s.db.Exec(`INSERT INTO messages(sender, recipient, text, delivered) VALUES(?,?,?,0)`, from, peer, text)
	if err != nil { return fmt.Errorf("db: %w", err) }
	id, _ := res.LastInsertId()

	// try deliver if online
	s.mu.Lock(); dst := s.clients[peer]; s.mu.Unlock()
	if dst == nil { return errors.New("peer offline") }

	ts := time.Now().Format("15:04:05")
	color := green
	if from == zohaibUser { color = cyan }
	writeLine(dst.w, color, fmt.Sprintf("[%s] %s: %s", ts, from, text))
	_, _ = s.db.Exec(`UPDATE messages SET delivered=1 WHERE id=?`, id)
	return nil
}

func (s *chatServer) deliverUndelivered(toUser string) {
	rows, err := s.db.Query(`
SELECT id, sender, text, strftime('%H:%M:%S', ts)
FROM messages WHERE recipient=? AND delivered=0 ORDER BY ts ASC`, toUser)
	if err != nil { return }
	defer rows.Close()

	s.mu.Lock(); uc := s.clients[toUser]; s.mu.Unlock()
	if uc == nil { return }

	count := 0
	var ids []int64
	for rows.Next() {
		var id int64; var sender, text, hhmmss string
		_ = rows.Scan(&id, &sender, &text, &hhmmss)
		c := green; if sender == zohaibUser { c = cyan }
		writeLine(uc.w, c, fmt.Sprintf("[missed %s] %s: %s", hhmmss, sender, text))
		ids = append(ids, id); count++
	}
	if count > 0 {
		writeLine(uc.w, yellow, fmt.Sprintf("You had %d offline message(s).", count))
		// mark delivered
		if len(ids) > 0 {
			placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
			args := make([]any, len(ids))
			for i, id := range ids { args[i] = id }
			_, _ = s.db.Exec(`UPDATE messages SET delivered=1 WHERE id IN (`+placeholders+`)`, args...)
		}
	}
}

func (s *chatServer) printHistory(w *bufio.Writer, n int) {
	rows, _ := s.db.Query(`
SELECT sender, recipient, text, strftime('%H:%M:%S', ts)
FROM messages
WHERE sender IN ('bilal','zohaib') AND recipient IN ('bilal','zohaib')
ORDER BY ts DESC LIMIT ?`, n)
	defer rows.Close()
	var stack [][4]string
	for rows.Next() {
		var sdr, rcp, txt, hh string
		_ = rows.Scan(&sdr, &rcp, &txt, &hh)
		stack = append(stack, [4]string{sdr, rcp, txt, hh})
	}
	for i := len(stack)-1; i>=0; i-- {
		sdr, _, txt, hh := stack[i][0], stack[i][1], stack[i][2], stack[i][3]
		c := green; if sdr==zohaibUser { c = cyan }
		writeLine(w, c, fmt.Sprintf("[%s] %s: %s", hh, sdr, txt))
	}
}

// ===== Video flow =====
// /video from requester → prompts callee to accept or decline. If accepted, generate sid and print URLs.

func (s *chatServer) handleVideoRequest(requester string) {
	callee := s.peerOf(requester)
	s.mu.Lock(); calleeConn := s.clients[callee]; s.mu.Unlock()
	if calleeConn == nil {
		if reqConn := s.clients[requester]; reqConn != nil {
			writeLine(reqConn.w, yellow, "Peer offline; cannot start video.")
		}
		return
	}
	// record pending request
	s.mu.Lock(); s.videoReq[callee] = requester; s.mu.Unlock()
	writeLine(calleeConn.w, yellow, fmt.Sprintf("%s requests your camera. Type /acceptvideo or /declinevideo", requester))
}

func (s *chatServer) handleVideoAccept(callee string) {
	s.mu.Lock(); requester, ok := s.videoReq[callee]; if ok { delete(s.videoReq, callee) }; s.mu.Unlock()
	if !ok { if c := s.clients[callee]; c != nil { writeLine(c.w, yellow, "No pending video request.") }; return }

	sid := generateSID()
	base := os.Getenv("VIDEO_BASE_URL")
	if base == "" { base = "http://127.0.0.1:5001" }

	senderURL := fmt.Sprintf("%s/v/send?sid=%s", base, sid) // Bilal opens this to SEND camera
	viewerURL := fmt.Sprintf("%s/v/view?sid=%s", base, sid) // Zohaib opens this to VIEW

	// In this design, the callee shares camera (as you requested). If you want requester to share instead, swap roles below.

	// Tell both sides
	if c := s.clients[callee]; c != nil {
		writeLine(c.w, yellow, "Video approved. Open this URL to share your camera:")
		writeLine(c.w, yellow, senderURL)
	}
	if r := s.clients[requester]; r != nil {
		writeLine(r.w, yellow, "Open this URL to view the camera:")
		writeLine(r.w, yellow, viewerURL)
	}
}

func (s *chatServer) handleVideoDecline(callee string) {
	s.mu.Lock(); requester, ok := s.videoReq[callee]; if ok { delete(s.videoReq, callee) }; s.mu.Unlock()
	if !ok { if c := s.clients[callee]; c != nil { writeLine(c.w, yellow, "No pending video request.") }; return }
	if r := s.clients[requester]; r != nil { writeLine(r.w, yellow, callee+" declined your video request.") }
	if c := s.clients[callee]; c != nil { writeLine(c.w, yellow, "Declined.") }
}

func generateSID() string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 12)
	rand.Seed(time.Now().UnixNano())
	for i := range b { b[i] = letters[rand.Intn(len(letters))] }
	return string(b)
}

// ===== Helpers =====

func write(w *bufio.Writer, color, s string) {
	_, _ = w.WriteString(color + s + reset)
	_ = w.Flush()
}
func writeLine(w *bufio.Writer, color, s string) {
	_, _ = w.WriteString(color + s + reset + "\r\n")
	_ = w.Flush()
}
func promptSymbol(u string) string {
	if u == bilalUser { return green + "> " + reset }
	return cyan + "> " + reset
}
func writePrompt(w *bufio.Writer, u string) {
	_, _ = w.WriteString(promptSymbol(u))
	_ = w.Flush()
}