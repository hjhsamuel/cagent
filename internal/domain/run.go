package domain

import "time"

// RunStatus 是本地运行状态，与远端 TaskStatus 独立；状态规则见 status.go。
// 取消 Run 后停止模型生成，但不把仍在执行的远端任务强制标为 cancelled。
type RunStatus string

const (
	RunQueued    RunStatus = "queued"
	RunRunning   RunStatus = "running"
	RunWaiting   RunStatus = "waiting_tool"
	RunCompleted RunStatus = "completed"
	RunFailed    RunStatus = "failed"
	RunCancelled RunStatus = "cancelled"
)

type Run struct {
	Scope          Scope
	ID             string
	SessionID      string
	IdempotencyKey string
	Status         RunStatus
	Version        int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
	TerminatedAt   time.Time
}

type EventKind string

const (
	EventRunStarted EventKind = "run.started"
	EventTextDelta  EventKind = "message.delta"
	// EventMessageCompleted 表示完整消息已落库，不应再次作为增量拼接。
	EventMessageCompleted EventKind = "message.completed"
	EventToolStarted      EventKind = "tool.started"
	EventToolWaiting      EventKind = "tool.waiting"
	EventToolProgress     EventKind = "tool.progress"
	EventToolFinished     EventKind = "tool.finished"
	EventRunCompleted     EventKind = "run.completed"
	EventRunFailed        EventKind = "run.failed"
	EventRunCancelled     EventKind = "run.cancelled"
)

// Event is a durable application event, independent of SSE encoding.
// Sequence is monotonically increasing within a scoped run and supports replay.
type Event struct {
	Scope     Scope
	RunID     string
	Sequence  int64
	Kind      EventKind
	Data      []byte
	CreatedAt time.Time
}
