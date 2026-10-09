package contextengine

import (
	"context"
	"math"
	"strings"

	"github.com/hjhsamuel/cagent/internal/domain"
)

// Summarizer 只生成派生文本，不选择覆盖水位或执行工具；历史是不可信资料。
type Summarizer interface {
	Summarize(context.Context, []domain.Message) (string, error)
}

// CompressionPolicy 依据最新 assistant 响应报告的 prompt_tokens 触发摘要。
// 近期轮数只决定摘要范围，包含当前 Run。
// 一轮从 user 消息开始，保留整个尾部，因此不会从 assistant 消息中间截断。
type CompressionPolicy struct {
	WindowTokens     int64
	ThresholdPercent int
	KeepRecentRounds int
}

// CompressingBuilder 可并发复用；摘要失败时保留原输入，不重复生成或裁剪历史。
type CompressingBuilder struct {
	builder *Builder
	summary Summarizer
	policy  CompressionPolicy
}

// NewCompressing 复制用量阈值和轮次策略，不读取环境或调用模型。
func NewCompressing(summary Summarizer, policy CompressionPolicy) (*CompressingBuilder, error) {
	if summary == nil || policy.WindowTokens <= 0 || policy.ThresholdPercent < 1 || policy.ThresholdPercent > 100 || policy.KeepRecentRounds < 1 {
		return nil, invalid("context.compression")
	}
	return &CompressingBuilder{builder: New(), summary: summary, policy: policy}, nil
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
		if previous.Version <= 0 || previous.ThroughSequence <= 0 || len(in.History) == 0 || previous.ThroughSequence > in.History[len(in.History)-1].Sequence || strings.TrimSpace(previous.Summary) == "" || strings.TrimSpace(previous.PolicyVersion) == "" {
			return Prepared{}, invalid("context.snapshot")
		}
	}
	if previous != nil && (previous.PolicyVersion != in.PolicyVersion || previous.ThroughSequence > through) {
		in.Snapshot = nil
	}
	base, err := b.builder.Prepare(ctx, in)
	if err != nil {
		return Prepared{}, err
	}
	fallback := func() (Prepared, error) {
		if err := ctx.Err(); err != nil {
			return Prepared{}, err
		}
		return base, nil
	}
	// 最新一次响应未报告用量时不回退到旧响应或本地估算，不累加历史用量。
	var promptTokens int32
	for i := len(in.History) - 1; i >= 0; i-- {
		if in.History[i].Role == domain.RoleAssistant {
			promptTokens = in.History[i].PromptTokens
			break
		}
	}
	// 向上取整保证达到指定比例才触发；拆分整数运算避免大上下文上限溢出。
	window, percent := b.policy.WindowTokens, int64(b.policy.ThresholdPercent)
	threshold := window/100*percent + (window%100*percent+99)/100
	if int64(promptTokens) < threshold {
		return fallback()
	}
	if through == 0 || (previous != nil && (through < previous.ThroughSequence || (through == previous.ThroughSequence && in.Snapshot != nil))) {
		return fallback()
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
		return fallback()
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
	text, err := b.summary.Summarize(ctx, cloneMessages(prefix))
	if err != nil {
		return fallback()
	}
	if strings.TrimSpace(text) == "" {
		return fallback()
	}
	next := domain.ContextSnapshot{
		Scope:            in.Session.Scope,
		ID:               "context-" + in.RunID,
		SessionID:        in.Session.ID,
		ThroughSequence:  through,
		ValidatedThrough: through,
		Archived:         archiveValid,
		Summary:          text,
		PolicyVersion:    in.PolicyVersion,
		Version:          version + 1,
	}
	in.Snapshot = &next
	prepared, err := b.builder.Prepare(ctx, in)
	if err != nil {
		return fallback()
	}
	next.Version = version
	prepared.NewSnapshot = &next
	return prepared, nil
}
