package airwallex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Live is a real HTTP client against the Airwallex API (sandbox or
// production base URL). It implements token exchange
// (POST /api/v1/authentication/login with the client ID / API key pair),
// caches the bearer token until shortly before it expires, and talks to
// the Issuing and Global Accounts endpoints documented in
// docs/AIRWALLEX-SETUP.md.
//
// Sandbox credentials are pending organizer approval (see
// docs/AIRWALLEX-SETUP.md) — this client is written against Airwallex's
// published API shape but has not yet been exercised against a live
// sandbox. AIRWALLEX_MODE=mock (internal/airwallex/mock.go) is the
// default and what the demo runs against today.
type Live struct {
	baseURL    string
	clientID   string
	apiKey     string
	httpClient *http.Client

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
}

// NewLive constructs a Live client. baseURL, clientID and apiKey must be
// non-empty; config.Load already enforces this when AIRWALLEX_MODE=live.
func NewLive(baseURL, clientID, apiKey string) *Live {
	return &Live{
		baseURL:    baseURL,
		clientID:   clientID,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

type loginResponse struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"` // RFC3339
}

// authenticate returns a valid bearer token, refreshing it via the
// authentication/login endpoint if the cached one is missing or about to
// expire. Airwallex tokens are short-lived (typically ~30 minutes); we
// refresh 60s early to avoid racing expiry mid-request.
func (l *Live) authenticate(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.token != "" && time.Now().Before(l.tokenExpiry.Add(-60*time.Second)) {
		return l.token, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/api/v1/authentication/login", nil)
	if err != nil {
		return "", fmt.Errorf("airwallex: build login request: %w", err)
	}
	req.Header.Set("x-client-id", l.clientID)
	req.Header.Set("x-api-key", l.apiKey)

	resp, err := l.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("airwallex: login request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("airwallex: read login response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("airwallex: login failed with status %d: %s", resp.StatusCode, string(body))
	}

	var lr loginResponse
	if err := json.Unmarshal(body, &lr); err != nil {
		return "", fmt.Errorf("airwallex: malformed login response: %w", err)
	}
	if lr.Token == "" {
		return "", fmt.Errorf("airwallex: login response missing token")
	}
	expiry := time.Now().Add(30 * time.Minute)
	if lr.ExpiresAt != "" {
		if parsed, err := time.Parse(time.RFC3339, lr.ExpiresAt); err == nil {
			expiry = parsed
		}
	}

	l.token = lr.Token
	l.tokenExpiry = expiry
	return l.token, nil
}

// doJSON issues an authenticated JSON request and decodes a JSON
// response into out (if out is non-nil).
func (l *Live) doJSON(ctx context.Context, method, path string, reqBody any, out any) error {
	token, err := l.authenticate(ctx)
	if err != nil {
		return err
	}

	var bodyReader io.Reader
	if reqBody != nil {
		encoded, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("airwallex: encode request body: %w", err)
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, l.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("airwallex: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := l.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("airwallex: request to %s failed: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("airwallex: read response from %s: %w", path, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("airwallex: %s returned status %d: %s", path, resp.StatusCode, string(respBody))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("airwallex: decode response from %s: %w", path, err)
		}
	}
	return nil
}

type issueCardRequest struct {
	RequestID       string `json:"request_id"`
	CardholderName  string `json:"cardholder_name"`
	LimitMinorUnits int64  `json:"limit_minor_units"`
	LimitInterval   string `json:"limit_interval"`
	Currency        string `json:"currency"`
}

type cardResponse struct {
	CardID          string `json:"card_id"`
	CardholderName  string `json:"cardholder_name"`
	Status          string `json:"status"`
	LimitMinorUnits int64  `json:"limit_minor_units"`
	LimitInterval   string `json:"limit_interval"`
	Currency        string `json:"currency"`
	CreatedAt       string `json:"created_at"` // RFC3339
}

func (c cardResponse) toCard() *Card {
	createdAt := time.Now()
	if c.CreatedAt != "" {
		if parsed, err := time.Parse(time.RFC3339, c.CreatedAt); err == nil {
			createdAt = parsed
		}
	}
	return &Card{
		ID:              c.CardID,
		CardholderName:  c.CardholderName,
		Status:          CardStatus(c.Status),
		LimitMinorUnits: c.LimitMinorUnits,
		LimitInterval:   c.LimitInterval,
		Currency:        c.Currency,
		CreatedAt:       createdAt,
	}
}

// IssueCard calls POST /api/v1/issuing/cards/create.
func (l *Live) IssueCard(ctx context.Context, params IssueCardParams) (*Card, error) {
	if params.CardholderName == "" {
		return nil, fmt.Errorf("airwallex: cardholder_name is required")
	}
	reqBody := issueCardRequest{
		RequestID:       fmt.Sprintf("ration-%s-%d", params.TenantID, time.Now().UnixNano()),
		CardholderName:  params.CardholderName,
		LimitMinorUnits: params.LimitMinorUnits,
		LimitInterval:   params.LimitInterval,
		Currency:        params.Currency,
	}
	var out cardResponse
	if err := l.doJSON(ctx, http.MethodPost, "/api/v1/issuing/cards/create", reqBody, &out); err != nil {
		return nil, err
	}
	return out.toCard(), nil
}

// GetCard calls GET /api/v1/issuing/cards/{id}.
func (l *Live) GetCard(ctx context.Context, cardID string) (*Card, error) {
	var out cardResponse
	path := "/api/v1/issuing/cards/" + url.PathEscape(cardID)
	if err := l.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.toCard(), nil
}

type setCardControlsRequest struct {
	LimitMinorUnits             int64    `json:"limit_minor_units"`
	LimitInterval               string   `json:"limit_interval,omitempty"`
	AllowedCategories           []string `json:"allowed_categories,omitempty"`
	PerTransactionCapMinorUnits int64    `json:"per_transaction_cap_minor_units,omitempty"`
}

// SetCardControls calls POST /api/v1/issuing/cards/{id}/update.
func (l *Live) SetCardControls(ctx context.Context, cardID string, controls CardControls) (*Card, error) {
	reqBody := setCardControlsRequest{
		LimitMinorUnits:             controls.LimitMinorUnits,
		LimitInterval:               controls.LimitInterval,
		AllowedCategories:           controls.AllowedCategories,
		PerTransactionCapMinorUnits: controls.PerTransactionCapMinorUnits,
	}
	var out cardResponse
	path := "/api/v1/issuing/cards/" + url.PathEscape(cardID) + "/update"
	if err := l.doJSON(ctx, http.MethodPost, path, reqBody, &out); err != nil {
		return nil, err
	}
	return out.toCard(), nil
}

// CloseCard calls POST /api/v1/issuing/cards/{id}/close.
func (l *Live) CloseCard(ctx context.Context, cardID string) (*Card, error) {
	var out cardResponse
	path := "/api/v1/issuing/cards/" + url.PathEscape(cardID) + "/close"
	if err := l.doJSON(ctx, http.MethodPost, path, nil, &out); err != nil {
		return nil, err
	}
	return out.toCard(), nil
}

type transactionResponse struct {
	ID               string `json:"id"`
	AmountMinorUnits int64  `json:"amount_minor_units"`
	Currency         string `json:"currency"`
	Description      string `json:"description"`
	OccurredAt       string `json:"occurred_at"` // RFC3339
}

type listTransactionsResponse struct {
	Items []transactionResponse `json:"items"`
}

// ListGlobalAccountTransactions calls
// GET /api/v1/global_accounts/{tenant_id}/transactions, optionally scoped
// by a "since" query parameter.
func (l *Live) ListGlobalAccountTransactions(ctx context.Context, params ListGlobalAccountTransactionsParams) ([]Transaction, error) {
	path := "/api/v1/global_accounts/" + url.PathEscape(params.TenantID) + "/transactions"
	if !params.Since.IsZero() {
		path += "?since=" + url.QueryEscape(strconv.FormatInt(params.Since.Unix(), 10))
	}
	var out listTransactionsResponse
	if err := l.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	txs := make([]Transaction, 0, len(out.Items))
	for _, item := range out.Items {
		occurred := time.Now()
		if item.OccurredAt != "" {
			if parsed, err := time.Parse(time.RFC3339, item.OccurredAt); err == nil {
				occurred = parsed
			}
		}
		txs = append(txs, Transaction{
			ID:               item.ID,
			AmountMinorUnits: item.AmountMinorUnits,
			Currency:         item.Currency,
			Description:      item.Description,
			OccurredAt:       occurred,
		})
	}
	return txs, nil
}

var _ Client = (*Live)(nil)
