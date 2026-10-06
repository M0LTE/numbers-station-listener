package ubersdr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/m0lte/numbers-station-listener/internal/model"
	"github.com/m0lte/numbers-station-listener/internal/provider"
)

const (
	// closeBound is the most Close waits.
	closeBound = 2 * time.Second
	// gracefulClose is how long the close handshakes get before the raw
	// connections are closed under them; less than closeBound so Close can
	// still report a clean finish.
	gracefulClose = 1500 * time.Millisecond

	// keepaliveEvery is the JSON ping interval on both sockets. UberSDR's
	// inactivity timeout (session_timeout) counts only JSON messages, per
	// socket; a session exists only while the relay has listeners, so
	// pinging is legitimate activity.
	keepaliveEvery = 30 * time.Second
	minKeepalive   = 5 * time.Second
	// checkEvery is how often the session checks its keepalive and stall
	// timers.
	checkEvery = 5 * time.Second
	// stallTimeout ends a session whose audio has stopped. The server sends
	// 50 frames a second, silence included, so 20 s of nothing is a dead
	// connection.
	stallTimeout = 20 * time.Second
	// sessionTimeSlack treats an unexpected end this close to the receiver's
	// max_session_time as the time limit.
	sessionTimeSlack = 15 * time.Second

	audioBuffer = 128
	specBuffer  = 32

	// cwHalfWidthHz widens the CW filter from the server's +/-200 Hz.
	// Priyom lists whole kHz and transmitters can be a few hundred Hz off;
	// beyond about -500 Hz the receiver's +500 Hz CW tone offset would fold
	// signals below the dial (docs/ubersdr-protocol.md 5.2).
	cwHalfWidthHz = 450

	defaultSpanHz  = 12000
	assumedBinsMax = 1024
)

// binLadder is the server's zoom ladder in Hz per bin.
var binLadder = []float64{0.5, 1, 2, 5, 10, 20, 50, 100, 200, 300, 500, 1000, 2000, 5000}

// binWidthFor returns the narrowest ladder bin width whose span covers
// spanHz with bins bins.
func binWidthFor(spanHz, bins int) float64 {
	if spanHz <= 0 {
		spanHz = defaultSpanHz
	}
	if bins <= 0 {
		bins = assumedBinsMax
	}
	for _, w := range binLadder {
		if w*float64(bins) >= float64(spanHz) {
			return w
		}
	}
	return binLadder[len(binLadder)-1]
}

// connResponse is the /connection reply (docs/ubersdr-protocol.md 2).
type connResponse struct {
	Allowed        bool   `json:"allowed"`
	Reason         string `json:"reason"`
	SessionTimeout int    `json:"session_timeout"`
	MaxSessionTime int    `json:"max_session_time"`
}

// register POSTs /connection for id and maps any refusal.
func (p *Provider) register(ctx context.Context, client *http.Client, base *url.URL, id string) (connResponse, error) {
	cr, status, err := p.postConnection(ctx, client, base, id)
	if err != nil {
		return cr, err
	}
	if err := connectionError(status, cr); err != nil {
		return cr, err
	}
	return cr, nil
}

func (p *Provider) postConnection(ctx context.Context, client *http.Client, base *url.URL, id string) (connResponse, int, error) {
	body, _ := json.Marshal(map[string]string{"user_session_id": id})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base.String()+"/connection", strings.NewReader(string(body)))
	if err != nil {
		return connResponse{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", p.ua)
	resp, err := client.Do(req)
	if err != nil {
		return connResponse{}, 0, fmt.Errorf("ubersdr /connection: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var cr connResponse
	if json.Unmarshal(raw, &cr) != nil {
		cr = connResponse{} // ban pages are HTML; the status says enough
	}
	return cr, resp.StatusCode, nil
}

// connectionError maps a /connection answer to nil (allowed), a
// RejectedError (try another receiver) or a plain error (our fault).
func connectionError(status int, cr connResponse) error {
	reason := strings.TrimSpace(cr.Reason)
	if reason == "" {
		reason = "HTTP " + strconv.Itoa(status)
	}
	switch {
	case status == http.StatusOK && cr.Allowed:
		return nil
	case status == http.StatusOK:
		return &provider.RejectedError{Reason: reason}
	case status == http.StatusTooManyRequests:
		retry := 10 * time.Second
		if strings.Contains(strings.ToLower(reason), "daily") {
			retry = time.Hour
		}
		return &provider.RejectedError{Reason: reason, RetryAfter: retry}
	case status == http.StatusForbidden, status == http.StatusGone,
		status == http.StatusServiceUnavailable, status == http.StatusUnauthorized:
		return &provider.RejectedError{Reason: reason}
	default:
		return fmt.Errorf("ubersdr /connection: HTTP %d: %s", status, reason)
	}
}

// dialWS opens a WebSocket with our User-Agent and maps handshake refusals.
func (p *Provider) dialWS(ctx context.Context, client *http.Client, u string) (*websocket.Conn, error) {
	c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"User-Agent": []string{p.ua}},
	})
	if err != nil {
		if resp == nil {
			return nil, fmt.Errorf("ubersdr websocket: %w", err)
		}
		var text string
		if resp.Body != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			text = strings.TrimSpace(string(b))
		}
		reason := fmt.Sprintf("websocket refused, HTTP %d", resp.StatusCode)
		if text != "" {
			reason += ": " + text
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, &provider.RejectedError{Reason: reason}
		case http.StatusTooManyRequests:
			return nil, &provider.RejectedError{Reason: reason, RetryAfter: 2 * time.Second}
		}
		return nil, errors.New("ubersdr " + reason)
	}
	c.SetReadLimit(1 << 20)
	return c, nil
}

func wsURL(base *url.URL, path string, q url.Values) string {
	u := *base
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = path
	u.RawQuery = q.Encode()
	return u.String()
}

// serverMessage is a JSON control message on either socket.
type serverMessage struct {
	Type   string `json:"type"`
	Error  string `json:"error"`
	Status int    `json:"status"`
}

// refusal turns a server error message received while starting into a
// RejectedError with a sensible retry hint.
func refusal(m serverMessage) *provider.RejectedError {
	r := &provider.RejectedError{Reason: m.Error}
	l := strings.ToLower(m.Error)
	switch {
	case strings.Contains(l, "too many session attempts"):
		r.RetryAfter = 30 * time.Second
	case strings.Contains(l, "rate limit"):
		r.RetryAfter = 10 * time.Second
	case strings.Contains(l, "daily"):
		r.RetryAfter = time.Hour
	}
	return r
}

// Open registers a fresh UUID, then opens the audio socket and, on the same
// UUID, the spectrum socket. It returns once audio is flowing. A spectrum
// socket that fails to start is logged and the session runs audio-only.
func (p *Provider) Open(ctx context.Context, r model.Receiver, req provider.OpenRequest) (provider.Session, error) {
	base, err := p.guard(r)
	if err != nil {
		return nil, err
	}
	mode, err := model.ParseMode(string(req.Mode))
	if err != nil {
		return nil, err
	}
	if req.FreqHz <= 0 || !r.Covers(req.FreqHz) {
		return nil, &provider.RejectedError{Reason: fmt.Sprintf("%d Hz is outside this receiver's tuning range", req.FreqHz)}
	}

	client, tr := p.sessionClient()
	id := newUUID()
	cr, err := p.register(ctx, client, base, id)
	if err != nil {
		client.CloseIdleConnections()
		return nil, err
	}

	aq := url.Values{}
	aq.Set("frequency", strconv.FormatInt(req.FreqHz, 10))
	aq.Set("mode", string(mode))
	if mode == model.ModeCWU || mode == model.ModeCWL {
		aq.Set("bandwidthLow", strconv.Itoa(-cwHalfWidthHz))
		aq.Set("bandwidthHigh", strconv.Itoa(cwHalfWidthHz))
	}
	aq.Set("user_session_id", id)
	aq.Set("format", "opus")
	aq.Set("version", "4")
	ac, err := p.dialWS(ctx, client, wsURL(base, "/ws", aq))
	if err != nil {
		tr.closeAll()
		client.CloseIdleConnections()
		return nil, err
	}
	dec := newOpusDecoder(framingUnknown)
	first, err := readFirstAudio(ctx, ac, dec)
	if err != nil {
		closeConns(tr, gracefulClose, ac)
		client.CloseIdleConnections()
		return nil, err
	}

	span := req.SpanHz
	if span <= 0 {
		span = defaultSpanHz
	}
	sq := url.Values{}
	sq.Set("user_session_id", id)
	sq.Set("mode", "binary8")
	sq.Set("version", "2")
	sq.Set("frequency", strconv.FormatInt(req.FreqHz, 10))
	sq.Set("bin_bandwidth", strconv.FormatFloat(binWidthFor(span, assumedBinsMax), 'f', -1, 64))
	spec := &specDecoder{}
	sc, err := p.dialWS(ctx, client, wsURL(base, "/ws/user-spectrum", sq))
	if err == nil {
		err = startSpectrum(ctx, sc, spec, req.FreqHz, span)
		if err != nil {
			closeConns(tr, gracefulClose, sc)
			sc = nil
		}
	}
	if err != nil {
		p.log.Warn("ubersdr spectrum unavailable, continuing with audio only",
			"receiver", r.Key(), "err", err)
	}

	s := &session{
		log: p.log.With("receiver", r.Key(), "freqHz", req.FreqHz, "mode", mode),
		p:   p, base: base, client: client, tr: tr, id: id,
		ac: ac, sc: sc,
		audio:    make(chan model.AudioPacket, audioBuffer),
		spec:     make(chan model.SpectrumRow, specBuffer),
		done:     make(chan struct{}),
		sockDone: make(chan struct{}),
		stop:     make(chan struct{}),
		opened:   time.Now(),
		maxDur:   time.Duration(cr.MaxSessionTime) * time.Second,
		ping:     keepaliveEvery,
	}
	if cr.SessionTimeout > 0 {
		if d := time.Duration(cr.SessionTimeout) * time.Second / 3; d < s.ping {
			s.ping = max(d, minKeepalive)
		}
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.start(first, dec, spec)
	return s, nil
}

// readFirstAudio waits for the first audio frame, turning a server error
// message into a refusal.
func readFirstAudio(ctx context.Context, c *websocket.Conn, dec *opusDecoder) (decoded, error) {
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			return decoded{}, fmt.Errorf("ubersdr: audio socket ended before audio started: %w", err)
		}
		if typ == websocket.MessageText {
			var m serverMessage
			if json.Unmarshal(b, &m) == nil && m.Type == "error" {
				return decoded{}, refusal(m)
			}
			continue
		}
		d, err := dec.decode(b)
		if err != nil {
			return decoded{}, err
		}
		d.pkt.Opus = append([]byte(nil), d.pkt.Opus...)
		return d, nil
	}
}

// startSpectrum waits for the first config message and, if the server's
// bin count means our span does not fit (or is far too wide), asks for a
// better bin width. The decoder drops rows until the full frame after that.
func startSpectrum(ctx context.Context, c *websocket.Conn, dec *specDecoder, freqHz int64, span int) error {
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			return fmt.Errorf("spectrum socket ended before it started: %w", err)
		}
		if typ != websocket.MessageBinary || !isGzip(b) {
			continue // a SPEC frame before config cannot be placed; skip it
		}
		m, err := decodeControl(b)
		if err != nil {
			return fmt.Errorf("spectrum control message: %w", err)
		}
		switch m.Type {
		case "error":
			return refusal(serverMessage{Type: m.Type, Error: m.Error, Status: m.Status})
		case "config":
			dec.config(m)
			n := m.BinCount
			if n <= 0 {
				return nil
			}
			want := binWidthFor(span, n)
			if want != m.BinBandwidth {
				msg, _ := json.Marshal(map[string]any{"type": "zoom", "frequency": freqHz, "binBandwidth": want})
				wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := c.Write(wctx, websocket.MessageText, msg)
				cancel()
				if err != nil {
					return fmt.Errorf("spectrum zoom: %w", err)
				}
			}
			return nil
		}
	}
}

// session is one upstream channel: an audio socket and optionally a
// spectrum socket on the same UUID.
type session struct {
	log    *slog.Logger
	p      *Provider
	base   *url.URL
	client *http.Client
	tr     *connTracker
	id     string

	ac, sc *websocket.Conn

	audio    chan model.AudioPacket
	spec     chan model.SpectrumRow
	done     chan struct{} // closed last, after Err is final
	sockDone chan struct{} // closed once both sockets are closed and readers gone
	stop     chan struct{} // closed when the session starts ending

	ctx    context.Context // read loops; cancelled only once sockets are closed
	cancel context.CancelFunc
	wg     sync.WaitGroup

	opened    time.Time
	maxDur    time.Duration
	ping      time.Duration
	lastAudio atomic.Int64 // unix ns

	endOnce sync.Once
	mu      sync.Mutex
	err     error
	closing bool
}

func (s *session) Audio() <-chan model.AudioPacket    { return s.audio }
func (s *session) Spectrum() <-chan model.SpectrumRow { return s.spec }
func (s *session) Done() <-chan struct{}              { return s.done }
func (s *session) MaxDuration() time.Duration         { return s.maxDur }

func (s *session) Err() error {
	select {
	case <-s.done:
	default:
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Close closes both sockets with close frames and returns once they are
// closed, or after closeBound regardless.
func (s *session) Close() error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.end(nil, false)
	t := time.NewTimer(closeBound)
	defer t.Stop()
	select {
	case <-s.sockDone:
		return nil
	case <-t.C:
		return errors.New("ubersdr: close handshake did not finish within 2s")
	}
}

func (s *session) start(first decoded, dec *opusDecoder, spec *specDecoder) {
	s.lastAudio.Store(time.Now().UnixNano())
	s.audio <- first.pkt
	// Count every goroutine before starting any: the audio loop can end the
	// session at once, and shutdown's Wait must not run before an Add, or
	// a late spectrum loop could send on channels shutdown has closed.
	n := 2
	if s.sc != nil {
		n++
	}
	s.wg.Add(n)
	go s.audioLoop(dec)
	go s.keepalive()
	if s.sc != nil {
		go s.specLoop(spec)
	}
}

// end starts the shutdown once. cause is why (nil for Close); lost marks a
// connection that dropped rather than a protocol error of ours.
func (s *session) end(cause error, lost bool) {
	s.endOnce.Do(func() {
		close(s.stop)
		go s.shutdown(cause, lost)
	})
}

func (s *session) shutdown(cause error, lost bool) {
	closeConns(s.tr, gracefulClose, s.ac, s.sc)
	s.cancel()
	s.wg.Wait()
	close(s.audio)
	close(s.spec)
	s.client.CloseIdleConnections()
	close(s.sockDone)

	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()
	var err error
	if !closing {
		err = s.classify(cause, lost)
		s.log.Info("ubersdr session ended", "err", err)
	}
	s.mu.Lock()
	if !s.closing {
		s.err = err
	}
	s.mu.Unlock()
	close(s.done)
}

// classify explains an unexpected end. UberSDR kicks (time limit,
// inactivity, admin) by dropping the TCP connection without a close frame,
// which looks like any other drop; a kicked UUID is refused by /connection
// with 410 for an hour, so one POST tells them apart.
func (s *session) classify(cause error, lost bool) error {
	if !lost {
		return cause
	}
	if s.maxDur > 0 && time.Since(s.opened) >= s.maxDur-sessionTimeSlack {
		return &provider.RejectedError{Reason: "receiver session time limit reached"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cr, status, err := s.p.postConnection(ctx, s.p.client, s.base, s.id)
	if err == nil {
		switch status {
		case http.StatusGone:
			return &provider.RejectedError{Reason: "receiver ended the session (kicked or timed out)"}
		case http.StatusForbidden:
			return connectionError(status, cr)
		}
	}
	return fmt.Errorf("ubersdr: upstream connection lost: %w", cause)
}

func (s *session) audioLoop(dec *opusDecoder) {
	defer s.wg.Done()
	for {
		typ, b, err := s.ac.Read(s.ctx)
		if err != nil {
			s.end(err, true)
			return
		}
		if typ == websocket.MessageText {
			var m serverMessage
			if json.Unmarshal(b, &m) == nil && m.Type == "error" {
				s.log.Warn("ubersdr audio socket error message", "error", m.Error, "status", m.Status)
			}
			continue
		}
		d, err := dec.decode(b)
		if err != nil {
			s.end(err, false)
			return
		}
		s.lastAudio.Store(time.Now().UnixNano())
		offer(s.audio, d.pkt)
	}
}

func (s *session) specLoop(dec *specDecoder) {
	defer s.wg.Done()
	bad := 0
	for {
		typ, b, err := s.sc.Read(s.ctx)
		if err != nil {
			s.end(err, true)
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		if isGzip(b) {
			m, err := decodeControl(b)
			if err != nil {
				continue
			}
			switch m.Type {
			case "config":
				dec.config(m)
			case "error":
				s.log.Warn("ubersdr spectrum socket error message", "error", m.Error, "status", m.Status)
			}
			continue
		}
		row, ok, err := dec.frame(b)
		if err != nil {
			if bad++; bad == 1 || bad%100 == 0 {
				s.log.Warn("ubersdr bad spectrum frame", "err", err, "count", bad)
			}
			continue
		}
		if ok {
			offer(s.spec, row)
		}
	}
}

// keepalive sends JSON pings on both sockets and ends a stalled session.
func (s *session) keepalive() {
	defer s.wg.Done()
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	lastPing := time.Now()
	ping := []byte(`{"type":"ping"}`)
	for {
		select {
		case <-s.stop:
			return
		case now := <-t.C:
			if now.Sub(time.Unix(0, s.lastAudio.Load())) > stallTimeout {
				s.end(errors.New("ubersdr: no audio from upstream for 20s"), true)
				return
			}
			if now.Sub(lastPing) < s.ping {
				continue
			}
			lastPing = now
			for _, c := range []*websocket.Conn{s.ac, s.sc} {
				if c == nil {
					continue
				}
				ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
				_ = c.Write(ctx, websocket.MessageText, ping)
				cancel()
			}
		}
	}
}

// offer queues v, dropping the oldest item rather than blocking.
func offer[T any](c chan T, v T) {
	select {
	case c <- v:
		return
	default:
	}
	select {
	case <-c:
	default:
	}
	select {
	case c <- v:
	default:
	}
}
