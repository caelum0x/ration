package httpapi

import (
	"net/http"

	gatewaypb "github.com/caelum0x/ration/pkg/pb/gateway"
	"google.golang.org/grpc/codes"
)

// cardJSON is gateway.proto's Card message, field-for-field, with
// Status rendered as its string name (e.g. "CARD_STATUS_ACTIVE")
// rather than a raw protobuf enum integer — the one concession this
// facade makes for a plain-JSON/curl audience that wouldn't otherwise
// know commonpb.CardStatus's int-to-name mapping.
type cardJSON struct {
	ID              string `json:"id"`
	TenantID        string `json:"tenant_id"`
	AirwallexCardID string `json:"airwallex_card_id"`
	CardholderName  string `json:"cardholder_name"`
	Status          string `json:"status"`
	LimitMinorUnits int64  `json:"limit_minor_units"`
	LimitInterval   string `json:"limit_interval"`
	Currency        string `json:"currency"`
	CreatedAtUnix   int64  `json:"created_at_unix"`
}

func newCardJSON(c *gatewaypb.Card) cardJSON {
	return cardJSON{
		ID:              c.GetId(),
		TenantID:        c.GetTenantId(),
		AirwallexCardID: c.GetAirwallexCardId(),
		CardholderName:  c.GetCardholderName(),
		Status:          c.GetStatus().String(),
		LimitMinorUnits: c.GetLimitMinorUnits(),
		LimitInterval:   c.GetLimitInterval(),
		Currency:        c.GetCurrency(),
		CreatedAtUnix:   c.GetCreatedAtUnix(),
	}
}

type issueCardRequestJSON struct {
	TenantID        string `json:"tenant_id"`
	CardholderName  string `json:"cardholder_name"`
	LimitMinorUnits int64  `json:"limit_minor_units"`
	LimitInterval   string `json:"limit_interval"`
	Currency        string `json:"currency"`
}

// handleIssueCard serves POST /v1/cards.
func (h *Handler) handleIssueCard(w http.ResponseWriter, r *http.Request) {
	var body issueCardRequestJSON
	if err := decodeJSONBody(r, &body); err != nil {
		writeBadRequest(w, h.log, err.Error())
		return
	}
	if body.TenantID == "" {
		writeBadRequest(w, h.log, "tenant_id is required")
		return
	}
	if body.CardholderName == "" {
		writeBadRequest(w, h.log, "cardholder_name is required")
		return
	}
	if body.LimitMinorUnits <= 0 {
		writeBadRequest(w, h.log, "limit_minor_units must be > 0")
		return
	}
	if body.Currency == "" {
		writeBadRequest(w, h.log, "currency is required")
		return
	}

	card, err := h.client.IssueCard(clientContext(r), &gatewaypb.IssueCardRequest{
		TenantId:        body.TenantID,
		CardholderName:  body.CardholderName,
		LimitMinorUnits: body.LimitMinorUnits,
		LimitInterval:   body.LimitInterval,
		Currency:        body.Currency,
	})
	if err != nil {
		writeGRPCError(w, h.log, err)
		return
	}
	writeJSON(w, h.log, http.StatusCreated, newCardJSON(card))
}

// handleGetCard serves GET /v1/cards/{card_id}.
//
// A card ID that doesn't exist surfaces as a proper gRPC
// codes.NotFound (services/card-issuance/internal/grpcserver wraps
// cardsvc's airwallex.ErrNotFound in status.Error(codes.NotFound, ...)
// before it leaves that service), which this facade's
// codes.NotFound -> 404 mapping (see errors.go's httpStatusForCode)
// then translates correctly — fixed for every GatewayClient caller
// (TUI, CLI, sdk/go/ration, sdk/rust/ration-client, this facade), not
// just here.
func (h *Handler) handleGetCard(w http.ResponseWriter, r *http.Request) {
	cardID := r.PathValue("card_id")
	if cardID == "" {
		writeBadRequest(w, h.log, "card_id path parameter is required")
		return
	}

	card, err := h.client.GetCard(clientContext(r), &gatewaypb.GetCardRequest{CardId: cardID})
	if err != nil {
		writeGRPCError(w, h.log, err)
		return
	}
	if card.GetId() == "" {
		// Defensive: some GatewayClient implementations may return a
		// nil/zero-value Card with no error for an unknown ID rather
		// than an error — treat that the same as a clean 404 rather
		// than serving an empty-but-200 body.
		body := errorBody{}
		body.Error.Code = codes.NotFound.String()
		body.Error.Message = "card not found"
		writeJSON(w, h.log, http.StatusNotFound, body)
		return
	}
	writeJSON(w, h.log, http.StatusOK, newCardJSON(card))
}

type adjustCardLimitRequestJSON struct {
	NewLimitMinorUnits int64  `json:"new_limit_minor_units"`
	Reason             string `json:"reason"`
}

// handleAdjustCardLimit serves PATCH /v1/cards/{card_id}/limit.
func (h *Handler) handleAdjustCardLimit(w http.ResponseWriter, r *http.Request) {
	cardID := r.PathValue("card_id")
	if cardID == "" {
		writeBadRequest(w, h.log, "card_id path parameter is required")
		return
	}

	var body adjustCardLimitRequestJSON
	if err := decodeJSONBody(r, &body); err != nil {
		writeBadRequest(w, h.log, err.Error())
		return
	}
	if body.NewLimitMinorUnits <= 0 {
		writeBadRequest(w, h.log, "new_limit_minor_units must be > 0")
		return
	}

	card, err := h.client.AdjustCardLimit(clientContext(r), &gatewaypb.AdjustCardLimitRequest{
		CardId:             cardID,
		NewLimitMinorUnits: body.NewLimitMinorUnits,
		Reason:             body.Reason,
	})
	if err != nil {
		writeGRPCError(w, h.log, err)
		return
	}
	writeJSON(w, h.log, http.StatusOK, newCardJSON(card))
}

type listCardsResponseJSON struct {
	Cards []cardJSON `json:"cards"`
}

// handleListCards serves GET /v1/tenants/{tenant_id}/cards.
func (h *Handler) handleListCards(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenant_id")
	if tenantID == "" {
		writeBadRequest(w, h.log, "tenant_id path parameter is required")
		return
	}

	resp, err := h.client.ListCards(clientContext(r), &gatewaypb.ListCardsRequest{TenantId: tenantID})
	if err != nil {
		writeGRPCError(w, h.log, err)
		return
	}
	out := listCardsResponseJSON{Cards: make([]cardJSON, 0, len(resp.GetCards()))}
	for _, c := range resp.GetCards() {
		out.Cards = append(out.Cards, newCardJSON(c))
	}
	writeJSON(w, h.log, http.StatusOK, out)
}
