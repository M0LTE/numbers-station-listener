package pskr

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"
)

// TestLiveFeed subscribes to the real broker (mqtt.pskreporter.info, run
// by M0LTE) until 200 messages have arrived. It only runs with
// NSL_INTEGRATION=1. It has no deadline: a dead feed hangs, which is an
// honest failure.
func TestLiveFeed(t *testing.T) {
	if os.Getenv("NSL_INTEGRATION") != "1" {
		t.Skip("set NSL_INTEGRATION=1 to subscribe to the live PSKReporter feed")
	}
	const want = 200
	store := NewStore(DefaultParams())
	handled := make(chan struct{}, want)
	c := NewClient(Config{
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
		handled: func() {
			select {
			case handled <- struct{}{}:
			default:
			}
		},
	}, store)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	for range want {
		<-handled
	}
	cancel()
	<-done

	cs := c.Stats()
	st := store.Stats(time.Now())
	t.Logf("client %+v, store %+v", cs, st)
	if cs.BadPayload != 0 {
		t.Errorf("%d of %d payloads failed to parse", cs.BadPayload, cs.Messages)
	}
	if st.Accepted == 0 || st.Rejected > st.Accepted/10 {
		t.Errorf("accepted %d, rejected %d", st.Accepted, st.Rejected)
	}
}
