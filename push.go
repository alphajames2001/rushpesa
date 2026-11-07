package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// push.go sends FCM data messages through the FCM HTTP v1 API.
//
// No Firebase SDK on purpose: the service-account JWT -> OAuth2 access token
// exchange is ~30 lines with golang-jwt (already a dependency), and it keeps
// go.mod/go.sum untouched.
//
// Credentials come from FIREBASE_CREDENTIALS_JSON (the whole service-account
// JSON as one env var). Unset means push is disabled and the server still
// starts. Never commit that JSON.

// errInvalidToken means FCM says the device token is dead; delete it.
var errInvalidToken = errors.New("fcm: device token is no longer valid")

type Pusher struct {
	projectID   string
	clientEmail string
	tokenURI    string
	key         *rsa.PrivateKey
	http        *http.Client

	mu          sync.Mutex
	accessToken string
	expiry      time.Time
}

// NewPusher returns (nil, nil) when FIREBASE_CREDENTIALS_JSON is not set.
func NewPusher() (*Pusher, error) {
	raw := strings.TrimSpace(os.Getenv("FIREBASE_CREDENTIALS_JSON"))
	if raw == "" {
		return nil, nil
	}
	var creds struct {
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal([]byte(raw), &creds); err != nil {
		return nil, fmt.Errorf("FIREBASE_CREDENTIALS_JSON is not valid JSON: %w", err)
	}
	if creds.ProjectID == "" || creds.ClientEmail == "" || creds.PrivateKey == "" {
		return nil, errors.New("FIREBASE_CREDENTIALS_JSON is missing project_id, client_email or private_key")
	}
	block, _ := pem.Decode([]byte(creds.PrivateKey))
	if block == nil {
		return nil, errors.New("FIREBASE_CREDENTIALS_JSON: private_key is not valid PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("FIREBASE_CREDENTIALS_JSON: parsing private_key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("FIREBASE_CREDENTIALS_JSON: private_key is not an RSA key")
	}
	tokenURI := creds.TokenURI
	if tokenURI == "" {
		tokenURI = "https://oauth2.googleapis.com/token"
	}
	return &Pusher{
		projectID:   creds.ProjectID,
		clientEmail: creds.ClientEmail,
		tokenURI:    tokenURI,
		key:         key,
		http:        &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// bearer returns a cached OAuth2 access token, refreshing it a minute early.
func (p *Pusher) bearer(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.accessToken != "" && time.Now().Before(p.expiry.Add(-time.Minute)) {
		return p.accessToken, nil
	}

	now := time.Now()
	assertion, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":   p.clientEmail,
		"scope": "https://www.googleapis.com/auth/firebase.messaging",
		"aud":   p.tokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}).SignedString(p.key)
	if err != nil {
		return "", fmt.Errorf("signing fcm jwt: %w", err)
	}

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("fcm token request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fcm token request returned %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.AccessToken == "" {
		return "", fmt.Errorf("unexpected fcm token response: %s", string(raw))
	}
	p.accessToken = out.AccessToken
	p.expiry = now.Add(time.Duration(out.ExpiresIn) * time.Second)
	return p.accessToken, nil
}

// Send delivers one data-only message. The keys must stay exactly title /
// body / withdrawalId: MpesaMessagingService reads message.data[...] and
// returns silently when "body" is missing. There is deliberately no
// "notification" block, otherwise Android would show it itself when the app
// is backgrounded and never call onMessageReceived.
func (p *Pusher) Send(ctx context.Context, deviceToken, title, body, id string) error {
	bearer, err := p.bearer(ctx)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"token": deviceToken,
			"data": map[string]string{
				"title":        title,
				"body":         body,
				"withdrawalId": id,
			},
			"android": map[string]any{"priority": "HIGH"},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", p.projectID),
		bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("fcm send failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		return nil
	}

	var e struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	// Only a 404 / UNREGISTERED means the token is dead. A 400 can equally be
	// a bad payload, so those are logged by the caller, not deleted.
	dead := resp.StatusCode == http.StatusNotFound || e.Error.Status == "NOT_FOUND"
	for _, d := range e.Error.Details {
		if d.ErrorCode == "UNREGISTERED" {
			dead = true
		}
	}
	if dead {
		return errInvalidToken
	}
	return fmt.Errorf("fcm send returned %d: %s", resp.StatusCode, string(raw))
}
