package ubersdr

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// connTracker remembers the raw connections a session's transport dialled,
// so that a session can be torn down within its bound even when the peer
// never answers the WebSocket close handshake.
type connTracker struct {
	mu     sync.Mutex
	conns  []net.Conn
	closed bool
}

type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func (t *connTracker) wrap(d dialFunc) dialFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := d(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.closed {
			c.Close()
			return nil, net.ErrClosed
		}
		t.conns = append(t.conns, c)
		return c, nil
	}
}

// closeAll closes every connection dialled so far and refuses new ones.
func (t *connTracker) closeAll() {
	if t == nil {
		return
	}
	t.mu.Lock()
	conns := t.conns
	t.conns, t.closed = nil, true
	t.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// sessionClient returns a client for one session. When the provider's
// transport is an *http.Transport (or the default), it is cloned with its
// dialers tracked and HTTP/2 disabled, since a WebSocket upgrade needs
// HTTP/1.1. Otherwise the tracker is nil and Close cannot force anything.
func (p *Provider) sessionClient() (*http.Client, *connTracker) {
	rt := p.client.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	base, ok := rt.(*http.Transport)
	if !ok {
		return &http.Client{Transport: rt}, nil
	}
	tr := &connTracker{}
	c := base.Clone()
	d := c.DialContext
	if d == nil {
		nd := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		d = nd.DialContext
	}
	c.DialContext = tr.wrap(d)
	if c.DialTLSContext != nil {
		c.DialTLSContext = tr.wrap(c.DialTLSContext)
	}
	// HTTP/1.1 only. Clearing TLSNextProto is not enough on its own: a clone
	// of a transport that has already been used (http.DefaultTransport
	// always has) carries "h2" in its TLS NextProtos, the server picks it,
	// and the HTTP/1 client then reads HTTP/2 frames (seen live on M0LTE).
	c.ForceAttemptHTTP2 = false
	c.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if c.TLSClientConfig != nil {
		c.TLSClientConfig = c.TLSClientConfig.Clone()
	} else {
		c.TLSClientConfig = &tls.Config{}
	}
	c.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return &http.Client{Transport: c}, tr
}

// closeConns closes WebSocket connections with a normal close frame each,
// in parallel. If the handshakes have not finished within bound, the raw
// connections are closed under them. It returns once every Close call has
// returned, or at bound when nothing can be forced.
func closeConns(tr *connTracker, bound time.Duration, conns ...*websocket.Conn) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, c := range conns {
		if c == nil {
			continue
		}
		wg.Add(1)
		go func(c *websocket.Conn) {
			defer wg.Done()
			_ = c.Close(websocket.StatusNormalClosure, "")
		}(c)
	}
	go func() { wg.Wait(); close(done) }()
	t := time.NewTimer(bound)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-t.C:
	}
	if tr == nil {
		return
	}
	tr.closeAll()
	<-done
}
