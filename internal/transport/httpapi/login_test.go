package httpapi_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/transport/httpapi"
)

func TestAPILoginGeneratesRandomScopedJWT(t *testing.T) {
	cfg := settings()
	cfg.Login.TokenTTL = 30 * time.Minute
	s := &stubService{}
	h := handler(t, s, stubEvents{}, cfg)
	for _, auth := range []string{"", "wrong", token(t)} {
		w := request(h, "POST", "/api/v1/auth/login", "", auth)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"token":`) {
			t.Fatal("login requires existing credentials", w.Code)
		}
	}
	seen := map[string]bool{}
	var first httpapi.Claims
	var firstToken string
	// Both empty bodies and ignored account/identity fields must issue tokens.
	for i, body := range []string{"", `{}`, `{"username":"wrong","password":"wrong","user_id":"attacker","tenant_id":"attacker"}`} {
		before := time.Now()
		w := request(h, "POST", "/api/v1/auth/login", body, "")
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("token generation failed", w.Code)
		}
		var response struct {
			Token     string `json:"token"`
			TokenType string `json:"token_type"`
			ExpiresIn int64  `json:"expires_in"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Token == "" || response.TokenType != "Bearer" || response.ExpiresIn != 1800 {
			t.Fatal("invalid token response")
		}
		var claims httpapi.Claims
		parsed, err := jwt.ParseWithClaims(response.Token, &claims, func(*jwt.Token) (any, error) { return []byte(cfg.JWT.Secret), nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired())
		if err != nil || !parsed.Valid {
			t.Fatal("invalid JWT", err)
		}
		for _, id := range []string{claims.Subject, claims.TenantID, claims.ID} {
			parsedID, err := uuid.Parse(id)
			if err != nil || parsedID.Version() != 4 || seen[id] {
				t.Fatal("identity is invalid or reused", id)
			}
			seen[id] = true
		}
		if claims.IssuedAt == nil || claims.ExpiresAt.Time.Before(before.Add(cfg.Login.TokenTTL-time.Second)) || claims.ExpiresAt.Time.After(time.Now().Add(cfg.Login.TokenTTL)) {
			t.Fatal("invalid token lifetime")
		}
		if _, err := jwt.ParseWithClaims(response.Token, &httpapi.Claims{}, func(*jwt.Token) (any, error) { return []byte(cfg.JWT.Secret), nil }, jwt.WithTimeFunc(func() time.Time { return claims.ExpiresAt.Time.Add(time.Second) })); err == nil {
			t.Fatal("token does not expire")
		}
		w = request(h, "POST", "/api/v1/sessions", `{}`, response.Token)
		if w.Code != 201 || s.scope != (domain.Scope{UserID: claims.Subject, TenantID: claims.TenantID}) {
			t.Fatal("token scope not used", w.Code)
		}
		if i == 0 {
			first, firstToken = claims, response.Token
		} else if claims.Subject == first.Subject || claims.TenantID == first.TenantID || response.Token == firstToken {
			t.Fatal("login reused identity")
		}
	}
	w := request(h, "GET", "/debug/metrics", "", firstToken)
	if w.Code != 200 {
		t.Fatal("diagnostics requires authentication", w.Code)
	}
	w = request(handler(t, s, stubEvents{}, settings()), "POST", "/api/v1/auth/login", "", "")
	if w.Code != 200 {
		t.Fatal("API login requires authentication", w.Code)
	}
	w = request(h, "POST", "/debug/auth/login", "", "")
	if w.Code != 404 {
		t.Fatal("old login route still registered", w.Code)
	}
	w = request(h, "POST", "/api/v1/sessions", `{}`, "")
	if w.Code != 401 {
		t.Fatal("business API lost JWT protection", w.Code)
	}
}
