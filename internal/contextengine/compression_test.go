package contextengine

import (
	"context"
	"errors"
	"fmt"
	"math"
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
	in.History[3].PromptTokens = 8192
	return in
}

// 即使模型漏写了要求，用户原文也必须保留；完整工具对及悬而未决的调用逐字节不变。
func TestCompressionPreservesRequirementsAndPendingCalls(t *testing.T) {
	in := compressInput()
	before := cloneMessages(in.History)
	var calls int
	b, err := NewCompressing(summaryFunc(func(_ context.Context, m []domain.Message) (string, error) {
		calls++
		if len(m) != 4 {
			t.Fatal("wrong prefix", len(m))
		}
		m[0].Parts[0].Text = "mutated"
		return "facts", nil
	}), CompressionPolicy{WindowTokens: 10240, ThresholdPercent: 80, KeepRecentRounds: 1})
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

func TestCompressionRecentRoundsPolicyMigrationAndConcurrency(t *testing.T) {
	in := compressInput()
	var calls atomic.Int32
	f := summaryFunc(func(context.Context, []domain.Message) (string, error) { calls.Add(1); return "facts", nil })
	b, _ := NewCompressing(f, CompressionPolicy{WindowTokens: 10240, ThresholdPercent: 80, KeepRecentRounds: 2})
	if out, e := b.Prepare(context.Background(), in); e != nil || out.NewSnapshot != nil || calls.Load() != 0 {
		t.Fatal("recent round compressed")
	}
	in.Snapshot = snapshot(in, 4)
	in.Snapshot.PolicyVersion = "old-policy"
	b, _ = NewCompressing(f, CompressionPolicy{WindowTokens: 10240, ThresholdPercent: 80, KeepRecentRounds: 1})
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
	b, _ = NewCompressing(f, CompressionPolicy{WindowTokens: 10240, ThresholdPercent: 80, KeepRecentRounds: 2})
	out, e := b.Prepare(context.Background(), in)
	if e != nil || out.Snapshot != nil || !strings.Contains(ids(out.Messages), "answer") {
		t.Fatal("recent history hidden", e)
	}
}

func TestCompressionFailureFallsBackWithoutRetry(t *testing.T) {
	for _, kind := range []string{"error", "empty", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			in := compressInput()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			b, err := NewCompressing(summaryFunc(func(context.Context, []domain.Message) (string, error) {
				calls++
				if kind == "cancel" {
					cancel()
					return "late summary", nil
				}
				if kind == "empty" {
					return " ", nil
				}
				return "", errors.New("summary failed")
			}), CompressionPolicy{WindowTokens: 10240, ThresholdPercent: 80, KeepRecentRounds: 1})
			if err != nil {
				t.Fatal(err)
			}
			out, err := b.Prepare(ctx, in)
			if calls != 1 {
				t.Fatal("summary retried", calls)
			}
			if kind == "cancel" {
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(out, Prepared{}) {
					t.Fatal("late summary accepted", err)
				}
			} else {
				base, baseErr := New().Prepare(ctx, in)
				if err != nil || baseErr != nil || out.NewSnapshot != nil || !reflect.DeepEqual(out.Messages, base.Messages) {
					t.Fatal("raw history lost", err)
				}
			}
		})
	}
}

func TestCompressionReportedUsagePreservesRecentRounds(t *testing.T) {
	for _, keep := range []int{1, 2} {
		calls := 0
		b, err := NewCompressing(summaryFunc(func(context.Context, []domain.Message) (string, error) {
			calls++
			return "facts", nil
		}), CompressionPolicy{WindowTokens: 10240, ThresholdPercent: 80, KeepRecentRounds: keep})
		if err != nil {
			t.Fatal(err)
		}
		in := input()
		in.History[1].PromptTokens = 8192
		out, err := b.Prepare(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if keep == 1 && (calls != 1 || out.NewSnapshot == nil || out.NewSnapshot.ThroughSequence != 2) {
			t.Fatal("older round was not summarized")
		}
		if keep == 2 && (calls != 0 || out.NewSnapshot != nil) {
			t.Fatal("protected recent round was summarized")
		}
	}
}

func TestCompressionTriggersOnlyOnLatestReportedPromptTokens(t *testing.T) {
	for _, tokens := range []int32{0, 8191, 8192, 8193} {
		t.Run(fmt.Sprint(tokens), func(t *testing.T) {
			in := compressInput()
			// 旧响应曾达到阈值；最新响应缺失/偏低时不能沿用或累加旧值。
			in.History[1].PromptTokens = 20000
			in.History[3].PromptTokens = tokens
			calls := 0
			b, err := NewCompressing(summaryFunc(func(context.Context, []domain.Message) (string, error) {
				calls++
				return "facts", nil
			}), CompressionPolicy{WindowTokens: 10240, ThresholdPercent: 80, KeepRecentRounds: 1})
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := 0
			if tokens >= 8192 {
				wantCalls = 1
			}
			out, err := b.Prepare(context.Background(), in)
			if err != nil || (out.NewSnapshot != nil) != (tokens >= 8192) || calls != wantCalls {
				t.Fatal("unexpected usage trigger", calls, out.NewSnapshot, err)
			}
		})
	}
}

func TestCompressionThresholdFollowsModelWindow(t *testing.T) {
	for _, tc := range []struct {
		window  int64
		percent int
		usage   int32
		trigger bool
	}{
		{10000, 80, 7999, false}, {10000, 80, 8000, true},
		{20000, 80, 8000, false}, {20000, 80, 16000, true},
		{10000, 50, 5000, true}, {10000, 100, 9999, false},
		{10000, 100, 10000, true},
		{10001, 80, 8000, false}, {10001, 80, 8001, true},
		{1, 1, 0, false}, {1, 1, 1, true},
		{math.MaxInt64, 100, math.MaxInt32, false},
	} {
		t.Run(fmt.Sprintf("%d/%d/%d", tc.window, tc.percent, tc.usage), func(t *testing.T) {
			in := input()
			in.History[1].PromptTokens = tc.usage
			calls := 0
			b, err := NewCompressing(summaryFunc(func(context.Context, []domain.Message) (string, error) {
				calls++
				return "facts", nil
			}), CompressionPolicy{WindowTokens: tc.window, ThresholdPercent: tc.percent, KeepRecentRounds: 1})
			if err != nil {
				t.Fatal(err)
			}
			out, err := b.Prepare(context.Background(), in)
			if err != nil || (out.NewSnapshot != nil) != tc.trigger || (calls == 1) != tc.trigger {
				t.Fatal("incorrect model percentage trigger", calls, out.NewSnapshot, err)
			}
		})
	}
}
