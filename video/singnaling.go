package main


type msg struct {
Type string `json:"type"` // offer, answer, ice
SDP string `json:"sdp,omitempty"`
Cand json.RawMessage `json:"candidate,omitempty"`
}


func (s *server) ws(w http.ResponseWriter, r *http.Request) {
c, err := upgrader.Upgrade(w, r, nil)
if err != nil { return }


// first message must be hello {role,sid}
_, data, err := c.ReadMessage()
if err != nil { c.Close(); return }


var hi hello
if err := json.Unmarshal(data, &hi); err != nil || (hi.Role != "sender" && hi.Role != "viewer") || hi.SID == "" {
c.Close(); return
}


ep := s.getOrCreate(hi.SID)


// attach
ep.mu.Lock()
if hi.Role == "sender" {
if ep.sender != nil { ep.sender.Close() }
ep.sender = c
} else {
if ep.viewer != nil { ep.viewer.Close() }
ep.viewer = c
}
ep.mu.Unlock()


go func(role, sid string, conn *websocket.Conn) {
defer func(){
ep.mu.Lock()
if role=="sender" && ep.sender==conn { ep.sender=nil }
if role=="viewer" && ep.viewer==conn { ep.viewer=nil }
ep.mu.Unlock()
conn.Close()
}()
for {
_, b, err := conn.ReadMessage()
if err != nil { return }
var m msg
if json.Unmarshal(b, &m) != nil { continue }


ep.mu.Lock()
var dst *websocket.Conn
if role=="sender" { dst = ep.viewer } else { dst = ep.sender }
ep.mu.Unlock()


if dst != nil {
dst.WriteMessage(websocket.TextMessage, b)
}
}
}(hi.Role, hi.SID, c)
}


func (s *server) getOrCreate(sid string) *endpoint {
s.mu.Lock(); defer s.mu.Unlock()
ep := s.sessions[sid]
if ep == nil { ep = &endpoint{}; s.sessions[sid] = ep }
return ep
}


func init() { rand.Seed(time.Now().UnixNano()) }