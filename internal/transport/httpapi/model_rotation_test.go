package httpapi_test

import (
	"context"
	"errors"
	"github.com/hjhsamuel/cagent/internal/transport/httpapi"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRotationHTTPAuthorization(t *testing.T) {
	cfg := settings()
	cfg.DiagnosticsToken = strings.Repeat("a", 32)
	calls := 0
	fail := false
	handler, err := httpapi.NewWithKeyRotation(&stubService{}, &stubEvents{}, cfg, "agent", func(context.Context) (string, error) {
		calls++
		if fail {
			return "", errors.New("private-secret")
		}
		return "v2", nil
	}, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range []string{"", "Bearer wrong", "Bearer " + token(t)} {
		req := httptest.NewRequest(http.MethodPost, "/debug/model-keys/rotate", nil)
		req.Header.Set("Authorization", auth)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != 401 || calls != 0 {
			t.Fatal("unauthorized rotation")
		}
	}
	for _, want := range []int{200, 500} {
		req := httptest.NewRequest(http.MethodPost, "/debug/model-keys/rotate", nil)
		req.Header.Set("Authorization", "Bearer "+cfg.DiagnosticsToken)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != want || strings.Contains(response.Body.String(), "private-secret") {
			t.Fatal("invalid rotation response", response.Code)
		}
		fail = true
	}
}
