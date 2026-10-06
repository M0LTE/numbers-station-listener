package api

import (
	"net"
	"net/http"
)

// PortRedirect sends every request to the same host and path on another
// port. It answers 302 rather than 301 so browsers do not remember it if
// the site later moves to port 80 itself.
func PortRedirect(port string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host == "" {
			http.Error(w, "no host", http.StatusBadRequest)
			return
		}
		target := "http://" + net.JoinHostPort(host, port) + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusFound)
	})
}
