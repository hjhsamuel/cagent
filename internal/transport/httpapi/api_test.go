package httpapi_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hjhsamuel/cagent/internal/app"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"github.com/hjhsamuel/cagent/internal/transport/httpapi"
)

func settings() config.HTTP {
	cfg := config.Defaults().HTTP
	cfg.JWT = config.JWT{Secret: strings.Repeat("s", 32)}
	cfg.SSEHeartbeat = 10 * time.Millisecond
	cfg.WriteTimeout = 50 * time.Millisecond
	return cfg
}
func claims() httpapi.Claims {
	return httpapi.Claims{TenantID: "tenant", RegisteredClaims: jwt.RegisteredClaims{Subject: "user", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
}
func sign(t *testing.T, c httpapi.Claims, method jwt.SigningMethod, key string) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, c).SignedString([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func token(t *testing.T) string {
	return sign(t, claims(), jwt.SigningMethodHS256, settings().JWT.Secret)
}

type stubService struct {
	scope     domain.Scope
	start     app.StartRun
	err       error
	panics    bool
	cancelled atomic.Int32
}

func (s *stubService) CreateSession(_ context.Context, scope domain.Scope, agent string) (domain.Session, error) {
	if s.panics {
		panic("private panic")
	}
	s.scope = scope
	return domain.Session{ID: "session", AgentID: agent}, s.err
}

func (s *stubService) CreateSessionWithModel(ctx context.Context, scope domain.Scope, agent, modelID string) (domain.Session, error) {
	value, err := s.CreateSession(ctx, scope, agent)
	value.ModelID = modelID
	value.APIKeyID = "private-key-reference"
	return value, err
}
func (s *stubService) GetSession(_ context.Context, scope domain.Scope, id string) (domain.Session, error) {
	s.scope = scope
	return domain.Session{ID: id}, s.err
}
func (s *stubService) StartRun(_ context.Context, r app.StartRun) (domain.Run, error) {
	s.start = r
	return domain.Run{ID: "run", SessionID: r.SessionID, Status: domain.RunQueued}, s.err
}
func (s *stubService) GetRun(_ context.Context, scope domain.Scope, id string) (domain.Run, error) {
	s.scope = scope
	return domain.Run{ID: id}, s.err
}
func (s *stubService) CancelRun(context.Context, domain.Scope, string) error {
	s.cancelled.Add(1)
	return s.err
}

type stubEvents struct {
	preflight error
	follow    func(context.Context, int64, func(domain.Event) error) error
}

func (e stubEvents) CheckCursor(context.Context, domain.Scope, string, int64) error {
	return e.preflight
}
func (e stubEvents) Follow(ctx context.Context, _ domain.Scope, _ string, n int64, f func(domain.Event) error) error {
	if e.follow != nil {
		return e.follow(ctx, n, f)
	}
	return nil
}
func handler(t *testing.T, s app.Service, e httpapi.Events, cfg config.HTTP) http.Handler {
	t.Helper()
	h, err := httpapi.New(s, e, cfg, "configured-agent", func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func request(h http.Handler, method, path, body, bearer string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("X-User-ID", "attacker")
	req.Header.Set("X-Tenant-ID", "attacker")
	req.Header.Set("Idempotency-Key", "stable-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestCreateSessionModelSelection(t *testing.T) {
	for _, tt := range []struct {
		body, model string
		status      int
	}{
		{`{"model_id":"glm"}`, "glm", 201},
		{`{}`, "", 201},
		{"", "", 201},
		{`{"model_id":" "}`, "", 400},
		{`{"model_id":123}`, "", 400},
		{`{`, "", 400},
		{`{} {}`, "", 400},
	} {
		s := new(stubService)
		w := request(handler(t, s, stubEvents{}, settings()), "POST", "/api/v1/sessions", tt.body, token(t))
		if w.Code != tt.status || strings.Contains(w.Body.String(), "private-key-reference") || strings.Contains(w.Body.String(), "api_key") {
			t.Fatalf("body %q: %d %s", tt.body, w.Code, w.Body.String())
		}
		if tt.model != "" && !strings.Contains(w.Body.String(), `"model_id":"`+tt.model+`"`) {
			t.Fatal("selected model missing in DTO")
		}
	}
}

func TestJWTRejectsUntrustedIdentity(t *testing.T) {
	good := claims()
	cases := map[string]string{"missing": "", "malformed": "not-a-token", "wrong-key": sign(t, good, jwt.SigningMethodHS256, strings.Repeat("x", 32)), "wrong-algorithm": sign(t, good, jwt.SigningMethodHS384, settings().JWT.Secret)}
	for _, name := range []string{"expired", "missing-exp", "future-nbf", "missing-tenant", "missing-sub"} {
		c := claims()
		switch name {
		case "expired":
			c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
		case "missing-exp":
			c.ExpiresAt = nil
		case "future-nbf":
			c.NotBefore = jwt.NewNumericDate(time.Now().Add(time.Hour))
		case "missing-tenant":
			c.TenantID = ""
		case "missing-sub":
			c.Subject = ""
		}
		cases[name] = sign(t, c, jwt.SigningMethodHS256, settings().JWT.Secret)
	}
	for name, bearer := range cases {
		t.Run(name, func(t *testing.T) {
			s := new(stubService)
			w := request(handler(t, s, stubEvents{}, settings()), "POST", "/api/v1/sessions", "", bearer)
			if w.Code != 401 || s.scope.UserID != "" || strings.Contains(w.Body.String(), bearer) && bearer != "" {
				t.Fatalf("auth failed: %d %s", w.Code, w.Body.String())
			}
		})
	}
	s := new(stubService)
	w := request(handler(t, s, stubEvents{}, settings()), "POST", "/api/v1/sessions", `{"tenant_id":"evil","user_id":"evil","agent_id":"evil"}`, token(t))
	if w.Code != 201 || s.scope != (domain.Scope{TenantID: "tenant", UserID: "user"}) || !strings.Contains(w.Body.String(), "configured-agent") || strings.Contains(w.Body.String(), "Scope") {
		t.Fatalf("trusted scope/DTO: %d %s %+v", w.Code, w.Body.String(), s.scope)
	}
}

func TestJWTAllowsOptionalIssuerAndAudience(t *testing.T) {
	c := claims()
	c.Issuer = "external-identity"
	c.Audience = jwt.ClaimStrings{"external-api"}
	s := new(stubService)
	w := request(handler(t, s, stubEvents{}, settings()), "POST", "/api/v1/sessions", "", sign(t, c, jwt.SigningMethodHS256, settings().JWT.Secret))
	if w.Code != http.StatusCreated || s.scope != (domain.Scope{TenantID: "tenant", UserID: "user"}) {
		t.Fatalf("optional claims rejected: %d %s %+v", w.Code, w.Body.String(), s.scope)
	}
}

func TestRoutesLimitsErrorsAndRecovery(t *testing.T) {
	s := new(stubService)
	cfg := settings()
	cfg.MaxBodyBytes = 32
	h := handler(t, s, stubEvents{}, cfg)
	bearer := token(t)
	w := request(h, "POST", "/api/v1/sessions/session/runs", `{"text":"hello"}`, bearer)
	if w.Code != 202 || s.start.IdempotencyKey != "stable-key" || s.start.Input[0].Text != "hello" || s.start.Scope.UserID != "user" {
		t.Fatalf("start: %d %+v", w.Code, s.start)
	}
	for body, status := range map[string]int{`{`: 400, `{} {}`: 400, `{"text":"` + strings.Repeat("x", 100) + `"}`: 413} {
		if w = request(h, "POST", "/api/v1/sessions/session/runs", body, bearer); w.Code != status {
			t.Fatalf("body: %d want %d", w.Code, status)
		}
	}
	for err, status := range map[error]int{apperrors.ErrInvalidArgument: 400, apperrors.ErrNotFound: 404, apperrors.ErrConflict: 409, apperrors.ErrUnsupported: 501, context.DeadlineExceeded: 503, errors.New("secret database diagnostic"): 500} {
		s.err = err
		w = request(h, "GET", "/api/v1/runs/run", "", bearer)
		if w.Code != status || strings.Contains(w.Body.String(), "secret") || w.Header().Get("X-Request-ID") == "" {
			t.Fatalf("error: %d %s", w.Code, w.Body.String())
		}
	}
	s.err = nil
	s.panics = true
	w = request(h, "POST", "/api/v1/sessions", "", bearer)
	if w.Code != 500 || strings.Contains(w.Body.String(), "private") {
		t.Fatal(w.Body.String())
	}
	s.panics = false
	w = request(h, "POST", "/api/v1/runs/run/cancel", "", bearer)
	if w.Code != 204 || s.cancelled.Load() != 1 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		if w = request(h, "GET", path, "", ""); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	notReady, err := httpapi.New(s, stubEvents{}, cfg, "agent", func(context.Context) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if w = request(notReady, "GET", "/readyz", "", ""); w.Code != 503 {
		t.Fatal(w.Code)
	}
}

func openStream(t *testing.T, url, bearer, cursor string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Last-Event-ID", cursor)
	client := &http.Client{Timeout: 5 * time.Second}
	r, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestSSEReplayHeartbeatDisconnectAndPreflight(t *testing.T) {
	s := new(stubService)
	e := stubEvents{follow: func(ctx context.Context, after int64, f func(domain.Event) error) error {
		for n := after + 1; n <= 3; n++ {
			if err := f(domain.Event{RunID: "run", Sequence: n, Kind: domain.EventTextDelta, Data: []byte("line\nnext")}); err != nil {
				return err
			}
		}
		return nil
	}}
	server := httptest.NewServer(handler(t, s, e, settings()))
	defer server.Close()
	response := openStream(t, server.URL+"/api/v1/runs/run/events", token(t), "1")
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if response.StatusCode != 200 || strings.Contains(text, "id: 1\n") || !strings.Contains(text, "id: 2\n") || !strings.Contains(text, "id: 3\n") || !strings.Contains(text, "event: message.delta\n") {
		t.Fatalf("replay: %d %s", response.StatusCode, text)
	}
	stopped := make(chan struct{})
	e.follow = func(ctx context.Context, _ int64, _ func(domain.Event) error) error {
		defer close(stopped)
		<-ctx.Done()
		return ctx.Err()
	}
	waiting := httptest.NewServer(handler(t, s, e, settings()))
	defer waiting.Close()
	response = openStream(t, waiting.URL+"/api/v1/runs/run/events", token(t), "")
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, ": heartbeat") {
			break
		}
	}
	response.Body.Close()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("subscription leaked")
	}
	if s.cancelled.Load() != 0 {
		t.Fatal("disconnect cancelled run")
	}
	for err, status := range map[error]int{store.ErrCursorExpired: 410, apperrors.ErrNotFound: 404, apperrors.ErrInvalidArgument: 400} {
		srv := httptest.NewServer(handler(t, s, stubEvents{preflight: err}, settings()))
		r := openStream(t, srv.URL+"/api/v1/runs/run/events", token(t), "0")
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		srv.Close()
		if r.StatusCode != status || strings.Contains(r.Header.Get("Content-Type"), "event-stream") || strings.Contains(string(body), ": connected") {
			t.Fatalf("preflight: %d %s", r.StatusCode, body)
		}
	}
	response = openStream(t, server.URL+"/api/v1/runs/run/events", token(t), "-1")
	response.Body.Close()
	if response.StatusCode != 400 {
		t.Fatal(response.StatusCode)
	}
}

// 不读取响应的真实 TCP 客户端必须被写截止时间断开，且不能无限预取事件。
func TestSlowTCPConsumerIsDisconnected(t *testing.T) {
	done := make(chan struct{})
	var produced atomic.Int32
	e := stubEvents{follow: func(ctx context.Context, _ int64, f func(domain.Event) error) error {
		defer close(done)
		payload := []byte(strings.Repeat("x", 8<<20))
		for n := int64(1); ; n++ {
			produced.Add(1)
			if err := f(domain.Event{RunID: "run", Sequence: n, Kind: domain.EventTextDelta, Data: payload}); err != nil {
				return err
			}
		}
	}}
	srv := httptest.NewServer(handler(t, new(stubService), e, settings()))
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetReadBuffer(1024)
	}
	fmt.Fprintf(conn, "GET /api/v1/runs/run/events HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\n\r\n", token(t))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("slow client retained subscription")
	}
	if produced.Load() > 3 {
		t.Fatalf("unbounded prefetch: %d", produced.Load())
	}
}
