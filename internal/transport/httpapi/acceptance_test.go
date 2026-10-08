package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/hjhsamuel/cagent/internal/adapter/a2a"
	"github.com/hjhsamuel/cagent/internal/adapter/adk"
	"github.com/hjhsamuel/cagent/internal/adapter/mongodb"
	"github.com/hjhsamuel/cagent/internal/adapter/toolhttp"
	"github.com/hjhsamuel/cagent/internal/app"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"github.com/hjhsamuel/cagent/internal/tool"
)

// TestAcceptanceTaskRestartReplay 将交付边界串为同一条真实持久链路。
// 仅外部模型和 A2A 提供方由本地 HTTP 夹具替代，认证、两个协议 SDK、
// ADK 检查点、MongoDB 事务、恢复扫描和网络 SSE 均使用生产代码。
// 重启通过关闭旧应用并重建全部客户端实现；持久化中间状态另由故障专项测试覆盖。
func TestAcceptanceTaskRestartReplay(t *testing.T) {
	ctx := context.Background()
	db, opts := testDatabase(t)
	scope := domain.Scope{TenantID: "tenant", UserID: "user"}
	var finished, outage atomic.Bool
	var executions, models atomic.Int32
	var remote *httptest.Server
	remote = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(map[string]any{"name": "remote_agent", "url": remote.URL, "protocolVersion": "0.3.0", "capabilities": map[string]any{}})
			return
		}
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ID string `json:"id"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		switch request.Method {
		case "message/send":
			executions.Add(1)
		case "tasks/get":
			if request.Params.ID != "remote-task" {
				t.Error("恢复改变了远端任务 ID")
			}
			if outage.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		default:
			t.Errorf("unexpected RPC: %s", request.Method)
		}
		state := "working"
		if finished.Load() {
			state = "completed"
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"kind": "task", "id": "remote-task", "contextId": "remote-context", "status": map[string]any{"state": state}, "artifacts": []any{map[string]any{"parts": []any{map[string]any{"kind": "text", "text": "tool answer"}}}}}})
	}))
	defer remote.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role   string `json:"role"`
				CallID string `json:"tool_call_id"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if models.Add(1) == 1 {
			fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"original-call","type":"function","function":{"name":"remote_agent","arguments":"{\"text\":\"work\"}"}}]}}]}`)
			return
		}
		count := 0
		for _, message := range request.Messages {
			if message.Role == "tool" {
				count++
				if message.CallID != "original-call" {
					t.Error("结果未续接原调用")
				}
			}
		}
		if count != 1 {
			t.Errorf("工具结果必须恰好一次进入模型: %d", count)
		}
		fmt.Fprint(w, `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"final answer"}}]}`)
	}))
	defer model.Close()
	// 每次启动重新发现提供方并建立 Catalog，避免旧应用内存掩盖恢复缺陷。
	startInstance := func(database *mongodb.Database) (*app.Application, *httptest.Server) {
		t.Helper()
		client, err := a2a.New(ctx, toolhttp.Config{Scope: scope, ID: "remote", URL: remote.URL, Timeout: time.Second, MaxBytes: 1 << 20}, "/.well-known/agent-card.json")
		if err != nil {
			t.Fatal(err)
		}
		catalog, err := tool.NewCatalog([]tool.Entry{{Scope: scope, ConnectionID: "remote", Descriptor: client.Discover(), Executor: client, Tasks: client}}, tool.Limits{Timeout: time.Second, MaxInputBytes: 1024, MaxOutputBytes: 4096})
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Defaults()
		cfg.MongoDB.URI = opts.URI
		cfg.Agent.Provider, cfg.Agent.Model, cfg.Agent.BaseURL, cfg.Agent.APIKey, cfg.Agent.TokenEncoding = "openai", "test-model", model.URL, "test-key", "o200k_base"
		a, err := app.NewOpenAIService(ctx, database, cfg, nil, app.Options{Registry: catalog, LeaseDuration: 3 * time.Second, PollInterval: 20 * time.Millisecond, Tasks: config.Tasks{PollInterval: 20 * time.Millisecond, ReconnectBackoff: 20 * time.Millisecond, ObservationTimeout: time.Second}}, adk.ToolOptions{Registry: catalog, MaxModelCalls: 4})
		if err != nil {
			t.Fatal(err)
		}
		events, err := app.NewEventStream(database, 10*time.Millisecond, 2)
		if err != nil {
			t.Fatal(err)
		}
		s := httptest.NewServer(handler(t, a, events, settings()))
		t.Cleanup(func() {
			s.Close()
			if err := a.Close(ctx); err != nil {
				t.Error(err)
			}
		})
		return a, s
	}
	a, first := startInstance(db)
	bearer := token(t)
	call := func(server *httptest.Server, method, path, body, auth string, want int) []byte {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+auth)
		req.Header.Set("Idempotency-Key", "acceptance")
		response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != want {
			t.Fatalf("%s %s: %d %s", method, path, response.StatusCode, data)
		}
		return data
	}
	decodeID := func(data []byte) string {
		t.Helper()
		var resource struct{ ID string }
		if err := json.Unmarshal(data, &resource); err != nil || resource.ID == "" {
			t.Fatalf("missing resource ID: %s", data)
		}
		return resource.ID
	}
	call(first, "POST", "/api/v1/sessions", "", "invalid", 401)
	sessionID := decodeID(call(first, "POST", "/api/v1/sessions", "", bearer, 201))
	runPath := "/api/v1/sessions/" + sessionID + "/runs"
	runID := decodeID(call(first, "POST", runPath, `{"text":"do work"}`, bearer, 202))
	await := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("验收等待超时")
	}
	var task domain.Task
	await(func() bool {
		page, err := db.ListUnsettledTasks(ctx, scope, runID, store.KeyPage{Limit: 10})
		if err != nil || len(page.Items) != 1 {
			return false
		}
		task = page.Items[0]
		return task.Status == domain.TaskRunning
	})
	streamPath := "/api/v1/runs/" + runID + "/events"
	stream := openStream(t, first.URL+streamPath, bearer, "")
	stream.Body.Close()
	// 提供方观察失败只能记录本地诊断；不得将任务判失败，也不得重新发送 message/send。
	outage.Store(true)
	await(func() bool {
		current, err := db.GetTask(ctx, scope, task.ID)
		return err == nil && current.ObservationError != "" && !current.Status.IsTerminal()
	})
	page, err := db.ListEvents(ctx, scope, runID, store.SequencePage{Limit: 100})
	if err != nil || len(page.Items) == 0 {
		t.Fatal("missing persisted events", err)
	}
	cursor := page.Items[len(page.Items)-1].Sequence
	first.Close()
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// 新实例使用独立数据库连接，只依赖已落库的句柄/检查点。旧 SSE 已断开。
	db2, err := mongodb.Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db2.Close(ctx) })
	b, second := startInstance(db2)
	recovery, err := mongodb.OpenRecovery(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recovery.Close(ctx) })
	// 后注册的清理先执行：扫描器退出后才能关闭其管理连接。
	t.Cleanup(func() { b.Close(ctx) })
	if err := b.StartRecovery(recovery, 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	for _, foreign := range []httpapiScopeChange{{tenant: "other", user: "user"}, {tenant: "tenant", user: "other"}} {
		c := claims()
		c.TenantID, c.Subject = foreign.tenant, foreign.user
		auth := sign(t, c, jwt.SigningMethodHS256, settings().JWT.Secret)
		for _, path := range []string{"/api/v1/tasks/" + task.ID, "/api/v1/runs/" + runID, streamPath} {
			call(second, "GET", path, "", auth, 404)
		}
		call(second, "POST", "/api/v1/tasks/"+task.ID+"/cancel", "", auth, 404)
	}
	outage.Store(false)
	finished.Store(true)
	await(func() bool {
		run, err := db2.GetRun(ctx, scope, runID)
		return err == nil && run.Status == domain.RunCompleted
	})
	if got := decodeID(call(second, "POST", runPath, `{"text":"do work"}`, bearer, 202)); got != runID {
		t.Fatal("重启后幂等重放创建了新 Run")
	}
	call(second, "GET", "/api/v1/tasks/"+task.ID, "", bearer, 200)
	saved, err := db2.GetTask(ctx, scope, task.ID)
	if err != nil || saved.AppliedAt == nil || saved.Status != domain.TaskSucceeded {
		t.Fatal("任务未原子接纳", err)
	}
	delivery, err := db2.GetTaskDelivery(ctx, scope, task.ID)
	if err != nil || delivery.State != domain.DeliveryApplied {
		t.Fatal("交付未结算", err)
	}
	// 用真实网络重放并逐条比较数据库序号，既检查排他游标，也检查无丢失、无重复。
	replay := openStream(t, second.URL+streamPath, bearer, strconv.FormatInt(cursor, 10))
	data, err := io.ReadAll(replay.Body)
	replay.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	after, err := db2.ListEvents(ctx, scope, runID, store.SequencePage{After: cursor, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "id: ") {
			id, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
	}
	if len(ids) != len(after.Items) || len(ids) == 0 {
		t.Fatalf("SSE replay count %v vs %d", ids, len(after.Items))
	}
	for i, event := range after.Items {
		if ids[i] != event.Sequence || ids[i] != cursor+int64(i)+1 {
			t.Fatal("SSE sequence gap", ids)
		}
	}
	if strings.Count(string(data), "event: run.completed") != 1 || executions.Load() != 1 || models.Load() != 2 {
		t.Fatalf("重复执行或终态: executions=%d models=%d stream=%s", executions.Load(), models.Load(), data)
	}
}

type httpapiScopeChange struct{ tenant, user string }
