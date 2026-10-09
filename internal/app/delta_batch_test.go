package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/domain"
)

func delta(text string) agent.Update {
	raw, _ := json.Marshal(map[string]string{"invocation_id": "run", "text": text})
	return agent.Update{Kind: domain.EventTextDelta, Data: raw}
}
func TestDeltaBatchOrderSizeAndIdleFlush(t *testing.T) {
	var mu sync.Mutex
	var updates []agent.Update
	b := newDeltaBatch(context.Background(), func(_ context.Context, u agent.Update) error {
		mu.Lock()
		defer mu.Unlock()
		updates = append(updates, u)
		return nil
	})
	for _, s := range []string{"你", "好", "!"} {
		if err := b.Emit(context.Background(), delta(s)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Emit(context.Background(), agent.Update{Kind: domain.EventToolStarted}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(updates) != 2 || updates[0].Kind != domain.EventTextDelta || updates[1].Kind != domain.EventToolStarted {
		t.Fatal("boundary ordering", updates)
	}
	var data struct{ Text string }
	_ = json.Unmarshal(updates[0].Data, &data)
	if data.Text != "你好!" {
		t.Fatal(data)
	}
	mu.Unlock()
	if err := b.Emit(context.Background(), delta(strings.Repeat("x", 8<<10))); err != nil {
		t.Fatal(err)
	}
	if err := b.Emit(context.Background(), delta("idle")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		n := len(updates)
		mu.Unlock()
		if n == 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle text not flushed")
		}
		time.Sleep(time.Millisecond)
	}
	if err := b.Finish(); err != nil {
		t.Fatal(err)
	}
}
func TestDeltaBatchPropagatesPersistenceFailure(t *testing.T) {
	failure := errors.New("write failed")
	calls := 0
	b := newDeltaBatch(context.Background(), func(context.Context, agent.Update) error { calls++; return failure })
	if err := b.Emit(context.Background(), delta("buffered")); err != nil {
		t.Fatal(err)
	}
	if err := b.Emit(context.Background(), agent.Update{Kind: domain.EventMessageCompleted}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := b.Finish(); !errors.Is(err, failure) || calls != 1 {
		t.Fatal(err, calls)
	}
}
