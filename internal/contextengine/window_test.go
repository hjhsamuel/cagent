package contextengine

import (
	"context"
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
	out, err := builder(t).Prepare(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ids(out.Messages), "old-user") {
		t.Fatal("protected user lost")
	}
	in.History[1].Sequence = 4
	in.HistoryThrough = 4
	if _, err = builder(t).Prepare(context.Background(), in); err == nil {
		t.Fatal("tail gap accepted")
	}
	in.History[1].Sequence = 3
	in.HistoryThrough = 3
	in.Snapshot.ValidatedThrough = 0
	if _, err = builder(t).Prepare(context.Background(), in); err == nil {
		t.Fatal("legacy snapshot authorized gaps")
	}
}
func TestArchiveCompletedRoundKeepsCurrentAndOriginalHistory(t *testing.T) {
	in := toolInput()
	in.History[3].PromptTokens = 8192
	in.ArchiveCompleted = true
	in.RunStates = map[string]domain.RunStatus{"old": domain.RunCompleted}
	before := cloneMessages(in.History)
	b, err := NewCompressing(summaryFunc(func(context.Context, []domain.Message) (string, error) {
		return "constraint: original requirements; tool result facts", nil
	}), CompressionPolicy{WindowTokens: 10240, ThresholdPercent: 80, KeepRecentRounds: 1})
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
