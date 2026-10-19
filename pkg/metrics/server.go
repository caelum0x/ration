package metrics

import (
	"net/http"
	"time"
)

// Server builds the HTTP server exposing m's /metrics endpoint at addr.
// Callers are responsible for starting it (typically in its own
// goroutine) and shutting it down on process exit, mirroring how every
// service in this project already manages its gRPC/HTTP listeners in
// main.go.
func (m *Metrics) Server(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
}
