package airwallex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newTestLiveServer builds an httptest.Server that plays the role of the
// Airwallex API closely enough to exercise Live's request building,
// auth-token handling and response decoding end-to-end over real HTTP.
func newTestLiveServer(t *testing.T, loginCalls *int) *httptest.Server {
	t.Helper()
	cards := map[string]*cardResponse{}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v1/authentication/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-client-id") == "" || r.Header.Get("x-api-key") == "" {
			http.Error(w, "missing credentials", http.StatusUnauthorized)
			return
		}
		*loginCalls++
		_ = json.NewEncoder(w).Encode(loginResponse{
			Token:     "test-token",
			ExpiresAt: time.Now().Add(30 * time.Minute).Format(time.RFC3339),
		})
	})

	mux.HandleFunc("/api/v1/issuing/cards/create", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req issueCardRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		resp := &cardResponse{
			CardID:          "aw_card_live_1",
			CardholderName:  req.CardholderName,
			Status:          "ACTIVE",
			LimitMinorUnits: req.LimitMinorUnits,
			LimitInterval:   req.LimitInterval,
			Currency:        req.Currency,
			CreatedAt:       time.Now().Format(time.RFC3339),
		}
		cards[resp.CardID] = resp
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/api/v1/issuing/cards/aw_card_live_1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var req setCardControlsRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			c := cards["aw_card_live_1"]
			c.LimitMinorUnits = req.LimitMinorUnits
			_ = json.NewEncoder(w).Encode(c)
			return
		}
		if c, ok := cards["aw_card_live_1"]; ok {
			_ = json.NewEncoder(w).Encode(c)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	mux.HandleFunc("/api/v1/issuing/cards/aw_card_live_1/update", func(w http.ResponseWriter, r *http.Request) {
		var req setCardControlsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		c := cards["aw_card_live_1"]
		c.LimitMinorUnits = req.LimitMinorUnits
		_ = json.NewEncoder(w).Encode(c)
	})
	mux.HandleFunc("/api/v1/issuing/cards/does-not-exist", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	mux.HandleFunc("/api/v1/global_accounts/tenant_1/transactions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(listTransactionsResponse{
			Items: []transactionResponse{
				{ID: "tx_1", AmountMinorUnits: 500, Currency: "USD", OccurredAt: time.Now().Format(time.RFC3339)},
			},
		})
	})

	return httptest.NewServer(mux)
}

func TestLive_IssueCardGetCardSetControls(t *testing.T) {
	loginCalls := 0
	srv := newTestLiveServer(t, &loginCalls)
	defer srv.Close()

	live := NewLive(srv.URL, "client-1", "key-1")

	card, err := live.IssueCard(context.Background(), IssueCardParams{
		TenantID:        "tenant_1",
		CardholderName:  "Ada Lovelace",
		LimitMinorUnits: 5000,
		LimitInterval:   "daily",
		Currency:        "USD",
	})
	if err != nil {
		t.Fatalf("IssueCard() error = %v", err)
	}
	if card.ID != "aw_card_live_1" {
		t.Errorf("ID = %q, want aw_card_live_1", card.ID)
	}
	if loginCalls != 1 {
		t.Errorf("loginCalls = %d, want 1 (token should be cached across calls)", loginCalls)
	}

	got, err := live.GetCard(context.Background(), card.ID)
	if err != nil {
		t.Fatalf("GetCard() error = %v", err)
	}
	if got.CardholderName != "Ada Lovelace" {
		t.Errorf("CardholderName = %q, want Ada Lovelace", got.CardholderName)
	}
	// Second real API call should reuse the cached token, not log in again.
	if loginCalls != 1 {
		t.Errorf("loginCalls = %d, want 1 (token should still be cached)", loginCalls)
	}

	updated, err := live.SetCardControls(context.Background(), card.ID, CardControls{LimitMinorUnits: 2000})
	if err != nil {
		t.Fatalf("SetCardControls() error = %v", err)
	}
	if updated.LimitMinorUnits != 2000 {
		t.Errorf("LimitMinorUnits = %d, want 2000", updated.LimitMinorUnits)
	}
}

func TestLive_GetCard_NotFound(t *testing.T) {
	loginCalls := 0
	srv := newTestLiveServer(t, &loginCalls)
	defer srv.Close()

	live := NewLive(srv.URL, "client-1", "key-1")
	_, err := live.GetCard(context.Background(), "does-not-exist")
	if err != ErrNotFound {
		t.Fatalf("GetCard() error = %v, want %v", err, ErrNotFound)
	}
}

func TestLive_ListGlobalAccountTransactions(t *testing.T) {
	loginCalls := 0
	srv := newTestLiveServer(t, &loginCalls)
	defer srv.Close()

	live := NewLive(srv.URL, "client-1", "key-1")
	txs, err := live.ListGlobalAccountTransactions(context.Background(), ListGlobalAccountTransactionsParams{TenantID: "tenant_1"})
	if err != nil {
		t.Fatalf("ListGlobalAccountTransactions() error = %v", err)
	}
	if len(txs) != 1 || txs[0].ID != "tx_1" {
		t.Errorf("txs = %+v, want one tx_1", txs)
	}
}

func TestLive_Authenticate_MissingCredentials(t *testing.T) {
	loginCalls := 0
	srv := newTestLiveServer(t, &loginCalls)
	defer srv.Close()

	live := NewLive(srv.URL, "", "")
	_, err := live.IssueCard(context.Background(), IssueCardParams{CardholderName: "X"})
	if err == nil {
		t.Fatal("IssueCard() expected error for missing credentials, got nil")
	}
}

