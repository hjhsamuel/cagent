package contextengine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hjhsamuel/cagent/internal/domain"
)

type summaryFunc func(context.Context, []domain.Message) (string, error)

func (f summaryFunc) Summarize(c context.Context, m []domain.Message) (string, error) { return f(c, m) }

func compressInput() Input {
	in := toolInput()
	in.History[3].Parts[0].Text = strings.Repeat("old facts ", 1000)
	in.Budget = Budget{WindowTokens: 2000, OutputTokens: 100}
	return in
}

// 即使模型漏写了要求，用户原文也必须保留；完整工具对及悬而未决的调用逐字节不变。
func TestCompressionPreservesRequirementsAndPendingCalls(t *testing.T) {
	in := compressInput()
	before := cloneMessages(in.History)
	var calls int
	b, err := NewCompressing(counterFunc(testCount), summaryFunc(func(_ context.Context, m []domain.Message) (string, error) {
		calls++
		if len(m) != 4 {
			t.Fatal("wrong prefix", len(m))
		}
		m[0].Parts[0].Text = "mutated"
		return "facts", nil
	}), CompressionPolicy{80, 1})
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.Prepare(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || out.NewSnapshot == nil || out.NewSnapshot.Version != 0 || out.NewSnapshot.ThroughSequence != 4 {
		t.Fatalf("bad candidate %+v", out)
	}
	if !reflect.DeepEqual(before, in.History) {
		t.Fatal("history mutated")
	}
	for _, index := range []int{0, 1, 2, 4} {
		found := false
		for _, m := range out.Messages {
			if m.ID == in.History[index].ID {
				found = reflect.DeepEqual(m, in.History[index])
			}
		}
		if !found {
			t.Fatal("lost protected message", index)
		}
	}
	if out.Messages[0].Parts[0].Text != "fixed rules" || strings.Contains(ids(out.Messages), ",answer,") {
		t.Fatal("system lost or old answer retained")
	}
}

func TestCompressionBoundedFailureAndOversize(t *testing.T) {
	for _, kind := range []string{"error", "empty", "oversize", "cancel"} {
		for _, fits := range []bool{false, true} {
			t.Run(kind+map[bool]string{true: "/fits", false: "/over"}[fits], func(t *testing.T) {
				in := compressInput()
				if fits {
					in.Budget.WindowTokens = 13000
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				b, _ := NewCompressing(counterFunc(testCount), summaryFunc(func(context.Context, []domain.Message) (string, error) {
					calls++
					switch kind {
					case "error":
						return "", errors.New("summary failed")
					case "empty":
						return " ", nil
					case "oversize":
						return strings.Repeat("x", 20000), nil
					default:
						cancel()
						return "ok", nil
					}
				}), CompressionPolicy{1, 1})
				out, err := b.Prepare(ctx, in)
				if calls != 1 {
					t.Fatal("unbounded retry", calls)
				}
				if fits && kind != "cancel" {
					if err != nil || out.NewSnapshot != nil || len(out.Messages) == 0 {
						t.Fatal("valid fallback lost", err)
					}
				} else if err == nil || !reflect.DeepEqual(out, Prepared{}) {
					t.Fatal("unsafe partial output", err)
				}
			})
		}
	}
}

func TestCompressionRecentRoundsPolicyMigrationAndConcurrency(t *testing.T) {
	in := compressInput()
	in.Budget.WindowTokens = 30000
	var calls atomic.Int32
	f := summaryFunc(func(context.Context, []domain.Message) (string, error) { calls.Add(1); return "facts", nil })
	b, _ := NewCompressing(counterFunc(testCount), f, CompressionPolicy{80, 1})
	if out, e := b.Prepare(context.Background(), in); e != nil || out.NewSnapshot != nil || calls.Load() != 0 {
		t.Fatal("below threshold")
	}
	b, _ = NewCompressing(counterFunc(testCount), f, CompressionPolicy{1, 2})
	if out, e := b.Prepare(context.Background(), in); e != nil || out.NewSnapshot != nil || calls.Load() != 0 {
		t.Fatal("recent round compressed")
	}
	in.Snapshot = snapshot(in, 4)
	in.Snapshot.PolicyVersion = "old-policy"
	b, _ = NewCompressing(counterFunc(testCount), f, CompressionPolicy{1, 1})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, e := b.Prepare(context.Background(), in)
			if e != nil || out.NewSnapshot == nil || out.NewSnapshot.Version != 1 || out.NewSnapshot.PolicyVersion != "v1" {
				t.Errorf("migration: %+v %v", out, e)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 16 {
		t.Fatal(calls.Load())
	}
	// 同策略已覆盖该水位不再重复请求；扩大近期保留范围不能继续使用过度覆盖的摘要。
	in.Snapshot.PolicyVersion = "v1"
	_, e := b.Prepare(context.Background(), in)
	if e != nil || calls.Load() != 16 {
		t.Fatal("repeated summary", e)
	}
	b, _ = NewCompressing(counterFunc(testCount), f, CompressionPolicy{1, 2})
	out, e := b.Prepare(context.Background(), in)
	if e != nil || out.Snapshot != nil || !strings.Contains(ids(out.Messages), "answer") {
		t.Fatal("recent history hidden", e)
	}
}
