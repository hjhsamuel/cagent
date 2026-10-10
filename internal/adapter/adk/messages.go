// Package adk 将领域请求桥接到固定版本的 Google ADK；领域层不引用 SDK 类型。
package adk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hjhsamuel/cagent/internal/agent"
	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

func invalid(field string) error {
	return apperrors.New(apperrors.ErrInvalidArgument, field, "invalid model adapter argument")
}
func unsupported(field string) error {
	return apperrors.New(apperrors.ErrUnsupported, field, "model adapter capability is not supported")
}

var ErrModel = errors.New("model execution failed")

// safeError 的展示不含 SDK 错误体/URL/密钥；保留原始原因以供 errors.Is/As 内部诊断。
func safeError(err error) error {
	if err == nil {
		return nil
	}
	// 不确定的工具启动可能同时带有超时/取消原因；不能在规整 context 错误时
	// 丢失它，否则应用无法记录“未获得句柄，禁止盲目重试”的持久诊断。
	if errors.Is(err, agent.ErrUncertain) {
		return &modelError{cause: err}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return &modelError{cause: err}
}

type modelError struct{ cause error }

func (e *modelError) Error() string        { return ErrModel.Error() }
func (e *modelError) Unwrap() error        { return e.cause }
func (e *modelError) Is(target error) bool { return target == ErrModel }

// Format 不展开密钥或提供方诊断；公共日志只展示稳定说明。
func (e *modelError) Format(s fmt.State, _ rune) { fmt.Fprint(s, e.Error()) }

// mapMessages 统一执行与恢复的消息映射，系统消息只进入 SystemInstruction。
// 工具历史的 ID 按原 Run+CallID 映射，避免不同运行的局部 ID 冲突；原 Task 路由不变。
// P6 支持文本和既有结构化工具历史；多模态/新工具执行留给 P8，未知内容明确拒绝。
func mapMessages(messages []domain.Message, name string) (*model.LLMRequest, error) {
	return mapMessagesForRun(messages, name, "")
}

// 当前 Run 的调用保留执行 ID，旧 Run 的局部 ID 仍隔离映射。
func mapMessagesForRun(messages []domain.Message, name, activeRun string) (*model.LLMRequest, error) {
	req := &model.LLMRequest{Model: name, Config: &genai.GenerateContentConfig{}}
	type key struct{ run, id string }
	names := map[key]string{}
	idFor := func(run, id string) string {
		if run == activeRun && activeRun != "" {
			return id
		}
		return callID(run, id)
	}
	for _, m := range messages {
		role := "user"
		switch m.Role {
		case domain.RoleSystem:
			role = "system"
		case domain.RoleUser, domain.RoleTool:
		case domain.RoleAssistant:
			role = "model"
		default:
			return nil, invalid("message.role")
		}
		c := &genai.Content{Role: role}
		for _, p := range m.Parts {
			part := &genai.Part{}
			switch p.Kind {
			case "", domain.PartText:
				if p.Data != nil || p.URI != "" || p.MIMEType != "" || p.ToolCallID != "" || p.ToolName != "" {
					return nil, unsupported("message.part")
				}
				part.Text = p.Text
			case domain.PartToolCall:
				if p.Text != "" || p.URI != "" || p.MIMEType != "" {
					return nil, unsupported("tool.call.part")
				}
				if m.Role != domain.RoleAssistant || p.ToolCallID == "" || p.ToolName == "" {
					return nil, invalid("tool.call")
				}
				args, err := object(p.Data)
				if err != nil {
					return nil, err
				}
				names[key{m.RunID, p.ToolCallID}] = p.ToolName
				part.FunctionCall = &genai.FunctionCall{ID: idFor(m.RunID, p.ToolCallID), Name: p.ToolName, Args: args}
			case domain.PartToolResult:
				name := names[key{m.RunID, p.ToolCallID}]
				if m.Role != domain.RoleTool || name == "" || (p.ToolName != "" && name != p.ToolName) {
					return nil, invalid("tool.result")
				}
				result := map[string]any{"output": p.Text}
				if p.Data != nil {
					var err error
					result, err = object(p.Data)
					if err != nil {
						return nil, err
					}
					if p.Text != "" {
						result = map[string]any{"text": p.Text, "data": result}
					}
				}
				if p.URI != "" || p.MIMEType != "" {
					return nil, unsupported("tool.result.artifact")
				}
				part.FunctionResponse = &genai.FunctionResponse{ID: idFor(m.RunID, p.ToolCallID), Name: name, Response: result}
			default:
				return nil, unsupported("message.part.kind")
			}
			c.Parts = append(c.Parts, part)
		}
		if len(c.Parts) == 0 {
			return nil, invalid("message.parts")
		}
		if role == "system" {
			if req.Config.SystemInstruction == nil {
				req.Config.SystemInstruction = &genai.Content{Role: "system"}
			}
			req.Config.SystemInstruction.Parts = append(req.Config.SystemInstruction.Parts, c.Parts...)
		} else {
			req.Contents = append(req.Contents, c)
		}
	}
	if len(req.Contents) == 0 {
		return nil, invalid("messages")
	}
	return req, nil
}
func callID(run, id string) string {
	data, _ := json.Marshal([]string{run, id})
	h := sha256.Sum256(data)
	return "call_" + hex.EncodeToString(h[:16])
}
func object(data []byte) (map[string]any, error) {
	var out map[string]any
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil || out == nil {
		return nil, invalid("part.data")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, invalid("part.data")
	}
	return out, nil
}
