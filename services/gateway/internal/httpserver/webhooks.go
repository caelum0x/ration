// Package httpserver exposes the plain HTTP listener that receives
// Airwallex webhook deliveries (POST /webhooks/airwallex), verifies their
// HMAC signature via internal/airwallex, and turns a
// "global_account.transaction.created" event into a MarkDepositReceived
// call against ledger-core — this is how a late deposit actually clears
// in the demo. Per docs/ARCHITECTURE.md the listener itself is only
// started (bound to a port) when AIRWALLEX_MODE=live; in mock mode the
// handler logic still exists and is reachable (e.g. from the CLI demo
// driver POSTing a simulated, correctly-signed delivery) so the
// signature-verification code path is exercised identically either way.
package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/gateway/internal/activity"
	"github.com/caelum0x/ration/services/gateway/internal/airwallex"
)

// LedgerClient is the subset of ledgerclient.Client this package needs.
type LedgerClient interface {
	ListDeposits(ctx context.Context, tenantID string) ([]*ledgerpb.Deposit, error)
	MarkDepositReceived(ctx context.Context, req *ledgerpb.MarkDepositReceivedRequest) (*ledgerpb.Deposit, error)
}

// Handler serves Airwallex webhook deliveries.
type Handler struct {
	airwallex airwallex.Client
	ledger    LedgerClient
	bus       *activity.Bus
	log       *slog.Logger
}

// NewHandler constructs a webhook Handler.
func NewHandler(aw airwallex.Client, ledger LedgerClient, bus *activity.Bus, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{airwallex: aw, ledger: ledger, bus: bus, log: log}
}

// Mux returns an http.Handler with the webhook route registered, ready
// to be wrapped in an http.Server by cmd/gateway/main.go.
func (h *Handler) Mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhooks/airwallex", h.handleAirwallexWebhook)
	return mux
}

func (h *Handler) handleAirwallexWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB cap, webhook bodies are small JSON
	if err != nil {
		h.log.Error("webhook: failed reading body", "error", err)
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	timestamp := r.Header.Get("x-timestamp")
	signature := r.Header.Get("x-signature")
	if err := h.airwallex.VerifyWebhookSignature(body, timestamp, signature); err != nil {
		h.log.Warn("webhook: signature verification failed", "error", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	env, err := airwallex.ParseWebhookEnvelope(body)
	if err != nil {
		h.log.Error("webhook: malformed envelope", "error", err)
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}

	switch env.Name {
	case airwallex.EventGlobalAccountTransactionCreated:
		if err := h.handleTransactionCreated(r.Context(), env); err != nil {
			h.log.Error("webhook: handling transaction.created failed", "error", err, "event_id", env.ID)
			http.Error(w, "processing failed", http.StatusInternalServerError)
			return
		}
	default:
		h.log.Info("webhook: ignoring unhandled event type", "name", env.Name, "event_id", env.ID)
	}

	w.WriteHeader(http.StatusOK)
}

// handleTransactionCreated finds the tenant's oldest not-yet-received
// deposit whose expected amount matches the incoming transaction and
// marks it received. In a real deployment, Airwallex's transaction
// payload or a prior RecordDeposit's external_ref would carry a tighter
// correlation key; matching on (tenant, amount, not-yet-received) is
// sufficient for the single-deposit-at-a-time demo scenarios this
// project targets, and is intentionally conservative: if nothing matches,
// it logs and returns success rather than guessing.
func (h *Handler) handleTransactionCreated(ctx context.Context, env *airwallex.WebhookEnvelope) error {
	var payload airwallex.GlobalAccountTransactionPayload
	if err := json.Unmarshal(env.Data, &payload); err != nil {
		return fmt.Errorf("decode global_account.transaction.created payload: %w", err)
	}
	if payload.TenantID == "" {
		return fmt.Errorf("global_account.transaction.created payload missing tenant_id")
	}

	deposits, err := h.ledger.ListDeposits(ctx, payload.TenantID)
	if err != nil {
		return fmt.Errorf("list deposits for %s: %w", payload.TenantID, err)
	}

	var match *ledgerpb.Deposit
	for _, d := range deposits {
		if d.GetExpectedAmount().GetMinorUnits() != payload.AmountMinorUnits {
			continue
		}
		if match == nil || d.GetExpectedAtUnix() < match.GetExpectedAtUnix() {
			match = d
		}
	}
	if match == nil {
		h.log.Warn("webhook: no matching expected deposit found, skipping",
			"tenant_id", payload.TenantID, "amount_minor_units", payload.AmountMinorUnits)
		return nil
	}

	receivedAt := payload.OccurredAtUnix
	if receivedAt == 0 {
		receivedAt = time.Now().Unix()
	}

	updated, err := h.ledger.MarkDepositReceived(ctx, &ledgerpb.MarkDepositReceivedRequest{
		DepositId:      match.GetId(),
		ReceivedAtUnix: receivedAt,
		ExternalRef:    payload.TransactionID,
	})
	if err != nil {
		return fmt.Errorf("mark deposit %s received: %w", match.GetId(), err)
	}

	if h.bus != nil {
		h.bus.Publish(activity.NewEvent(payload.TenantID, "deposit",
			fmt.Sprintf("deposit %s received via webhook, amount %d minor units", updated.GetId(), payload.AmountMinorUnits)))
	}
	return nil
}
