package pskr

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// fakeBroker speaks just enough MQTT 3.1.1 for a subscriber: CONNACK,
// SUBACK, PINGRESP and QoS 0 PUBLISH, over net.Pipe.
type fakeBroker struct {
	conns chan *brokerConn
}

type brokerConn struct {
	c    net.Conn
	subs chan []string // filters of each SUBSCRIBE
	// clientID from the CONNECT packet.
	clientID chan string
}

func (fb *fakeBroker) dial() (net.Conn, error) {
	server, client := net.Pipe()
	bc := &brokerConn{c: server, subs: make(chan []string, 4), clientID: make(chan string, 1)}
	go bc.serve()
	fb.conns <- bc
	return client, nil
}

func (bc *brokerConn) serve() {
	defer bc.c.Close()
	r := bufio.NewReader(bc.c)
	for {
		hdr, err := r.ReadByte()
		if err != nil {
			return
		}
		n, err := readVarint(r)
		if err != nil {
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return
		}
		switch hdr >> 4 {
		case 1: // CONNECT
			// protocol name, level, flags, keepalive, then client id.
			pn := int(binary.BigEndian.Uint16(body))
			p := 2 + pn + 1 + 1 + 2
			idLen := int(binary.BigEndian.Uint16(body[p:]))
			bc.clientID <- string(body[p+2 : p+2+idLen])
			bc.write([]byte{0x20, 0x02, 0x00, 0x00})
		case 8: // SUBSCRIBE
			var filters []string
			for p := 2; p < len(body); {
				l := int(binary.BigEndian.Uint16(body[p:]))
				filters = append(filters, string(body[p+2:p+2+l]))
				p += 2 + l + 1
			}
			ack := []byte{0x90, byte(2 + len(filters)), body[0], body[1]}
			ack = append(ack, make([]byte, len(filters))...)
			bc.write(ack)
			sort.Strings(filters)
			bc.subs <- filters
		case 12: // PINGREQ
			bc.write([]byte{0xD0, 0x00})
		case 14: // DISCONNECT
			return
		}
	}
}

func (bc *brokerConn) write(b []byte) { _, _ = bc.c.Write(b) }

func (bc *brokerConn) publish(topic string, payload []byte) {
	var body []byte
	body = binary.BigEndian.AppendUint16(body, uint16(len(topic)))
	body = append(body, topic...)
	body = append(body, payload...)
	pkt := []byte{0x30}
	pkt = appendVarint(pkt, len(body))
	bc.write(append(pkt, body...))
}

func readVarint(r *bufio.Reader) (int, error) {
	v, mul := 0, 1
	for range 4 {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v += int(b&0x7f) * mul
		if b&0x80 == 0 {
			return v, nil
		}
		mul *= 128
	}
	return 0, io.ErrUnexpectedEOF
}

func appendVarint(b []byte, n int) []byte {
	for {
		d := byte(n % 128)
		n /= 128
		if n > 0 {
			d |= 0x80
		}
		b = append(b, d)
		if n == 0 {
			return b
		}
	}
}

func TestClientSubscribesFeedsAndReconnects(t *testing.T) {
	// Only spots on the subscribed bands: the fake broker does no
	// filtering, and the client drops messages that match no route.
	var msgs []fixtureMsg
	for _, m := range loadFixture(t) {
		if strings.HasPrefix(m.Topic, "pskr/filter/v2/20m/") || strings.HasPrefix(m.Topic, "pskr/filter/v2/40m/") {
			msgs = append(msgs, m)
		}
	}
	if len(msgs) < 3 {
		t.Fatalf("fixture has %d 20m/40m spots, want 3", len(msgs))
	}
	synctest.Test(t, func(t *testing.T) {
		fb := &fakeBroker{conns: make(chan *brokerConn, 4)}
		store := NewStore(DefaultParams())
		handled := make(chan struct{}, 64)
		c := NewClient(Config{
			Bands:   []string{"20m", "40m"},
			Logger:  slog.New(slog.DiscardHandler),
			dial:    fb.dial,
			handled: func() { handled <- struct{}{} },
		}, store)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- c.Run(ctx) }()

		want := []string{"pskr/filter/v2/20m/#", "pskr/filter/v2/40m/#"}
		checkSubs := func(bc *brokerConn) {
			t.Helper()
			got := <-bc.subs
			if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
				t.Fatalf("subscribed to %v, want %v", got, want)
			}
		}

		bc := <-fb.conns
		if id := <-bc.clientID; len(id) != len("nsl-")+8 || id[:4] != "nsl-" {
			t.Fatalf("client id %q", id)
		}
		checkSubs(bc)
		bc.publish(msgs[0].Topic, msgs[0].Payload)
		bc.publish(msgs[1].Topic, []byte("{broken"))
		<-handled
		<-handled
		synctest.Wait()
		if st := c.Stats(); !st.Connected || st.Messages != 2 || st.BadPayload != 1 || !st.LastMessage.Equal(time.Now()) {
			t.Fatalf("client stats %+v", st)
		}
		if st := store.Stats(time.Now()); st.Accepted != 1 {
			t.Fatalf("store stats %+v", st)
		}

		// The broker drops the connection; the client reconnects and
		// subscribes again.
		bc.c.Close()
		bc2 := <-fb.conns
		<-bc2.clientID
		checkSubs(bc2)
		bc2.publish(msgs[2].Topic, msgs[2].Payload)
		<-handled
		if st := store.Stats(time.Now()); st.Accepted != 2 {
			t.Fatalf("after reconnect: %+v", st)
		}

		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run: %v", err)
		}
		if c.Stats().Connected {
			t.Fatal("still connected after shutdown")
		}
	})
}

// Refused connections are retried, not given up on.
func TestClientRetriesInitialConnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fb := &fakeBroker{conns: make(chan *brokerConn, 4)}
		attempts := 0
		c := NewClient(Config{
			Logger: slog.New(slog.DiscardHandler),
			dial: func() (net.Conn, error) {
				attempts++
				if attempts < 3 {
					return nil, &net.OpError{Op: "dial", Err: io.ErrUnexpectedEOF}
				}
				return fb.dial()
			},
		}, NewStore(DefaultParams()))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- c.Run(ctx) }()
		bc := <-fb.conns
		<-bc.clientID
		if got := <-bc.subs; len(got) != len(HFBands) {
			t.Fatalf("default subscription %v", got)
		}
		if waited := time.Since(start); waited != 2*DefaultConnectRetryInterval {
			t.Fatalf("connected after %v, want two retry intervals", waited)
		}
		cancel()
		<-done
	})
}

func TestFilters(t *testing.T) {
	c := NewClient(Config{TopicRoot: "pskr/filter/v2raw", Bands: []string{"30m"}}, NewStore(Params{}))
	f := c.Filters()
	if len(f) != 1 || f["pskr/filter/v2raw/30m/#"] != 0 {
		t.Fatalf("filters %v", f)
	}
}
