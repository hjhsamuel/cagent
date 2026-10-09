package httpapi_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"github.com/hjhsamuel/cagent/internal/transport/httpapi"
)

type adminModels struct {
	catalog *config.ModelCatalog
	fail    bool
	calls   int
}

func (m *adminModels) List(context.Context) ([]config.ModelView, error) {
	m.calls++
	return m.catalog.ListModelConfigurations(), nil
}
func (m *adminModels) Get(_ context.Context, id string) (config.ModelView, error) {
	m.calls++
	return m.catalog.GetModelConfiguration(id)
}
func (m *adminModels) Put(_ context.Context, id string, input config.ModelInput) (config.ModelView, error) {
	m.calls++
	return m.catalog.PutModelConfiguration(id, input, nil, func(*schema.Model, *schema.Model) error {
		if m.fail {
			return apperrors.ErrConflict
		}
		return nil
	})
}
func (m *adminModels) Delete(_ context.Context, id string) error {
	m.calls++
	return m.catalog.DeleteModelConfiguration(id, func(*schema.Model, *schema.Model) error { return nil })
}

const modelBody = `{"model":"vendor-model","provider":"GLM","base_url":"https://example.invalid/v1","api_keys":[{"id":"key-1","value":"supplier-secret","weight":1}],"config":{"window_tokens":32768,"request_timeout":"2s"}}`

func TestModelManagementHTTP(t *testing.T) {
	cfg := settings()
	catalog, _ := config.NewModelCatalog(nil, "agent", config.NewDeferredKeyring(config.ModelEncryption{Keys: map[string]string{"v1": "MDEyMzQ1Njc4OWFiY2RlZg=="}}))
	models := &adminModels{catalog: catalog}
	handler, err := httpapi.NewWithModelManagement(&stubService{}, &stubEvents{}, cfg, "agent", models, nil, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body, auth string, want int) string {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		for _, secret := range []string{"supplier-secret", "ciphertext", "nonce", "token_encoding", "max_tokens_field", "output_tokens"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("secret exposed")
			}
		}
		return w.Body.String()
	}
	auth := ""
	request("GET", "/debug/models", "", auth, 200)
	request("GET", "/debug/models/model", "", auth, 404)
	request("PUT", "/debug/models/model", modelBody, auth, 200)
	if response := request("GET", "/debug/models/model", "", auth, 200); !strings.Contains(response, `"id":"key-1"`) {
		t.Fatal("missing key reference")
	}
	request("PUT", "/debug/models/model", strings.Replace(modelBody, `"value":"supplier-secret",`, "", 1), auth, 200)
	for _, body := range []string{"", "{", modelBody + `{}`, strings.Replace(modelBody, `"model":`, `"unknown":1,"model":`, 1)} {
		request("PUT", "/debug/models/model", body, auth, 400)
	}
	for _, removed := range []string{`"token_encoding":"o200k_base",`, `"max_tokens_field":"max_tokens",`, `"output_tokens":2048,`} {
		request("PUT", "/debug/models/model", strings.Replace(modelBody, `"config":{`, `"config":{`+removed, 1), auth, 400)
	}
	models.fail = true
	request("PUT", "/debug/models/model", modelBody, auth, 409)
	request("DELETE", "/debug/models/model", "", auth, 204)
	request("DELETE", "/debug/models/model", "", auth, 404)
	cfg.MaxBodyBytes = 10
	handler, _ = httpapi.NewWithModelManagement(&stubService{}, &stubEvents{}, cfg, "agent", models, nil, func(context.Context) bool { return true })
	request("PUT", "/debug/models/model", modelBody, auth, 413)
	handler, _ = httpapi.NewWithModelManagement(&stubService{}, &stubEvents{}, cfg, "agent", nil, nil, func(context.Context) bool { return true })
	request("GET", "/debug/models", "", auth, 404)
}
