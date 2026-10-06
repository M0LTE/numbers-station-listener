package pskr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// Defaults for Config.
const (
	DefaultBroker               = "tcp://mqtt.pskreporter.info:1883"
	DefaultTopicRoot            = "pskr/filter/v2"
	DefaultConnectRetryInterval = 15 * time.Second
	DefaultMaxReconnectInterval = 2 * time.Minute
)

// Config configures a Client. Zero values take the defaults.
type Config struct {
	// Broker is the MQTT URL; DefaultBroker when empty.
	Broker string
	// TopicRoot is the topic tree to subscribe under; DefaultTopicRoot
	// (the tier with valid locators at both ends) when empty.
	TopicRoot string
	// Bands to subscribe to, one "{root}/{band}/#" filter each; every
	// band in HFBands when empty.
	Bands []string
	// ClientID; "nsl-" plus random hex when empty.
	ClientID string
	// ConnectRetryInterval is the wait between attempts before the first
	// connection succeeds; DefaultConnectRetryInterval when zero.
	ConnectRetryInterval time.Duration
	// MaxReconnectInterval caps the doubling backoff after a lost
	// connection; DefaultMaxReconnectInterval when zero.
	MaxReconnectInterval time.Duration
	// Logger; slog.Default() when nil.
	Logger *slog.Logger

	// dial replaces the network connection, for tests.
	dial func() (net.Conn, error)
	// handled is called after each message, for tests.
	handled func()
}

// ClientStats describes the feed connection, for admin pages.
type ClientStats struct {
	Connected bool `json:"connected"`
	// Messages is every message received; BadPayload of them failed to
	// parse.
	Messages    uint64    `json:"messages"`
	BadPayload  uint64    `json:"badPayload"`
	LastMessage time.Time `json:"lastMessage"`
}

// Client subscribes to the PSKReporter feed and adds every spot to a
// Store. It subscribes at QoS 0 with a clean session, as the broker's
// operator asks: a slow QoS 1 subscriber makes the broker buffer without
// bound. Spots missed while disconnected are simply missed.
type Client struct {
	cfg   Config
	store *Store
	log   *slog.Logger

	mu    sync.Mutex
	stats ClientStats
}

// NewClient makes a client feeding store. Call Run to start it.
func NewClient(cfg Config, store *Store) *Client {
	if cfg.Broker == "" {
		cfg.Broker = DefaultBroker
	}
	if cfg.TopicRoot == "" {
		cfg.TopicRoot = DefaultTopicRoot
	}
	if len(cfg.Bands) == 0 {
		for _, b := range HFBands {
			cfg.Bands = append(cfg.Bands, b.Name)
		}
	}
	if cfg.ClientID == "" {
		var b [4]byte
		_, _ = rand.Read(b[:])
		cfg.ClientID = "nsl-" + hex.EncodeToString(b[:])
	}
	if cfg.ConnectRetryInterval <= 0 {
		cfg.ConnectRetryInterval = DefaultConnectRetryInterval
	}
	if cfg.MaxReconnectInterval <= 0 {
		cfg.MaxReconnectInterval = DefaultMaxReconnectInterval
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Client{cfg: cfg, store: store, log: log.With("component", "pskr")}
}

// Filters returns the topic filters the client subscribes to.
func (c *Client) Filters() map[string]byte {
	f := make(map[string]byte, len(c.cfg.Bands))
	for _, b := range c.cfg.Bands {
		f[c.cfg.TopicRoot+"/"+b+"/#"] = 0
	}
	return f
}

// Stats returns the connection counters.
func (c *Client) Stats() ClientStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

// Run connects, retrying and reconnecting with backoff, and feeds the
// store until ctx is done. It returns nil on a clean shutdown.
func (c *Client) Run(ctx context.Context) error {
	if c.store == nil {
		return errors.New("pskr: client has no store")
	}
	opts := mqtt.NewClientOptions().
		AddBroker(c.cfg.Broker).
		SetClientID(c.cfg.ClientID).
		SetCleanSession(true).
		SetOrderMatters(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(c.cfg.ConnectRetryInterval).
		SetMaxReconnectInterval(c.cfg.MaxReconnectInterval).
		SetOnConnectHandler(c.onConnect).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			c.setConnected(false)
			c.log.Warn("pskreporter feed connection lost, reconnecting", "err", err)
		})
	if c.cfg.dial != nil {
		dial := c.cfg.dial
		opts.SetCustomOpenConnectionFn(func(*url.URL, mqtt.ClientOptions) (net.Conn, error) { return dial() })
	}
	cl := mqtt.NewClient(opts)
	c.log.Info("pskreporter feed connecting", "broker", c.cfg.Broker, "bands", len(c.cfg.Bands))
	cl.Connect() // with ConnectRetry the token only completes once connected
	<-ctx.Done()
	cl.Disconnect(250)
	c.setConnected(false)
	return nil
}

func (c *Client) onConnect(cl mqtt.Client) {
	c.setConnected(true)
	filters := c.Filters()
	c.log.Info("pskreporter feed connected, subscribing", "filters", len(filters))
	tok := cl.SubscribeMultiple(filters, func(_ mqtt.Client, m mqtt.Message) {
		c.handle(m.Payload(), time.Now())
	})
	// Do not block the connect handler on the SUBACK; log a refusal.
	go func() {
		tok.Wait()
		if err := tok.Error(); err != nil {
			c.log.Warn("pskreporter feed subscribe failed", "err", err)
		}
	}()
}

func (c *Client) handle(payload []byte, now time.Time) {
	sp, err := ParseSpot(payload)
	c.mu.Lock()
	c.stats.Messages++
	c.stats.LastMessage = now
	if err != nil {
		c.stats.BadPayload++
	}
	c.mu.Unlock()
	if err == nil {
		c.store.Add(sp, now)
	}
	if c.cfg.handled != nil {
		c.cfg.handled()
	}
}

func (c *Client) setConnected(v bool) {
	c.mu.Lock()
	c.stats.Connected = v
	c.mu.Unlock()
}
