package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/observability"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func sessionCatalog(t *testing.T, url string) *config.ModelCatalog {
	t.Helper()
	ring, err := config.NewKeyring(config.ModelEncryption{Keys: map[string]string{"v1": "MDEyMzQ1Njc4OWFiY2RlZg=="}})
	if err != nil {
		t.Fatal(err)
	}
	docs := []schema.Model{}
	for _, id := range []string{"first", "second"} {
		doc := schema.Model{ID: id, Model: "vendor-" + id, Provider: "GLM", BaseURL: url, Options: schema.ModelConfig{TokenEncoding: "o200k_base", MaxTokensField: "max_tokens", RequestTimeout: "2s", WindowTokens: 8192, OutputTokens: 2048}}
		for i := 0; i < 2; i++ {
			key, err := ring.Encrypt(fmt.Sprintf("secret-%s-%d", id, i), int64(i+1))
			if err != nil {
				t.Fatal(err)
			}
			doc.APIKeys = append(doc.APIKeys, key)
		}
		docs = append(docs, doc)
	}
	catalog, err := config.NewModelCatalog(docs, "agent", ring)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestSessionModelPersistsAcrossTurnsAndRestart(t *testing.T) {
	db, dbCfg := testDatabase(t)
	ctx := context.Background()
	var mu sync.Mutex
	var received []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct{ Model string }
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		received = append(received, body.Model+"/"+req.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	cfg := config.Defaults()
	cfg.MongoDB.URI = dbCfg.URI
	cfg.Context.CompressionThresholdPercent = 0
	cfg.Models = sessionCatalog(t, server.URL)
	opts := Options{LeaseDuration: 3 * time.Second, PollInterval: 50 * time.Millisecond}
	create := func() *Application {
		a, err := NewOpenAIService(ctx, db, cfg, nil, opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.Close(ctx) })
		return a
	}
	a := create()
	s, err := a.CreateSessionWithModel(ctx, scope, "agent", "first")
	if err != nil || s.ModelID != "first" || s.APIKeyID == "" {
		t.Fatal("session model was not selected at creation", err)
	}
	chosen, err := cfg.Models.Bind(s.ModelID, s.APIKeyID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		random, err := a.CreateSession(ctx, scope, "agent")
		if err != nil || random.ModelID == "" || random.APIKeyID == "" {
			t.Fatal("random creation did not bind immediately", err)
		}
		seen[random.ModelID] = true
	}
	if len(seen) != 2 {
		t.Fatal("random sessions did not use both models")
	}
	for i := 0; i < 2; i++ {
		run := start(t, a, s, fmt.Sprintf("turn-%d", i))
		awaitStatus(t, db, run, domain.RunCompleted)
		cp, err := db.GetCheckpoint(ctx, scope, run.ID, run.ID)
		if err != nil || cp.ModelID != s.ModelID || cp.APIKeyID != s.APIKeyID || strings.Contains(string(cp.Data), chosen.Agent.APIKey) {
			t.Fatal("checkpoint binding or secrecy", err)
		}
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	a = create()
	run := start(t, a, s, "after-restart")
	awaitStatus(t, db, run, domain.RunCompleted)
	saved, err := db.GetSession(ctx, scope, s.ID)
	if err != nil || saved.ModelID != s.ModelID || saved.APIKeyID != s.APIKeyID {
		t.Fatal("restart changed session binding", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 3 {
		t.Fatalf("unexpected calls: %v", received)
	}
	for _, call := range received {
		if call != "vendor-first/Bearer "+chosen.Agent.APIKey {
			t.Fatal("main dialogue changed model/key", received)
		}
	}
	if _, err := a.CreateSessionWithModel(ctx, scope, "agent", "missing"); !errors.Is(err, apperrors.ErrNotFound) {
		t.Fatal("unknown model accepted", err)
	}
	// Check the actual MongoDB envelope contains references, never the credential.
	client, err := mongo.Connect(options.Client().ApplyURI(dbCfg.URI))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Disconnect(ctx)
	raw, err := client.Database(dbCfg.Database).Collection(schema.SessionCollection).FindOne(ctx, map[string]any{"id": s.ID}).Raw()
	if err != nil || strings.Contains(string(raw), chosen.Agent.APIKey) {
		t.Fatal("session leaked credential", err)
	}
}

func TestSubagentSelectionIgnoresSessionAndRestoresBranchBinding(t *testing.T) {
	catalog := sessionCatalog(t, "https://example.invalid/v1")
	r := &sessionModels{cfg: config.Config{Models: catalog}}
	req := agent.Request{Run: domain.Run{Scope: scope, ID: "run", SessionID: "session"}, Caller: domain.AgentExecution{AgentID: "child", InvocationID: "child-1", ParentInvocationID: "run"}}
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		selected, err := r.selection(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		seen[selected.ModelID] = true
	}
	if len(seen) != 2 {
		t.Fatal("subagent is tied to main model")
	}
	selected, err := catalog.Select("second")
	if err != nil {
		t.Fatal(err)
	}
	cp := domain.Checkpoint{Scope: scope, RunID: req.Run.ID, Caller: req.Caller, Format: "test"}
	emit := selectedEmit(selected, func(_ context.Context, update agent.Update) error { cp = *update.Checkpoint; return nil })
	if err := emit(context.Background(), agent.Update{Checkpoint: &cp}); err != nil {
		t.Fatal(err)
	}
	req.Checkpoint = &cp
	for i := 0; i < 10; i++ {
		bound, err := r.selection(context.Background(), req)
		if err != nil || bound.ModelID != selected.ModelID || bound.APIKeyID != selected.APIKeyID || bound.Agent.APIKey != selected.Agent.APIKey {
			t.Fatal("branch recovery changed choice", err)
		}
	}
	cp.ModelID, cp.APIKeyID = "", ""
	if _, err := r.selection(context.Background(), req); !errors.Is(err, agent.ErrUncertain) {
		t.Fatal("unbound old branch was silently reassigned", err)
	}
}

func TestSubagentHTTPCallsAndCheckpointRecovery(t *testing.T) {
	var mu sync.Mutex
	var received []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct{ Model string }
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		mu.Lock()
		received = append(received, body.Model+"/"+req.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	cfg := config.Defaults()
	cfg.MongoDB.URI = "mongodb://localhost:27017"
	cfg.Context.CompressionThresholdPercent = 0
	cfg.Models = sessionCatalog(t, server.URL)
	r := &sessionModels{parent: context.Background(), cfg: cfg, gate: observability.NewGate(cfg.Capacity.Models)}
	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		req := agent.Request{Run: domain.Run{Scope: scope, ID: "run", SessionID: "main-session", Status: domain.RunRunning}, Caller: domain.AgentExecution{AgentID: "child", InvocationID: fmt.Sprintf("child-%d", i), ParentInvocationID: "run"}, Messages: []domain.Message{{Scope: scope, ID: "input", RunID: "run", SessionID: "main-session", Role: domain.RoleUser, Parts: []domain.Part{{Kind: domain.PartText, Text: "hello"}}}}}
		var cp *domain.Checkpoint
		if err := r.Execute(context.Background(), req, func(_ context.Context, u agent.Update) error {
			if u.Checkpoint != nil {
				cp = u.Checkpoint
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if cp == nil {
			t.Fatal("missing child checkpoint")
		}
		cp.Version = 1 // Simulate a checkpoint committed by the persistence layer.
		bound, err := cfg.Models.Bind(cp.ModelID, cp.APIKeyID)
		mu.Lock()
		call, before := received[len(received)-1], len(received)
		mu.Unlock()
		if err != nil || call != bound.Agent.Model+"/Bearer "+bound.Agent.APIKey {
			t.Fatal("child HTTP request mismatched binding", err)
		}
		seen[cp.ModelID] = true
		req.Checkpoint = cp
		if err := r.Recover(context.Background(), req, func(_ context.Context, _ agent.Update) error { return nil }); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		after := len(received)
		mu.Unlock()
		if after != before {
			t.Fatal("completed child checkpoint regenerated output")
		}
	}
	if len(seen) != 2 {
		t.Fatal("child HTTP calls did not use independent models")
	}
}

func TestLegacySessionBindingIsAtomic(t *testing.T) {
	db, _ := testDatabase(t)
	ctx := context.Background()
	old := domain.Session{Scope: scope, ID: "legacy-session", AgentID: "agent"}
	if err := db.CreateSession(ctx, old); err != nil {
		t.Fatal(err)
	}
	results := make(chan domain.Session, 8)
	errors := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := db.BindSessionModel(ctx, scope, old.ID, fmt.Sprintf("model-%d", i), fmt.Sprintf("key-%d", i))
			results <- s
			errors <- err
		}(i)
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	saved, err := db.GetSession(ctx, scope, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	for s := range results {
		if s.ModelID != saved.ModelID || s.APIKeyID != saved.APIKeyID || s.Version != saved.Version {
			t.Fatal("concurrent binding replaced original choice")
		}
	}
}
