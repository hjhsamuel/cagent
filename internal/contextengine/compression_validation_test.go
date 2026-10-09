package contextengine

import (
	"context"
	"github.com/hjhsamuel/cagent/internal/domain"
	"testing"
)

// 校验必须先于摘要网络调用，包括打算因策略版本变化而忽略的快照。
func TestCompressionRejectsInvalidHistoryBeforeSummary(t *testing.T) {
	for _, change := range []func(*Input){
		func(in *Input) { in.History[1].Sequence = 99 },
		func(in *Input) { in.History[2].Parts[0].ToolCallID = "missing" },
		func(in *Input) {
			in.Snapshot = snapshot(*in, 4)
			in.Snapshot.PolicyVersion = "old"
			in.Snapshot.Scope.UserID = "foreign"
		},
		func(in *Input) {
			in.Snapshot = snapshot(*in, 4)
			in.Snapshot.PolicyVersion = "old"
			in.Snapshot.Version = 0
		},
		func(in *Input) { in.Snapshot = snapshot(*in, 99); in.Snapshot.PolicyVersion = "old" },
	} {
		in := compressInput()
		change(&in)
		b, _ := NewCompressing(summaryFunc(func(context.Context, []domain.Message) (string, error) {
			t.Error("invalid data sent to summary")
			return "facts", nil
		}), CompressionPolicy{WindowTokens: 10240, ThresholdPercent: 80, KeepRecentRounds: 1})
		out, e := b.Prepare(context.Background(), in)
		if e == nil || len(out.Messages) != 0 {
			t.Fatal("invalid input accepted")
		}
	}
}
