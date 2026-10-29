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

// handleTransactionCreated correlates an incoming Airwallex transaction
// to one specific expected deposit and marks it received.
//
// Matching is two-tiered:
//
//  1. Exact correlation-key match: if payload.Reference is non-empty,
//     prefer the (sole) not-yet-received deposit for this tenant whose
//     own ExternalRef equals it exactly AND whose expected amount
//     equals the transaction's own amount — set back when that deposit
//     was first recorded (gateway's SimulateDeposit, or any
//     RecordDeposit caller that assigns a reference it told the tenant
//     to use on their wire transfer). The amount half of this check is
//     required, not optional: MarkDepositReceived always credits the
//     matched deposit's full pre-recorded expected amount (it has no
//     "amount actually transferred" field to credit instead), so a
//     reference match on its own would let any real transaction amount
//     — including a short transfer — still mark the full expected
//     amount received. With both halves required, this is a correct
//     identification, not a heuristic: no two concurrently-outstanding
//     deposits are ever given the same reference, so a match cannot
//     misattribute funds regardless of how many deposits of the same
//     amount a tenant has outstanding at once.
//  2. Amount heuristic fallback: (tenant, amount, not-yet-received,
//     oldest) — used only when no reference is present on the
//     transaction, or no deposit's ExternalRef matches it (e.g. an
//     older/pre-migration deposit recorded before a reference was
//     assigned). This remains the same conservative "when in doubt about
//     which of two same-amount deposits a transaction belongs to, guess
//     oldest-first" behavior as before this field existed, and inherits
//     the exact misattribution risk the heuristic alone cannot close:
//     two outstanding same-amount deposits for one tenant, neither
//     carrying a reference the transaction also carries.
//
// Either tier: if nothing matches, it logs and returns success rather
// than guessing.
//
// Deposits already marked received (ReceivedAtUnix != 0) are excluded
// from the candidate set entirely in both tiers — not just preferred
// against. Without this, a tenant with two deposits of the same amount
// (e.g. two $500 recurring deposits) where the older one already
// settled would have a genuinely new webhook for the newer deposit
// re-match the older, already-received one (it's still the "oldest"
// expected_at_unix in an amount-only comparison). MarkDepositReceived
// then no-ops against the already-Received deposit (ledger-core's own
// idempotency), silently discarding the real money: the new deposit
// never gets marked received and the tenant's available balance never
// reflects funds that actually arrived.
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

	match := matchDepositByReference(deposits, payload.Reference, payload.AmountMinorUnits)
	matchedBy := "reference"
	if match == nil {
		match = matchDepositByAmountHeuristic(deposits, payload.AmountMinorUnits)
		matchedBy = "amount_heuristic"
	}
	if match == nil {
		h.log.Warn("webhook: no matching expected deposit found, skipping",
			"tenant_id", payload.TenantID, "amount_minor_units", payload.AmountMinorUnits, "reference", payload.Reference)
		return nil
	}
	h.log.Info("webhook: matched transaction to expected deposit",
		"tenant_id", payload.TenantID, "deposit_id", match.GetId(), "matched_by", matchedBy)

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
