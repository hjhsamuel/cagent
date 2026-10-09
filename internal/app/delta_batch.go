package app

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"time"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/domain"
)

// Only plain text deltas are batched. A checkpoint, tool boundary or completed
// message always flushes first. Memory is bounded and the timer bounds idle latency.
type deltaBatch struct {
	mu     sync.Mutex
	ctx    context.Context
	emit   agent.Emit
	data   map[string]json.RawMessage
	text   string
	timer  *time.Timer
	err    error
	closed bool
}

func newDeltaBatch(ctx context.Context, emit agent.Emit) *deltaBatch {
	return &deltaBatch{ctx: ctx, emit: emit}
}
func (b *deltaBatch) flush() error {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	if b.err != nil {
		return b.err
	}
	if b.data == nil {
		return nil
	}
	b.data["text"], _ = json.Marshal(b.text)
	raw, err := json.Marshal(b.data)
	if err == nil {
		err = b.emit(b.ctx, agent.Update{Kind: domain.EventTextDelta, Data: raw})
	}
	b.data = nil
	b.text = ""
	b.err = err
	return err
}
func (b *deltaBatch) Emit(ctx context.Context, update agent.Update) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return context.Canceled
	}
	if b.err != nil {
		return b.err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var data map[string]json.RawMessage
	var text string
	plain := update.Kind == domain.EventTextDelta && update.Message == nil && update.Checkpoint == nil && len(update.Tasks) == 0 && update.AppliedTaskID == ""
	if plain {
		plain = json.Unmarshal(update.Data, &data) == nil && data != nil && json.Unmarshal(data["text"], &text) == nil
		delete(data, "text")
	}
	if !plain {
		if err := b.flush(); err != nil {
			return err
		}
		return b.emit(ctx, update)
	}
	if b.data != nil && (!reflect.DeepEqual(data, b.data) || len(b.text)+len(text) > 8<<10) {
		if err := b.flush(); err != nil {
			return err
		}
	}
	if b.data == nil {
		b.data = data
		b.timer = time.AfterFunc(75*time.Millisecond, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if !b.closed {
				_ = b.flush()
			}
		})
	}
	b.text += text
	if len(b.text) >= 8<<10 {
		return b.flush()
	}
	return nil
}
func (b *deltaBatch) Finish() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	err := b.flush()
	b.closed = true
	return err
}
