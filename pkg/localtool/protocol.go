// Package localtool 定义可执行 local tool 的 stdin/stdout JSON 协议。
// 工具可使用任意语言实现；本包提供 Go 实现的协议类型和入口辅助函数。
package localtool

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const ProtocolVersion = 1

type Scope struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
}

type Caller struct {
	AgentID            string `json:"agent_id"`
	InvocationID       string `json:"invocation_id"`
	ParentInvocationID string `json:"parent_invocation_id,omitempty"`
}

// Request 中 arguments 来自模型，config 来自可信管理配置。
type Request struct {
	ProtocolVersion int             `json:"protocol_version"`
	Name            string          `json:"name"`
	CallID          string          `json:"call_id"`
	Scope           Scope           `json:"scope"`
	SessionID       string          `json:"session_id"`
	RunID           string          `json:"run_id"`
	Caller          Caller          `json:"caller"`
	IdempotencyKey  string          `json:"idempotency_key,omitempty"`
	Arguments       json.RawMessage `json:"arguments"`
	Config          json.RawMessage `json:"config"`
}

// Response 是唯一 stdout 文档；业务错误仍以退出码 0 返回，日志写 stderr。
// 工具不能指定结果的调用 ID，服务始终关联原调用。
type Response struct {
	ProtocolVersion int    `json:"protocol_version"`
	Text            string `json:"text"`
	Error           string `json:"error,omitempty"`
}

// Decode 拒绝未知字段和多个 JSON 文档，便于工具校验配置及参数。
func Decode(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}

// Serve 处理一次调用。返回 Go error 表示协议或运行失败，由 main 返回非零退出码。
func Serve(in io.Reader, out io.Writer, handle func(Request) (Response, error)) error {
	const maxRequestBytes = 12 << 20
	data, err := io.ReadAll(io.LimitReader(in, maxRequestBytes+1))
	if err != nil || len(data) > maxRequestBytes {
		return errors.New("cannot read local tool request")
	}
	var req Request
	if Decode(data, &req) != nil || req.ProtocolVersion != ProtocolVersion || req.Name == "" || !object(req.Arguments) || !object(req.Config) {
		return errors.New("invalid local tool request")
	}
	response, err := handle(req)
	if err != nil {
		return err
	}
	response.ProtocolVersion = ProtocolVersion
	return json.NewEncoder(out).Encode(response)
}

func object(data []byte) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(data, &value) == nil && value != nil
}
