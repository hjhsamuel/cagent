package contextengine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hjhsamuel/cagent/internal/domain"
)

func TestVerifiedWindowPreservesUsersAndRejectsTailGaps(t *testing.T) {
	in := input()
	in.PreserveUsers = true
	in.Snapshot = snapshot(in, 2)
	in.Snapshot.ValidatedThrough = 2
	in.VerifiedPrefix = 2
	in.HistoryThrough = 3
	in.History = []domain.Message{in.History[0], in.History[2]}
	out, err := builder(t, counterFunc(testCount)).Prepare(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ids(out.Messages), "old-user") {
		t.Fatal("protected user lost")
	}
	in.History[1].Sequence = 4
	in.HistoryThrough = 4
	if _, err = builder(t, counterFunc(testCount)).Prepare(context.Background(), in); err == nil {
		t.Fatal("tail gap accepted")
	}
	in.History[1].Sequence = 3
	in.HistoryThrough = 3
	in.Snapshot.ValidatedThrough = 0
	if _, err = builder(t, counterFunc(testCount)).Prepare(context.Background(), in); err == nil {
		t.Fatal("legacy snapshot authorized gaps")
	}
}
func TestArchiveCompletedRoundKeepsCurrentAndOriginalHistory(t *testing.T) {
	in := toolInput()
	in.ArchiveCompleted = true
	in.RunStates = map[string]domain.RunStatus{"old": domain.RunCompleted}
	before := cloneMessages(in.History)
	b, err := NewCompressing(counterFunc(testCount), summaryFunc(func(context.Context, []domain.Message) (string, error) {
		return "constraint: original requirements; tool result facts", nil
	}), CompressionPolicy{1, 1})
	if err != nil {
		t.Fatal(err)
	}
	out, err := b.Prepare(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if out.NewSnapshot == nil || !out.NewSnapshot.Archived {
		t.Fatal("complete prefix not recorded")
	}
	for _, m := range out.Messages {
		if m.RunID == "old" {
			t.Fatal("archived tool group retained", m.ID)
		}
	}
	if !strings.Contains(ids(out.Messages), "current-user") || len(in.History) != len(before) {
		t.Fatal("current input or persistent source lost")
	}
	for i := range in.History {
		if in.History[i].ID != before[i].ID {
			t.Fatal("source modified")
		}
	}
}
func TestSegmentSummarySplitsOnlyBudgetRejection(t *testing.T) {
	source := input().History
	calls := 0
	text, err := summarizeSegments(context.Background(), summaryFunc(func(_ context.Context, m []domain.Message) (string, error) {
		calls++
		if len(m) > 2 {
			return "", ErrBudgetExceeded
		}
		return "facts", nil
	}), source)
	if err != nil || text != "facts" || calls != 4 {
		t.Fatal(text, err, calls)
	}
	calls = 0
	failure := errors.New("provider failed")
	_, err = summarizeSegments(context.Background(), summaryFunc(func(context.Context, []domain.Message) (string, error) { calls++; return "", failure }), source)
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatal("model failure retried", err, calls)
	}
}
