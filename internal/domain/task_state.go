package domain

import (
	"reflect"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
)

// RequestCancel 记录首次取消意图，绝不把请求成功或本地超时视为远端已取消。
// 重复请求及已终态任务返回 false，保留原时间；调用方据此决定是否需要持久化。
// 提供方不支持取消时仍保留意图，后续实际成功或失败可正常落库。
// 所有状态操作仅用于调用方独占的内存副本，不提供锁，不增加 Version；持久化层
// 必须以原版本原子提交，冲突后重新读取。at 是调用方传入的本地记录时间。
func (t *Task) RequestCancel(at time.Time) (bool, error) {
	if err := t.Status.Validate(); err != nil {
		return false, err
	}
	if t.Status.IsTerminal() || t.CancelRequestedAt != nil {
		return false, nil
	}
	if err := stateTime(at); err != nil {
		return false, err
	}
	t.CancelRequestedAt = &at
	t.UpdatedAt = at
	return true, nil
}

// RecordObservationError 仅记录查询/订阅失败的安全说明，不改变状态、结果、进度、
// 游标、最后成功观察时间或取消意图。调用方负责将原始错误转换成安全说明，
// 不应直接传入可能携带凭据的 err.Error()；原始错误及未知提供方状态由适配层保留。
// 相同说明和终态上的迟到观察错误是无变化操作；恢复成功由 ApplyUpdate 清除说明。
// 这不是远端失败通知，重试只能继续观察已有句柄，不能重新 Execute。
func (t *Task) RecordObservationError(message string, at time.Time) (bool, error) {
	if err := t.Status.Validate(); err != nil {
		return false, err
	}
	if err := required("task.observation_error", message); err != nil {
		return false, err
	}
	if t.Status.IsTerminal() || t.ObservationError == message {
		return false, nil
	}
	if err := stateTime(at); err != nil {
		return false, err
	}
	t.ObservationError = message
	t.UpdatedAt = at
	return true, nil
}

// ApplyUpdate 接收适配器协调顺序后的完整观察快照（不是字段补丁），返回是否变化。
// 同状态仍可更新活动进度和游标；完全相同的快照不更新时间，避免轮询造成空写入。
// nil 与空切片按 Go 的结构相等规则区分。首次成功观察或错误恢复即使内容相同也有变化。
// 终态一旦确认，状态、结果、进度及游标全部冻结：相同快照幂等，任何不同快照报冲突。
// 终态可以无 Result（失败/取消可能只有状态）；结果补齐应由适配器在提交终态之前完成。
// 非终态不得携带最终 Result，Result 必须匹配原调用。状态以提供方确认值为准，
// 不根据 Result.Error、取消请求或观察错误推断；所有校验失败均保持原对象不变。
// 输入的可变切片会深拷贝，防止提供方重用缓冲区绕过终态保护。成功不改 Version、
// AppliedAt 或 Run 状态；结果消费与检查点的原子协调留给后续持久化/运行时。
func (t *Task) ApplyUpdate(update TaskUpdate, at time.Time) (bool, error) {
	if err := t.Status.ValidateTransition(update.Status); err != nil {
		return false, err
	}
	if update.Result != nil {
		if !update.Status.IsTerminal() {
			return false, apperrors.New(apperrors.ErrInvalidArgument, "task.result", "result requires terminal status")
		}
		if err := update.Result.ValidateForCall(t.Call); err != nil {
			return false, err
		}
	}
	same := t.Status == update.Status && t.ProviderCursor == update.Cursor &&
		reflect.DeepEqual(t.Progress, update.Progress) && reflect.DeepEqual(t.Result, update.Result)
	if t.Status.IsTerminal() {
		if same {
			return false, nil
		}
		return false, stateConflict("task.update")
	}
	if same && t.ObservationError == "" && !t.LastObservedAt.IsZero() {
		return false, nil
	}
	if err := stateTime(at); err != nil {
		return false, err
	}
	t.Status = update.Status
	t.Progress = cloneParts(update.Progress)
	t.Result = nil
	if update.Result != nil {
		result := *update.Result
		result.Parts = cloneParts(result.Parts)
		t.Result = &result
	}
	t.ProviderCursor = update.Cursor
	t.LastObservedAt = at
	t.ObservationError = ""
	t.UpdatedAt = at
	return true, nil
}

// stateTime 只拒绝缺失时间；不使用本地时间比较提供方消息顺序，避免时钟偏差
// 或校时被误认为远端回退。顺序由适配器游标/查询快照及存储版本共同约束。
func stateTime(at time.Time) error {
	if at.IsZero() {
		return apperrors.New(apperrors.ErrInvalidArgument, "task.observed_at", "recording time is required")
	}
	return nil
}

func cloneParts(parts []Part) []Part {
	if parts == nil {
		return nil
	}
	result := make([]Part, len(parts))
	copy(result, parts)
	for i := range result {
		if parts[i].Data != nil {
			result[i].Data = make([]byte, len(parts[i].Data))
			copy(result[i].Data, parts[i].Data)
		}
	}
	return result
}
