package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

const (
	addr      = ":5000"
	dbPath    = "file:chat.db?_pragma=busy_timeout(5000)"
	bilalUser = "bilal"
	zohaibUser= "zohaib"

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
	db      *sql.DB
	mu      sync.Mutex
	clients map[string]*userConn // username -> connection
}

func main() {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil { log.Fatal(err) }
	if err := migrate(db); err != nil { log.Fatal(err) }
	if err := seedUsers(db); err != nil { log.Fatal(err) }

	s := &chatServer{
		db: db,
		clients: make(map[string]*userConn),
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
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS users(
		username TEXT PRIMARY KEY,
		password_hash BLOB NOT NULL
	);`)
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
		hash, _ := bcrypt.GenerateFromPassword([]byte(d.pass), bcrypt.DefaultCost)
		if _, err := db.Exec(`INSERT INTO users(username, password_hash) VALUES(?,?)`, d.name, hash); err != nil {
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
	writeLine(w, yellow, "Type:  login <username> <password>")
	writeLine(w, yellow, "Users: bilal, zohaib")
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
				// login success
				username = u
				s.attach(username, conn, w)
				writeLine(w, yellow, "Logged in as "+username+". Type your message and press Enter. Type /quit to exit.")
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
		if line == "" {
			writePrompt(w, username)
			continue
		}
		// send to peer
		if err := s.sendToPeer(username, line); err != nil {
			writeLine(w, yellow, "Peer is offline.")
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
	// Kick previous session if exists
	if old := s.clients[username]; old != nil {
		old.conn.Close()
	}
	s.clients[username] = &userConn{name: username, conn: conn, w: w}
}

func (s *chatServer) detach(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, username)
}

func (s *chatServer) peerOf(u string) string {
	if u == bilalUser { return zohaibUser }
	return bilalUser
}

func (s *chatServer) sendToPeer(from, text string) error {
	peer := s.peerOf(from)

	s.mu.Lock()
	dst := s.clients[peer]
	s.mu.Unlock()

	if dst == nil { return fmt.Errorf("peer offline") }

	ts := time.Now().Format("15:04:05")
	color := green
	if from == zohaibUser { color = cyan }
	writeLine(dst.w, color, fmt.Sprintf("[%s] %s: %s", ts, from, text))
	return nil
}

func (s *chatServer) systemBroadcast(exclude string, msg string) {
	s.mu.Lock()
	conns := make([]*userConn, 0, len(s.clients))
	for u, c := range s.clients {
		if u == exclude { continue }
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, uc := range conns {
		writeLine(uc.w, yellow, msg)
		writePrompt(uc.w, uc.name)
	}
}

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
