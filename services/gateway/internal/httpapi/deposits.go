package httpapi

import (
	"net/http"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
)

type simulateDepositRequestJSON struct {
	AmountMinorUnits int64 `json:"amount_minor_units"`
	ExpectedAtUnix   int64 `json:"expected_at_unix,omitempty"`
	MarkLate         bool  `json:"mark_late,omitempty"`
}

type simulateDepositResponseJSON struct {
	DepositID string `json:"deposit_id"`
}

// handleSimulateDeposit serves POST /v1/tenants/{tenant_id}/deposits/simulate
// — a demo/judging hook, same as the RPC it fronts (see gateway.proto's
// SimulateDeposit doc comment).
func (h *Handler) handleSimulateDeposit(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenant_id")
	if tenantID == "" {
		writeBadRequest(w, h.log, "tenant_id path parameter is required")
		return
	}

	var body simulateDepositRequestJSON
	if err := decodeJSONBody(r, &body); err != nil {
		writeBadRequest(w, h.log, err.Error())
		return
	}
	if body.AmountMinorUnits <= 0 {
		writeBadRequest(w, h.log, "amount_minor_units must be > 0")
		return
	}

	resp, err := h.client.SimulateDeposit(clientContext(r), &gatewaypb.SimulateDepositRequest{
		TenantId:         tenantID,
		AmountMinorUnits: body.AmountMinorUnits,
		ExpectedAtUnix:   body.ExpectedAtUnix,
		MarkLate:         body.MarkLate,
	})
	if err != nil {
		writeGRPCError(w, h.log, err)
		return
	}
	writeJSON(w, h.log, http.StatusCreated, simulateDepositResponseJSON{DepositID: resp.GetDepositId()})
}
