package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
)

type activityEventJSON struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Kind     string `json:"kind"`
	Summary  string `json:"summary"`
	AtUnix   int64  `json:"at_unix"`
}

func newActivityEventJSON(e *gatewaypb.ActivityEvent) activityEventJSON {
	return activityEventJSON{
		ID:       e.GetId(),
		TenantID: e.GetTenantId(),
		Kind:     e.GetKind(),
		Summary:  e.GetSummary(),
		AtUnix:   e.GetAtUnix(),
	}
}

// handleStreamActivity serves GET /v1/activity[?tenant_id=...] — the
// REST facade's equivalent of gateway.proto's server-streaming
// StreamActivity RPC. There is no REST/HTTP equivalent of gRPC
// streaming (see docs/adr/0003's "why gRPC first" section), so this
// handler approximates it with newline-delimited JSON
// (Content-Type: application/x-ndjson): one ActivityEvent JSON object
// per line, flushed as each event arrives, for as long as the HTTP
// client stays connected — curl -N, or any HTTP client that reads the
// response body incrementally rather than buffering it, sees events in
// real time.
func (h *Handler) handleStreamActivity(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeBadRequest(w, h.log, "streaming not supported by this response writer")
		return
	}

	tenantID := r.URL.Query().Get("tenant_id")

	stream, err := h.client.StreamActivity(clientContext(r), &gatewaypb.StreamActivityRequest{TenantId: tenantID})
	if err != nil {
		writeGRPCError(w, h.log, err)
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			// Headers are already sent (streaming started) — the
			// client's best signal at this point is the connection
			// simply ending, so log and stop rather than attempting a
			// second (invalid, after WriteHeader) status code.
			h.log.Error("httpapi: StreamActivity recv failed", "error", err)
			return
		}
		if err := enc.Encode(newActivityEventJSON(event)); err != nil {
			h.log.Error("httpapi: failed encoding activity event", "error", err)
			return
		}
		flusher.Flush()

		select {
		case <-r.Context().Done():
			return
		default:
		}
	}
}
