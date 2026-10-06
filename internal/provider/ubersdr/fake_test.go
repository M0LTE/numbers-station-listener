package ubersdr

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// pipeNet is an in-memory network for tests inside a testing/synctest
// bubble: dials hand one end of a net.Pipe to the server's listener.
type pipeNet struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newPipeNet() *pipeNet { return &pipeNet{ch: make(chan net.Conn), done: make(chan struct{})} }

func (n *pipeNet) Accept() (net.Conn, error) {
	select {
	case c := <-n.ch:
		return c, nil
	case <-n.done:
		return nil, net.ErrClosed
	}
}

func (n *pipeNet) Close() error   { n.once.Do(func() { close(n.done) }); return nil }
func (n *pipeNet) Addr() net.Addr { return pipeAddr{} }

func (n *pipeNet) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	a, b := net.Pipe()
	select {
	case n.ch <- b:
		return a, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-n.done:
		return nil, net.ErrClosed
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// scenario drives the fake UberSDR server.
type scenario struct {
	audioFrames [][]byte // binary frames sent on /ws after accept
	specConfig  []byte   // gzip config sent first on /ws/user-spectrum
	specFrames  [][]byte // then these
	// deaf: after sending, never read again and never answer a close.
	deaf bool
	// dropAudio: after sending, drop the audio TCP connection without a
	// close frame (how UberSDR kicks), and answer later /connection calls
	// for the same UUID with 410.
	dropAudio bool
	// audioText, if set, is sent as a text frame instead of audio, then the
	// connection is dropped (how a session-creation refusal arrives).
	audioText string
	// keepSending repeats the last audio frame once a second until the
	// client closes, so long tests do not trip the stall watchdog.
	keepSending bool
}

// fakeServer is a minimal UberSDR: /connection, /ws and /ws/user-spectrum.
type fakeServer struct {
	t   testing.TB
	sc  scenario
	net *pipeNet
	srv *http.Server
	tr  *http.Transport

	release chan struct{} // unblocks deaf handlers at the end

	mu            sync.Mutex
	uas           []string // User-Agent of every request, in order
	paths         []string
	ids           map[string]int // user_session_id -> /connection calls
	audioClose    websocket.StatusCode
	specClose     websocket.StatusCode
	specMessages  []string
	audioMessages []string
	handlers      sync.WaitGroup
}

func newFakeServer(t testing.TB, sc scenario) *fakeServer {
	f := &fakeServer{t: t, sc: sc, net: newPipeNet(), release: make(chan struct{}),
		ids: map[string]int{}, audioClose: -1, specClose: -1}
	mux := http.NewServeMux()
	mux.HandleFunc("/connection", f.connection)
	mux.HandleFunc("/ws", f.audio)
	mux.HandleFunc("/ws/user-spectrum", f.spectrum)
	f.srv = &http.Server{Handler: f.record(mux)}
	go f.srv.Serve(f.net)
	f.tr = &http.Transport{DialContext: f.net.dial}
	return f
}

func (f *fakeServer) client() *http.Client { return &http.Client{Transport: f.tr} }

// shutdown stops everything so the synctest bubble can exit.
func (f *fakeServer) shutdown() {
	close(f.release)
	f.handlers.Wait()
	f.srv.Close()
	f.net.Close()
	f.tr.CloseIdleConnections()
}

func (f *fakeServer) record(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.uas = append(f.uas, r.UserAgent())
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		h.ServeHTTP(w, r)
	})
}

func (f *fakeServer) connection(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID string `json:"user_session_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	f.mu.Lock()
	f.ids[b.ID]++
	calls := f.ids[b.ID]
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if f.sc.dropAudio && calls > 1 {
		w.WriteHeader(http.StatusGone)
		w.Write([]byte(`{"allowed":false,"reason":"Your session has been terminated. Please refresh the page."}`))
		return
	}
	w.Write([]byte(`{"client_ip":"192.0.2.1","allowed":true,"session_timeout":3600,"max_session_time":3600,"bypassed":false,"allowed_iq_modes":["iq48"],"daily_time_used_secs":0,"daily_time_remaining_secs":-1}`))
}

func (f *fakeServer) registered(r *http.Request) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ids[r.URL.Query().Get("user_session_id")] > 0
}

func (f *fakeServer) audio(w http.ResponseWriter, r *http.Request) {
	if !f.registered(r) {
		http.Error(w, "unregistered", http.StatusBadRequest)
		return
	}
	f.handlers.Add(1)
	defer f.handlers.Done()
	if f.sc.deaf {
		f.deafServe(w, r, nil, f.sc.audioFrames)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		f.t.Errorf("accept: %v", err)
		return
	}
	ctx := context.Background()
	if f.sc.audioText != "" {
		c.Write(ctx, websocket.MessageText, []byte(f.sc.audioText))
		c.CloseNow()
		return
	}
	for _, b := range f.sc.audioFrames {
		if err := c.Write(ctx, websocket.MessageBinary, b); err != nil {
			return
		}
	}
	if f.sc.dropAudio {
		c.CloseNow()
		return
	}
	codeCh := make(chan websocket.StatusCode, 1)
	go func() { codeCh <- readUntilClose(c, &f.mu, &f.audioMessages) }()
	var code websocket.StatusCode
	if f.sc.keepSending && len(f.sc.audioFrames) > 0 {
		last := f.sc.audioFrames[len(f.sc.audioFrames)-1]
		tk := time.NewTicker(time.Second)
	loop:
		for {
			select {
			case code = <-codeCh:
				break loop
			case <-tk.C:
				// A v4 delta frame: flags 0, delta 1 s, then the payload.
				fr := append([]byte{0x00}, binary.AppendVarint(nil, int64(time.Second))...)
				fr = append(fr, last[len(last)-50:]...)
				if err := c.Write(ctx, websocket.MessageBinary, fr); err != nil {
					code = <-codeCh
					break loop
				}
			}
		}
		tk.Stop()
	} else {
		code = <-codeCh
	}
	f.mu.Lock()
	f.audioClose = code
	f.mu.Unlock()
}

func (f *fakeServer) spectrum(w http.ResponseWriter, r *http.Request) {
	if !f.registered(r) {
		http.Error(w, "unregistered", http.StatusBadRequest)
		return
	}
	f.handlers.Add(1)
	defer f.handlers.Done()
	if f.sc.deaf {
		f.deafServe(w, r, f.sc.specConfig, f.sc.specFrames)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		f.t.Errorf("accept: %v", err)
		return
	}
	ctx := context.Background()
	if err := c.Write(ctx, websocket.MessageBinary, f.sc.specConfig); err != nil {
		return
	}
	// Read the client's zoom (if any) concurrently while sending frames, as
	// a real server would.
	codeCh := make(chan websocket.StatusCode, 1)
	go func() { codeCh <- readUntilClose(c, &f.mu, &f.specMessages) }()
	for _, b := range f.sc.specFrames {
		if err := c.Write(ctx, websocket.MessageBinary, b); err != nil {
			break
		}
	}
	code := <-codeCh
	f.mu.Lock()
	f.specClose = code
	f.mu.Unlock()
}

// readUntilClose records text messages and returns the close code the
// client sent (-1 for none).
func readUntilClose(c *websocket.Conn, mu *sync.Mutex, msgs *[]string) websocket.StatusCode {
	for {
		typ, b, err := c.Read(context.Background())
		if err != nil {
			return websocket.CloseStatus(err)
		}
		if typ == websocket.MessageText {
			mu.Lock()
			*msgs = append(*msgs, string(b))
			mu.Unlock()
		}
	}
}

// deafServe does the WebSocket handshake by hand, sends frames, then never
// reads: the client's close frame is never consumed, let alone answered.
func (f *fakeServer) deafServe(w http.ResponseWriter, r *http.Request, first []byte, frames [][]byte) {
	h := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		f.t.Errorf("hijack: %v", err)
		return
	}
	defer conn.Close()
	rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(h[:]) + "\r\n\r\n")
	if first != nil {
		writeFrame(rw.Writer, 0x2, first)
	}
	for _, b := range frames {
		writeFrame(rw.Writer, 0x2, b)
	}
	if err := rw.Flush(); err != nil {
		return
	}
	<-f.release
}

func writeFrame(w *bufio.Writer, opcode byte, p []byte) {
	w.WriteByte(0x80 | opcode)
	switch n := len(p); {
	case n < 126:
		w.WriteByte(byte(n))
	case n < 65536:
		w.WriteByte(126)
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(n))
		w.Write(b[:])
	default:
		w.WriteByte(127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		w.Write(b[:])
	}
	w.Write(p)
}
