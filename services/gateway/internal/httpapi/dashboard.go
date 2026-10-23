// This file implements the judge-facing live web dashboard: a
// shareable URL (GET /v1/dashboard) a judge can open in a browser
// instead of running services/tui locally. Ration's actual
// differentiators — the deterministic hard-rule envelope, the
// reserve-floor headroom every bridge-funding decision is gated on,
// the agent's recorded rationale — are otherwise invisible in a
// terminal demo unless explicitly called out on screen; this gives
// them a self-evident visual home.
//
// Deliberately NOT behind internal/tenantauth's credential check:
// GetDashboardSnapshot carries neither a tenant_id nor a card_id field
// (see gateway.proto's doc comment on the RPC), which
// internal/tenantauth's own doc comment already documents as passing
// through unscoped — "this interceptor only ever narrows what a
// request it can actually scope to a tenant can do." That is the
// correct, intentional behavior here, not an oversight: the entire
// point of a judge-facing dashboard is a URL that opens with zero
// login friction. It is read-only (no mutating RPC is reachable from
// this file) and aggregates data that is not otherwise
// commercially/competitively sensitive in a hackathon/demo deployment
// — this is explicitly a demo/judging posture, not a production one
// (see this project's many other "DEV-ONLY" postures, e.g.
// infra/dev-ca/generate-dev-ca.sh), and should gate behind real
// authentication before ever fronting real tenant data.
package httpapi

import (
	_ "embed"
	"encoding/json"
	"io"
	"net/http"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	"google.golang.org/grpc/metadata"
)

//go:embed dashboard.html
var dashboardHTML []byte

// handleDashboardPage serves the dashboard's one static HTML page — a
// single self-contained file (inline CSS, inline vanilla JS, zero
// external script/stylesheet/CDN dependencies) so it renders
// correctly even on a network-isolated judging box with no outbound
// internet access. The page's own JS polls handleDashboardSnapshot and
// opens an EventSource against handleDashboardActivityStream.
func (h *Handler) handleDashboardPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(dashboardHTML)
}

// tenantSnapshotJSON/agentDecisionSummaryJSON/dashboardSnapshotJSON
// mirror gateway.proto's GetDashboardSnapshotResponse shape in
// snake_case JSON, the same translation convention every other
// handler in this package (e.g. activityEventJSON) already follows.
type tenantSnapshotJSON struct {
	TenantID               string `json:"tenant_id"`
	TenantName             string `json:"tenant_name"`
	Currency               string `json:"currency"`
	ReserveFloorMinorUnits int64  `json:"reserve_floor_minor_units"`
	AvailableMinorUnits    int64  `json:"available_minor_units"`
	HeldMinorUnits         int64  `json:"held_minor_units"`
	HeadroomMinorUnits     int64  `json:"headroom_minor_units"`
}

type agentDecisionSummaryJSON struct {
	TenantID           string `json:"tenant_id"`
	TenantName         string `json:"tenant_name"`
	Trigger            string `json:"trigger"`
	Action             string `json:"action"`
	Rationale          string `json:"rationale"`
	PolicySnapshotJSON string `json:"policy_snapshot_json"`
	CreatedAtUnix      int64  `json:"created_at_unix"`
}

type dashboardSnapshotJSON struct {
	Tenants         []tenantSnapshotJSON       `json:"tenants"`
	RecentDecisions []agentDecisionSummaryJSON `json:"recent_decisions"`
}

func newTenantSnapshotJSON(t *gatewaypb.TenantSnapshot) tenantSnapshotJSON {
	return tenantSnapshotJSON{
		TenantID:               t.GetTenantId(),
		TenantName:             t.GetTenantName(),
		Currency:               t.GetCurrency(),
		ReserveFloorMinorUnits: t.GetReserveFloorMinorUnits(),
		AvailableMinorUnits:    t.GetAvailableMinorUnits(),
		HeldMinorUnits:         t.GetHeldMinorUnits(),
		HeadroomMinorUnits:     t.GetHeadroomMinorUnits(),
	}
}

func newAgentDecisionSummaryJSON(d *gatewaypb.AgentDecisionSummary) agentDecisionSummaryJSON {
	return agentDecisionSummaryJSON{
		TenantID:           d.GetTenantId(),
		TenantName:         d.GetTenantName(),
		Trigger:            d.GetTrigger(),
		Action:             d.GetAction().String(),
		Rationale:          d.GetRationale(),
		PolicySnapshotJSON: d.GetPolicySnapshotJson(),
		CreatedAtUnix:      d.GetCreatedAtUnix(),
	}
}

// handleDashboardSnapshot serves GET /v1/dashboard/snapshot — the REST
// facade's JSON translation of gateway.proto's GetDashboardSnapshot
// RPC, polled by dashboard.html every few seconds.
func (h *Handler) handleDashboardSnapshot(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.GetDashboardSnapshot(clientContext(r), &gatewaypb.GetDashboardSnapshotRequest{})
	if err != nil {
		writeGRPCError(w, h.log, err)
		return
	}

	out := dashboardSnapshotJSON{
		Tenants:         make([]tenantSnapshotJSON, 0, len(resp.GetTenants())),
		RecentDecisions: make([]agentDecisionSummaryJSON, 0, len(resp.GetRecentDecisions())),
	}
	for _, t := range resp.GetTenants() {
		out.Tenants = append(out.Tenants, newTenantSnapshotJSON(t))
	}
	for _, d := range resp.GetRecentDecisions() {
		out.RecentDecisions = append(out.RecentDecisions, newAgentDecisionSummaryJSON(d))
	}

	writeJSON(w, h.log, http.StatusOK, out)
}

// handleDashboardActivityStream serves GET /v1/dashboard/activity/stream
// — a true Server-Sent Events (text/event-stream) framing of the exact
// same gateway.proto StreamActivity RPC handleStreamActivity already
// exposes as newline-delimited JSON. SSE gets its own endpoint (rather
// than content-negotiating on /v1/activity) because the two framings
// have different clients and different contracts to keep stable:
// /v1/activity's NDJSON is this project's existing documented REST API
// surface (sdk/go, sdk/rust, any curl -N caller); this endpoint exists
// solely so a browser's native EventSource can consume it with zero
// client-side parsing code, which is exactly what dashboard.html's own
// JS does.
func (h *Handler) handleDashboardActivityStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeBadRequest(w, h.log, "streaming not supported by this response writer")
		return
	}

	tenantID := r.URL.Query().Get("tenant_id")

	// Deliberately NOT clientContext(r): the dashboard is a public,
	// credential-free surface by design (see this file's package doc
	// comment) — there is no caller-supplied credential to forward.
	// internal/tenantauth's StreamServerInterceptor requires SOME valid
	// credential on every streaming RPC regardless of tenant_id
	// (unlike the unary interceptor's "no tenant_id field, nothing to
	// authorize" exemption GetDashboardSnapshot relies on), so this
	// presents gateway's OWN operator-scoped credential instead — see
	// Handler.dashboardOperatorToken's doc comment for exactly why that
	// is safe here and would NOT be safe on handleStreamActivity's
	// /v1/activity route.
	ctx := r.Context()
	if h.dashboardOperatorToken != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+h.dashboardOperatorToken)
	}

	stream, err := h.client.StreamActivity(ctx, &gatewaypb.StreamActivityRequest{TenantId: tenantID})
	if err != nil {
		writeGRPCError(w, h.log, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx-style proxy buffering, if any sits in front
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			h.log.Error("httpapi: dashboard activity SSE recv failed", "error", err)
			return
		}

		payload, err := json.Marshal(newActivityEventJSON(event))
		if err != nil {
			h.log.Error("httpapi: failed encoding dashboard activity event", "error", err)
			return
		}
		// SSE framing: "data: <payload>\n\n" — the blank line is what
		// terminates one event for EventSource's parser. id: lets a
		// reconnecting browser resume cleanly (the browser replays
		// Last-Event-ID; this handler does not currently use it to
		// replay missed events — a reconnect simply resumes the live
		// feed from "now", same posture as /v1/activity's NDJSON and
		// services/tui's own StreamActivity client).
		if _, err := w.Write([]byte("id: " + event.GetId() + "\ndata: " + string(payload) + "\n\n")); err != nil {
			h.log.Error("httpapi: dashboard activity SSE write failed", "error", err)
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
