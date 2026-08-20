package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// daraja.go covers Safaricom Daraja C2B/STK Push (deposits) only.
// B2C (withdrawal payouts) is intentionally NOT implemented — withdrawals
// are created as 'pending' Postgres records by wallet.go and stop there.
// Wire up B2C separately when ready; nothing else in this file assumes it exists.

type DarajaClient struct {
	env            string // "sandbox" | "production"
	consumerKey    string
	consumerSecret string
	shortcode      string
	passkey        string
	callbackURL    string
	httpClient     *http.Client

	mu          sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

func NewDarajaClient(cfg Config) *DarajaClient {
	return &DarajaClient{
		env:            cfg.DarajaEnv,
		consumerKey:    cfg.DarajaConsumerKey,
		consumerSecret: cfg.DarajaConsumerSecret,
		shortcode:      cfg.DarajaShortcode,
		passkey:        cfg.DarajaPasskey,
		callbackURL:    cfg.DarajaCallbackURL,
		httpClient:     &http.Client{Timeout: 15 * time.Second},
	}
}

func (d *DarajaClient) baseURL() string {
	if d.env == "production" {
		return "https://api.safaricom.co.ke"
	}
	return "https://sandbox.safaricom.co.ke"
}

type oauthResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   string `json:"expires_in"`
}

// getAccessToken fetches (and caches) an OAuth token via HTTP Basic auth
// against Daraja's /oauth/v1/generate endpoint. Cached for its lifetime
// minus a safety margin so we don't fetch a new one on every deposit.
func (d *DarajaClient) getAccessToken(ctx context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.cachedToken != "" && time.Now().Before(d.tokenExpiry) {
		return d.cachedToken, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		d.baseURL()+"/oauth/v1/generate?grant_type=client_credentials", nil)
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(d.consumerKey, d.consumerSecret)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("daraja oauth request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("daraja oauth failed (%d): %s", resp.StatusCode, string(raw))
	}

	var out oauthResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("unexpected daraja oauth response: %s", string(raw))
	}

	d.cachedToken = out.AccessToken
	d.tokenExpiry = time.Now().Add(50 * time.Minute) // tokens are valid ~1hr
	return out.AccessToken, nil
}

type stkPushRequest struct {
	BusinessShortCode string `json:"BusinessShortCode"`
	Password          string `json:"Password"`
	Timestamp         string `json:"Timestamp"`
	TransactionType   string `json:"TransactionType"`
	Amount            string `json:"Amount"`
	PartyA            string `json:"PartyA"`
	PartyB            string `json:"PartyB"`
	PhoneNumber       string `json:"PhoneNumber"`
	CallBackURL       string `json:"CallBackURL"`
	AccountReference  string `json:"AccountReference"`
	TransactionDesc   string `json:"TransactionDesc"`
}

type STKPushResponse struct {
	MerchantRequestID   string `json:"MerchantRequestID"`
	CheckoutRequestID   string `json:"CheckoutRequestID"`
	ResponseCode        string `json:"ResponseCode"`
	ResponseDescription string `json:"ResponseDescription"`
	CustomerMessage     string `json:"CustomerMessage"`
	ErrorMessage        string `json:"errorMessage"`
}

// InitiateSTKPush triggers the "Lipa na M-Pesa" prompt on the user's phone.
// accountReference should be the internal transaction id so the callback
// (see wallet.go's DarajaCallback) can be matched back to the pending deposit.
func (d *DarajaClient) InitiateSTKPush(ctx context.Context, phone string, amount float64, accountReference string) (*STKPushResponse, error) {
	token, err := d.getAccessToken(ctx)
	if err != nil {
		return nil, err
	}

	timestamp := time.Now().Format("20060102150405")
	password := base64.StdEncoding.EncodeToString(
		[]byte(d.shortcode + d.passkey + timestamp))

	body := stkPushRequest{
		BusinessShortCode: d.shortcode,
		Password:          password,
		Timestamp:         timestamp,
		TransactionType:   "CustomerPayBillOnline",
		Amount:            fmt.Sprintf("%.0f", amount),
		PartyA:            phone,
		PartyB:            d.shortcode,
		PhoneNumber:       phone,
		CallBackURL:       d.callbackURL,
		AccountReference:  accountReference,
		TransactionDesc:   "dotPesa deposit",
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		d.baseURL()+"/mpesa/stkpush/v1/processrequest", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("daraja stk push request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	var out STKPushResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unexpected daraja stk response: %s", string(raw))
	}
	if resp.StatusCode >= 400 || out.ResponseCode != "0" {
		msg := firstNonEmpty(out.ErrorMessage, out.ResponseDescription, "STK push rejected")
		return &out, fmt.Errorf("daraja: %s", msg)
	}
	return &out, nil
}

// ---- Callback payload shape (Safaricom's actual STK callback envelope) ----

type STKCallbackPayload struct {
	Body struct {
		StkCallback struct {
			MerchantRequestID string `json:"MerchantRequestID"`
			CheckoutRequestID string `json:"CheckoutRequestID"`
			ResultCode        int    `json:"ResultCode"`
			ResultDesc        string `json:"ResultDesc"`
			CallbackMetadata  struct {
				Item []struct {
					Name  string `json:"Name"`
					Value any    `json:"Value"`
				} `json:"Item"`
			} `json:"CallbackMetadata"`
		} `json:"stkCallback"`
	} `json:"Body"`
}

// ExtractReceipt pulls the MpesaReceiptNumber and account reference out of a
// successful callback's metadata items (Safaricom returns them as a flat
// name/value array, not a clean object).
func (p *STKCallbackPayload) ExtractReceipt() (receipt string, amount float64, ok bool) {
	cb := p.Body.StkCallback
	if cb.ResultCode != 0 {
		return "", 0, false
	}
	for _, item := range cb.CallbackMetadata.Item {
		switch item.Name {
		case "MpesaReceiptNumber":
			if s, ok := item.Value.(string); ok {
				receipt = s
			}
		case "Amount":
			switch v := item.Value.(type) {
			case float64:
				amount = v
			}
		}
	}
	return receipt, amount, receipt != ""
}
