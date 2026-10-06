package api

import (
	"fmt"
	"net/http"
	"strings"
)

// handleMetrics writes Prometheus text format by hand; there are few
// enough series that a client library would not earn its place.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	st := s.relay.Stats()
	var b strings.Builder
	g := func(name, help, typ string, v float64) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n%s %g\n", name, help, name, typ, name, v)
	}
	g("nsl_upstream_sessions", "Upstream receiver sessions currently held.", "gauge", float64(st.UpstreamSessions))
	g("nsl_listeners", "Browser listeners attached (audio plus spectrum).", "gauge", float64(st.Listeners))
	g("nsl_idle_upstream_sessions", "Upstream sessions with no listeners (inside the grace period).", "gauge", float64(st.IdleUpstreams))
	g("nsl_overdue_upstream_sessions", "Upstream sessions idle past the grace period. Must be 0; alert otherwise.", "gauge", float64(st.OverdueUpstreams))
	g("nsl_upstream_opens_total", "Upstream sessions opened.", "counter", float64(st.Opens))
	g("nsl_upstream_open_failures_total", "Upstream session opens that failed.", "counter", float64(st.OpenFailures))
	g("nsl_upstream_grace_closes_total", "Upstream sessions released after the grace period.", "counter", float64(st.GraceCloses))
	g("nsl_watchdog_closes_total", "Upstream sessions the watchdog had to force-close (a bug).", "counter", float64(st.WatchdogCloses))
	g("nsl_leaked_leases_total", "Listener leases the watchdog had to release (a bug).", "counter", float64(st.LeakedLeases))
	g("nsl_upstream_reconnects_total", "Reconnects for remaining listeners after an upstream ended.", "counter", float64(st.Reconnects))
	g("nsl_sse_clients", "Server-Sent Event clients.", "gauge", float64(s.hub.count()))
	if s.prober != nil {
		rounds, probes := s.prober.Counts()
		g("nsl_probe_rounds_total", "Probe rounds run.", "counter", float64(rounds))
		g("nsl_probes_total", "Individual receiver probes made.", "counter", float64(probes))
	}
	g("nsl_receivers", "Receivers in the directory.", "gauge", float64(len(s.dir.Receivers())))
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = w.Write([]byte(b.String()))
}
