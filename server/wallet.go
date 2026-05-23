package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"padel-cli/api"
	"padel-cli/storage"
)

// walletCacheTTL is how long we trust a cached balance before showing it as
// stale in the UI. The probe locks a real court for ~10-15 minutes per call,
// so we cache aggressively and let the user refresh manually.
const walletCacheTTL = 60 * time.Minute

type walletStatus struct {
	Balance     string
	Currency    string
	WalletName  string
	LastChecked time.Time
	LastError   string
	Stale       bool
	Probing     bool
}

func (s *Server) cachedWalletStatus() walletStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.walletStatus
	if !status.LastChecked.IsZero() && time.Since(status.LastChecked) > walletCacheTTL {
		status.Stale = true
	}
	return status
}

func (s *Server) setWalletStatus(status walletStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.walletStatus = status
}

func (s *Server) handleWalletRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := s.refreshWalletBalance(r.Context())
	s.setWalletStatus(status)
	s.renderWalletFragment(w, status)
}

func (s *Server) renderWalletFragment(w http.ResponseWriter, status walletStatus) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmpl, ok := s.templates["dashboard"]
	if !ok {
		http.Error(w, "wallet fragment not found", http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, "wallet_fragment", status); err != nil {
		s.logger.Printf("wallet fragment render error: %v", err)
	}
}

func (s *Server) refreshWalletBalance(ctx context.Context) walletStatus {
	now := time.Now()
	creds, err := storage.LoadCredentials()
	if err != nil {
		return walletStatus{LastChecked: now, LastError: fmt.Sprintf("load credentials: %v", err)}
	}
	if creds == nil || creds.AccessToken == "" {
		return walletStatus{LastChecked: now, LastError: "not logged in — run `padel auth login`"}
	}

	client := api.NewClient()
	client.AccessToken = creds.AccessToken
	if creds.AccessTokenExpired(time.Now()) {
		if creds.RefreshToken == "" {
			return walletStatus{LastChecked: now, LastError: "access token expired and no refresh token — re-run `padel auth login`"}
		}
		refreshed, err := client.RefreshToken(ctx, creds.RefreshToken)
		if err != nil {
			return walletStatus{LastChecked: now, LastError: fmt.Sprintf("token refresh failed: %v", err)}
		}
		creds.AccessToken = refreshed.AccessToken
		creds.AccessTokenExpiration = refreshed.AccessTokenExpiration
		creds.RefreshToken = refreshed.RefreshToken
		creds.RefreshTokenExpiration = refreshed.RefreshTokenExpiration
		if err := storage.SaveCredentials(creds); err != nil {
			s.logger.Printf("wallet probe: save refreshed credentials: %v", err)
		}
		client.AccessToken = creds.AccessToken
	}

	venues, err := storage.LoadVenues()
	if err != nil {
		return walletStatus{LastChecked: now, LastError: fmt.Sprintf("load venues: %v", err)}
	}
	if len(venues) == 0 {
		return walletStatus{LastChecked: now, LastError: "no venues saved — add one with `padel venues add` first"}
	}

	venue := venues[0]
	tomorrow := time.Now().Add(24 * time.Hour)
	dayStart := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour).Add(-time.Second)

	availability, err := client.GetAvailability(ctx, venue.ID, dayStart, dayEnd)
	if err != nil {
		return walletStatus{LastChecked: now, LastError: fmt.Sprintf("availability lookup: %v", err)}
	}

	resourceID, slotStart, ok := pickWalletProbeSlot(availability)
	if !ok {
		return walletStatus{LastChecked: now, LastError: "no probe slot available at the saved venue — try later"}
	}

	intent, err := client.CreatePaymentIntent(ctx, api.PaymentIntentRequest{
		AllowedPaymentMethodTypes: []string{"MERCHANT_WALLET"},
		UserID:                    creds.UserID,
		Cart: api.PaymentIntentCart{
			RequestedItem: api.PaymentIntentItem{
				CartItemType:      "CUSTOMER_MATCH",
				CartItemVoucherID: nil,
				CartItemData: api.PaymentIntentItemData{
					SupportsSplitPayment: true,
					NumberOfPlayers:      4,
					TenantID:             venue.ID,
					ResourceID:           resourceID,
					Start:                slotStart,
					Duration:             60,
					MatchRegistrations: []api.MatchRegistration{
						{UserID: creds.UserID, PayNow: true},
					},
				},
			},
		},
	})
	if err != nil {
		return walletStatus{LastChecked: now, LastError: fmt.Sprintf("probe intent: %v", err)}
	}

	// We deliberately do not PATCH/confirm. The temporal lock on this slot
	// expires automatically in ~10-15 minutes.
	for _, raw := range intent.AvailablePaymentMethods {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if methodType, _ := entry["method_type"].(string); methodType != "MERCHANT_WALLET" {
			continue
		}
		data, _ := entry["data"].(map[string]any)
		if data == nil {
			continue
		}
		balance, _ := data["balance"].(string)
		walletName, _ := data["name"].(string)
		amount, currency := splitBalance(balance)
		return walletStatus{
			Balance:     amount,
			Currency:    currency,
			WalletName:  walletName,
			LastChecked: now,
		}
	}
	return walletStatus{LastChecked: now, LastError: "probe response had no MERCHANT_WALLET method — wallet may be unavailable"}
}

// pickWalletProbeSlot finds the latest-start 60-minute slot in the response.
// Latest-of-day is least likely to be one the user actually wants right now,
// minimising the social impact of the 10-15 minute temporal lock.
func pickWalletProbeSlot(resources []api.AvailabilityResource) (string, string, bool) {
	type candidate struct {
		resourceID string
		startTime  string
		startDate  string
	}
	var best *candidate
	for _, resource := range resources {
		for _, slot := range resource.Slots {
			if slot.Duration != 60 {
				continue
			}
			c := candidate{
				resourceID: resource.ResourceID,
				startTime:  slot.StartTime,
				startDate:  resource.StartDate,
			}
			if best == nil || c.startTime > best.startTime {
				c := c
				best = &c
			}
		}
	}
	if best == nil {
		return "", "", false
	}
	// Build the naive datetime Playtomic expects.
	dateOnly := best.startDate
	if len(dateOnly) >= 10 {
		dateOnly = dateOnly[:10]
	}
	timeOnly := best.startTime
	if len(timeOnly) >= 8 {
		timeOnly = timeOnly[:8]
	} else if len(timeOnly) == 5 {
		timeOnly = timeOnly + ":00"
	}
	return best.resourceID, fmt.Sprintf("%sT%s", dateOnly, timeOnly), true
}

func splitBalance(balance string) (string, string) {
	if balance == "" {
		return "", ""
	}
	// Playtomic returns balances like "117.5 AUD" — amount + space + currency.
	parts := strings.SplitN(balance, " ", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return balance, ""
}
