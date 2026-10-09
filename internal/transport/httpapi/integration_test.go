package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/app"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"github.com/hjhsamuel/cagent/internal/transport/httpapi"
)

// 完整链路采用真实 Gin、JWT、ADK/OpenAI SDK、MongoDB 副本集及网络 SSE。
// 仅模型提供方是受控 HTTP 服务，以同步点验证客户端断开后仍能提交最终输出。
func TestHTTPADKDatabaseLifecycle(t *testing.T) {
	db, dbCfg := testDatabase(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var calls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 先消费请求体，让 net/http 启动连接断开检测；未读完请求体时只等待
		// Context 会让测试提供方自己阻塞，无法观察客户端的取消。
		_, _ = io.Copy(io.Discard, r.Body)
		n := calls.Add(1)
		entered <- struct{}{}
		if n > 1 {
			<-r.Context().Done()
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	cfg := config.Defaults()
	cfg.MongoDB.URI = dbCfg.URI
	cfg.Agent.WindowTokens = 32768
	cfg.Agent.Provider = "openai"
	cfg.Agent.Model = "test-model"
	cfg.Agent.BaseURL = provider.URL
	cfg.Agent.APIKey = "test-key"
	application, err := app.NewOpenAIService(context.Background(), db, cfg, nil, app.Options{LeaseDuration: 3 * time.Second, PollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer application.Close(context.Background())
	events, err := app.NewEventStream(db, 10*time.Millisecond, 2)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler(t, application, events, settings()))
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	bearer := token(t)
	call := func(method, path, body, auth, key string) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+auth)
		req.Header.Set("Idempotency-Key", key)
		r, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer r.Body.Close()
		data, e := io.ReadAll(r.Body)
		if e != nil {
			t.Fatal(e)
		}
		return r.StatusCode, data
	}
	status, data := call("POST", "/api/v1/sessions", "", bearer, "")
	if status != 201 {
		t.Fatalf("create: %d %s", status, data)
	}
	var session struct{ ID string }
	json.Unmarshal(data, &session)
	path := "/api/v1/sessions/" + session.ID + "/runs"
	status, data = call("POST", path, `{"text":"hello"}`, bearer, "stable")
	if status != 202 {
		t.Fatalf("start: %d %s", status, data)
	}
	var run struct{ ID string }
	json.Unmarshal(data, &run)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("model never started")
	}
	stream := openStream(t, server.URL+"/api/v1/runs/"+run.ID+"/events", bearer, "")
	stream.Body.Close()
	status, data = call("POST", path, `{"text":"hello"}`, bearer, "stable")
	var retry struct{ ID string }
	json.Unmarshal(data, &retry)
	if status != 202 || retry.ID != run.ID {
		t.Fatal("idempotent replay lost")
	}
	if status, _ = call("POST", path, `{"text":"different"}`, bearer, "stable"); status != 409 {
		t.Fatalf("input conflict: %d", status)
	}
	if status, _ = call("POST", path, `{"text":"another"}`, bearer, "other"); status != 409 {
		t.Fatalf("active conflict: %d", status)
	}
	for _, c := range []httpapi.Claims{func() httpapi.Claims { c := claims(); c.Subject = "other-user"; return c }(), func() httpapi.Claims { c := claims(); c.TenantID = "other-tenant"; return c }()} {
		auth := sign(t, c, jwt.SigningMethodHS256, settings().JWT.Secret)
		for _, op := range [][2]string{{"GET", "/api/v1/sessions/" + session.ID}, {"POST", path}, {"GET", "/api/v1/runs/" + run.ID}, {"POST", "/api/v1/runs/" + run.ID + "/cancel"}, {"GET", "/api/v1/runs/" + run.ID + "/events"}} {
			if status, _ = call(op[0], op[1], `{"text":"x"}`, auth, ""); status != 404 {
				t.Fatalf("scope isolation %v: %d", op, status)
			}
		}
	}
	close(release)
	replay := openStream(t, server.URL+"/api/v1/runs/"+run.ID+"/events", bearer, "1")
	data, err = io.ReadAll(replay.Body)
	replay.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "event: run.completed") || strings.Contains(string(data), "id: 1\n") || calls.Load() != 1 {
		t.Fatalf("completion after disconnect: %s calls %d", data, calls.Load())
	}
	scope := domain.Scope{TenantID: "tenant", UserID: "user"}
	messages, err := db.ListMessages(context.Background(), scope, session.ID, store.SequencePage{Limit: 10})
	if err != nil || len(messages.Items) != 2 || messages.Items[1].Parts[0].Text != "answer" {
		t.Fatalf("durable output: %+v %v", messages, err)
	}
	// 清理后的旧游标必须在 SSE 响应提交前以 410 返回。
	recovery, err := mongodb.OpenRecovery(context.Background(), dbCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer recovery.Close(context.Background())
	if err = recovery.PruneEvents(context.Background(), scope, run.ID, 1); err != nil {
		t.Fatal(err)
	}
	replay = openStream(t, server.URL+"/api/v1/runs/"+run.ID+"/events", bearer, "0")
	replay.Body.Close()
	if replay.StatusCode != 410 {
		t.Fatalf("pruned cursor: %d", replay.StatusCode)
	}
	// 第二个运行在提供方等待，以真实取消事务验证终止事件及远端请求取消。
	status, data = call("POST", path, `{"text":"cancel me"}`, bearer, "cancel-key")
	if status != 202 {
		t.Fatalf("second start: %d %s", status, data)
	}
	json.Unmarshal(data, &run)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("second model never started")
	}
	status, data = call("POST", "/api/v1/runs/"+run.ID+"/cancel", "", bearer, "")
	if status != 204 {
		t.Fatalf("cancel: %d %s", status, data)
	}
	replay = openStream(t, server.URL+"/api/v1/runs/"+run.ID+"/events", bearer, "")
	data, err = io.ReadAll(replay.Body)
	replay.Body.Close()
	if err != nil || !strings.Contains(string(data), "event: run.cancelled") {
		t.Fatalf("cancelled stream: %s %v", data, err)
	}
	persisted, err := db.GetRun(context.Background(), scope, run.ID)
	if err != nil || persisted.Status != domain.RunCancelled {
		t.Fatalf("cancel not persisted: %+v %v", persisted, err)
	}
}
