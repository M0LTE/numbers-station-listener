package api

import (
	"net/http/httptest"
	"testing"
)

func TestPortRedirect(t *testing.T) {
	cases := map[string]string{
		"numbers":        "http://numbers:8080/api/now?x=1",
		"numbers.lan:80": "http://numbers.lan:8080/api/now?x=1",
		"10.45.0.26":     "http://10.45.0.26:8080/api/now?x=1",
		"[fd00::1]:80":   "http://[fd00::1]:8080/api/now?x=1",
	}
	for host, want := range cases {
		req := httptest.NewRequest("GET", "/api/now?x=1", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		PortRedirect("8080").ServeHTTP(rec, req)
		if rec.Code != 302 || rec.Header().Get("Location") != want {
			t.Errorf("%s: %d %q, want %q", host, rec.Code, rec.Header().Get("Location"), want)
		}
	}
}
