// Package webhookhandler exposes the plain HTTP listener that receives
// Airwallex webhook deliveries (POST /webhooks/airwallex), verifies
// their HMAC signature via internal/airwallex, and turns a
// "global_account.transaction.created" event into a MarkDepositReceived
// call against ledger-core — this is how a late deposit actually clears
// in the demo — publishing a normalized event onto internal/eventbus's
// Bus for StreamEvents subscribers (services/gateway) either way.
//
// This is a direct extraction of the gateway's former
// internal/httpserver/webhooks.go (docs/adr/0004,
// docs/SCALE-EXPANSION.md section 1): the logic is unchanged, only the
// package name and its dependency on webhook-ingestion's own
// internal/airwallex (signature verification only, no Issuing API
// client) and internal/eventbus (renamed from internal/activity) differ.
package webhookhandler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	ledgerpb "github.com/caelum0x/ration/pkg/pb/ledger"
	"github.com/caelum0x/ration/services/webhook-ingestion/internal/airwallex"
	"github.com/caelum0x/ration/services/webhook-ingestion/internal/eventbus"
)

// LedgerClient is the subset of ledgerclient.Client this package needs.
type LedgerClient interface {
	ListDeposits(ctx context.Context, tenantID string) ([]*ledgerpb.Deposit, error)
	MarkDepositReceived(ctx context.Context, req *ledgerpb.MarkDepositReceivedRequest) (*ledgerpb.Deposit, error)
}

// Handler serves Airwallex webhook deliveries.
type Handler struct {
	webhookSecret string
	ledger        LedgerClient
	bus           *eventbus.Bus
	log           *slog.Logger
}

// NewHandler constructs a webhook Handler. webhookSecret is the
// Airwallex-issued secret every delivery's "x-signature" header is
// verified against (AIRWALLEX_WEBHOOK_SECRET).
func NewHandler(webhookSecret string, ledger LedgerClient, bus *eventbus.Bus, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{webhookSecret: webhookSecret, ledger: ledger, bus: bus, log: log}
}

// Mux returns an http.Handler with the webhook route registered, ready
// to be wrapped in an http.Server by cmd/webhook-ingestion/main.go.
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
	if err := airwallex.VerifySignature(h.webhookSecret, body, timestamp, signature); err != nil {
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
//
// Deposits already marked received (ReceivedAtUnix != 0) are excluded
// from the candidate set entirely — not just preferred against. Without
// this, a tenant with two deposits of the same amount (e.g. two $500
// recurring deposits) where the older one already settled would have a
// genuinely new webhook for the newer deposit re-match the older,
// already-received one (it's still the "oldest" expected_at_unix in an
// amount-only comparison). MarkDepositReceived then no-ops against the
// already-Received deposit (ledger-core's own idempotency), silently
// discarding the real money: the new deposit never gets marked received
// and the tenant's available balance never reflects funds that actually
// arrived.
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
		if d.GetReceivedAtUnix() != 0 {
			// Already settled — never a valid candidate for a new
			// transaction event, regardless of amount or age.
			continue
		}
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
		h.bus.Publish(eventbus.NewEvent(payload.TenantID, "deposit",
			fmt.Sprintf("deposit %s received via webhook, amount %d minor units", updated.GetId(), payload.AmountMinorUnits)))
	}
	return nil
}
