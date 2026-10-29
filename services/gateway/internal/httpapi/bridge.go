package httpapi

import (
	"errors"
	"net/http"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
)

type rationBridgeFundingRequestJSON struct {
	RequestedMinorUnits int64  `json:"requested_minor_units"`
	Reason              string `json:"reason"`
}

type rationBridgeFundingResponseJSON struct {
	GrantedMinorUnits int64  `json:"granted_minor_units"`
	FullyGranted      bool   `json:"fully_granted"`
	Explanation       string `json:"explanation"`
}

// handleRationBridgeFunding serves POST /v1/tenants/{tenant_id}/bridge-funding.
func (h *Handler) handleRationBridgeFunding(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenant_id")
	if tenantID == "" {
		writeBadRequest(w, h.log, "tenant_id path parameter is required")
		return
	}

	var body rationBridgeFundingRequestJSON
	if err := decodeJSONBody(w, r, &body); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeRequestEntityTooLarge(w, h.log, err.Error())
		} else {
			writeBadRequest(w, h.log, err.Error())
		}
		return
	}
	if body.RequestedMinorUnits <= 0 {
		writeBadRequest(w, h.log, "requested_minor_units must be > 0")
		return
	}

	resp, err := h.client.RationBridgeFunding(clientContext(r), &gatewaypb.RationBridgeFundingRequest{
		TenantId:            tenantID,
		RequestedMinorUnits: body.RequestedMinorUnits,
		Reason:              body.Reason,
	})
	if err != nil {
		writeGRPCError(w, h.log, err)
		return
	}
	writeJSON(w, h.log, http.StatusOK, rationBridgeFundingResponseJSON{
		GrantedMinorUnits: resp.GetGrantedMinorUnits(),
		FullyGranted:      resp.GetFullyGranted(),
		Explanation:       resp.GetExplanation(),
	})
}
