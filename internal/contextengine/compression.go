package contextengine

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/hjhsamuel/cagent/internal/domain"
)

// Summarizer 只生成派生文本，不选择覆盖水位，也不执行工具。实现必须自行限制
// 摘要请求的输入、输出和超时；历史是不可信资料，不能提升成系统指令。
type Summarizer interface {
	Summarize(context.Context, []domain.Message) (string, error)
}

// CompressionPolicy 的阈值按可用输入预算百分比计算；近期轮数包含当前 Run。
// 一轮从 user 消息开始，保留整个尾部，因此不会从 assistant 消息中间截断。
type CompressionPolicy struct {
	ThresholdPercent int
	KeepRecentRounds int
}

// CompressingBuilder 可并发复用，摘要预算拒绝时允许最多 32 次受限分段尝试。失败只回退到已经
// 通过预算检查的输入；无可用回退则返回错误，不裁掉用户要求或工具关联。
type CompressingBuilder struct {
	builder *Builder
	summary Summarizer
	policy  CompressionPolicy
}

// NewCompressing 复制策略值，不读环境、不调用模型；计数器与摘要实现必须支持并发。
// 非法阈值或保留轮数立即拒绝，避免运行到预算上限才暴露装配错误。
func NewCompressing(counter TokenCounter, summary Summarizer, policy CompressionPolicy) (*CompressingBuilder, error) {
	if summary == nil || policy.ThresholdPercent < 1 || policy.ThresholdPercent > 100 || policy.KeepRecentRounds < 1 {
		return nil, invalid("context.compression")
	}
	b, err := New(counter)
	if err != nil {
		return nil, err
	}
	return &CompressingBuilder{builder: b, summary: summary, policy: policy}, nil
}

// Compatible validated prefixes use incremental summaries. A policy change
// rebuilds from original history; persistence still uses the previous CAS version.
// 旧快照版本始终作为保存的 CAS 基线；策略变化可以重算同一水位，但不能倒退。
func (b *CompressingBuilder) Prepare(ctx context.Context, in Input) (Prepared, error) {
	in.PreserveUsers = !in.ArchiveCompleted
	var through int64
	rounds := 0
	for i := len(in.History) - 1; i >= 0; i-- {
		if in.History[i].Role == domain.RoleUser {
			rounds++
			if rounds == b.policy.KeepRecentRounds {
				through = in.History[i].Sequence - 1
				break
			}
		}
	}
	// 当前运行所有消息都是不可压缩内容；摘要只能覆盖它之前的已完成历史。
	for _, m := range in.History {
		if m.RunID == in.RunID {
			if through >= m.Sequence {
				through = m.Sequence - 1
			}
			break
		}
	}
	previous := in.Snapshot
	// 策略升级只允许忽略不兼容内容，不能借此跳过作用域、版本或水位校验。
	if previous != nil {
		if err := previous.ValidateForSession(in.Session); err != nil {
			return Prepared{}, err
		}
		if previous.Version <= 0 || previous.ThroughSequence <= 0 || len(in.History) == 0 || previous.ThroughSequence > in.History[len(in.History)-1].Sequence || previous.TokenEstimate < 0 || strings.TrimSpace(previous.Summary) == "" || strings.TrimSpace(previous.PolicyVersion) == "" {
			return Prepared{}, invalid("context.snapshot")
		}
	}
	if previous != nil && (previous.PolicyVersion != in.PolicyVersion || previous.ThroughSequence > through) {
		in.Snapshot = nil
	}
	base, baseErr := b.builder.Prepare(ctx, in)
	if baseErr != nil && !errors.Is(baseErr, ErrBudgetExceeded) {
		return Prepared{}, baseErr
	}
	limit, _ := in.Budget.InputLimit() // Builder 已校验预算。
	threshold := (limit/100)*b.policy.ThresholdPercent + ((limit%100)*b.policy.ThresholdPercent+99)/100
	if baseErr == nil && base.EstimatedTokens < threshold {
		return base, nil
	}
	fallback := func(err error) (Prepared, error) {
		if e := ctx.Err(); e != nil {
			return Prepared{}, e
		}
		if baseErr == nil {
			return base, nil
		}
		return Prepared{}, err
	}
	if through == 0 || (previous != nil && (through < previous.ThroughSequence || (through == previous.ThroughSequence && in.Snapshot != nil))) {
		return fallback(baseErr)
	}
	// 没有可替换的普通 assistant 消息时，摘要只会增加开销，直接保持原输入。
	removable := false
	var prefix []domain.Message
	for _, m := range in.History {
		if m.Sequence > through {
			break
		}
		prefix = append(prefix, m)
		plain := m.Role == domain.RoleAssistant
		for _, p := range m.Parts {
			if p.Kind == domain.PartToolCall || p.Kind == domain.PartToolResult {
				plain = false
			}
		}
		removable = removable || plain
		if in.ArchiveCompleted && in.RunStates[m.RunID].IsTerminal() {
			removable = true
		}
	}
	if !removable {
		return fallback(baseErr)
	}
	version := int64(0)
	archiveValid := in.ArchiveCompleted
	prefixRuns := make(map[string]bool)
	if archiveValid {
		for _, m := range prefix {
			prefixRuns[m.RunID] = true
			if !in.RunStates[m.RunID].IsTerminal() {
				archiveValid = false
			}
		}
		for _, m := range in.History {
			if m.Sequence > through && prefixRuns[m.RunID] {
				archiveValid = false
			}
		}
	}
	if previous != nil {
		version = previous.Version
	}
	if version < 0 || version == math.MaxInt64 {
		return Prepared{}, invalid("context.snapshot.version")
	}
	if in.VerifiedPrefix > 0 && in.Snapshot != nil {
		source := []domain.Message{{Scope: in.Session.Scope, ID: "previous-summary", SessionID: in.Session.ID, Role: domain.RoleUser, Parts: []domain.Part{{Kind: domain.PartText, Text: "此前已验证历史的派生摘要：\n" + in.Snapshot.Summary}}}}
		for _, m := range prefix {
			if m.Sequence > in.VerifiedPrefix {
				source = append(source, m)
			}
		}
		prefix = source
	}
	text, err := summarizeSegments(ctx, b.summary, cloneMessages(prefix))
	if err != nil {
		return fallback(err)
	}
	if strings.TrimSpace(text) == "" {
		return fallback(invalid("context.summary"))
	}
	next := domain.ContextSnapshot{Scope: in.Session.Scope, ID: "context-" + in.RunID, SessionID: in.Session.ID,
		ThroughSequence: through, ValidatedThrough: through, Archived: archiveValid, Summary: text, PolicyVersion: in.PolicyVersion, Version: version + 1}
	in.Snapshot = &next
	prepared, err := b.builder.Prepare(ctx, in)
	if err != nil {
		return fallback(err)
	}
	// 摘要不减小上下文时不保存派生噪声；仍必须遵守原有输入预算。
	if baseErr == nil && prepared.EstimatedTokens >= base.EstimatedTokens {
		return base, nil
	}
	next.Version = version
	next.TokenEstimate = prepared.EstimatedTokens
	prepared.NewSnapshot = &next
	return prepared, nil
}
