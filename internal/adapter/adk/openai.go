package adk

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"math"
	"net/http"
	"strings"

	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/observability"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// OpenAIModel 通过官方 OpenAI SDK 的 Chat Completions 实现 ADK model.LLM。
// 模型、URL、密钥均在构造时显式传入；不同实例互不共享动态配置。
// 不采用 ADK openaimodel 的 Responses 路径，以兼容只实现 Chat Completions 的服务。
type OpenAIModel struct {
	capacity *observability.Gate
	client   openai.Client
	keys     *config.KeyPool
	thinking *config.Thinking
	name     string
}

// NewOpenAI 不发网络请求，不回退到 SDK 环境变量。显式关闭自动重试，避免未知
// 提交结果导致重复计费/生成；重定向被禁止，API 密钥不会被转发到不同地址。
// HTTPClient 可注入测试 Transport，但配置的超时和重定向策略始终生效。
func NewOpenAI(cfg config.Agent, client *http.Client) (*OpenAIModel, error) {
	if err := cfg.ValidateOpenAI(); err != nil {
		return nil, err
	}
	httpClient := &http.Client{}
	if client != nil {
		*httpClient = *client
	}
	httpClient.Timeout = cfg.RequestTimeout
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	sdk := openai.NewClient(option.WithAPIKey(cfg.APIKey), option.WithBaseURL(strings.TrimRight(cfg.BaseURL, "/")+"/"), option.WithHTTPClient(httpClient), option.WithMaxRetries(0))
	var thinking *config.Thinking
	if cfg.Thinking != nil && cfg.Thinking.Enabled {
		// 深拷贝，避免调用方修改共享 JSON 参数影响并发请求。
		data, err := json.Marshal(cfg.Thinking)
		if err != nil {
			return nil, invalid("model.thinking")
		}
		thinking = &config.Thinking{}
		if err := decodeJSON(data, thinking); err != nil {
			return nil, invalid("model.thinking")
		}
	}
	return &OpenAIModel{
		capacity: observability.NewGate(config.Defaults().Capacity.Models, "model"),
		client:   sdk,
		keys:     cfg.Keys,
		thinking: thinking,
		name:     cfg.Model,
	}, nil
}

// SetCapacity 仅允许启动装配时调用；主模型与摘要模型共享同一闸门。
func (m *OpenAIModel) SetCapacity(g *observability.Gate) { m.capacity = g }
func (m *OpenAIModel) Name() string                      { return m.name }

// wireMessages 与实际请求使用同一转换。结构化工具历史保持 assistant/tool 角色，
// 不将工具结果合并为用户文本；P8 的可执行工具定义由 wireTools 单独转换。
func wireMessages(req *model.LLMRequest) ([]openai.ChatCompletionMessageParamUnion, error) {
	if req == nil || req.Config == nil {
		return nil, invalid("model.request")
	}
	var out []openai.ChatCompletionMessageParamUnion
	contents := req.Contents
	if req.Config.SystemInstruction != nil {
		contents = append([]*genai.Content{req.Config.SystemInstruction}, contents...)
	}
	for _, c := range contents {
		if c == nil {
			return nil, invalid("content")
		}
		role := c.Role
		if role == "model" {
			role = "assistant"
		}
		if role != "system" && role != "assistant" && role != "user" {
			return nil, invalid("content.role")
		}
		var text strings.Builder
		var calls []map[string]any
		var results []map[string]any
		for _, p := range c.Parts {
			if p == nil {
				return nil, invalid("content.part")
			}
			if p.InlineData != nil || p.FileData != nil || p.Thought || len(p.ThoughtSignature) > 0 || p.ExecutableCode != nil || p.CodeExecutionResult != nil {
				return nil, unsupported("content.part")
			}
			text.WriteString(p.Text)
			if f := p.FunctionCall; f != nil {
				if role != "assistant" {
					return nil, invalid("content.call")
				}
				args, e := json.Marshal(f.Args)
				if e != nil {
					return nil, invalid("content.args")
				}
				calls = append(calls, map[string]any{"id": f.ID, "type": "function", "function": map[string]any{"name": f.Name, "arguments": string(args)}})
			}
			if f := p.FunctionResponse; f != nil {
				if role != "user" {
					return nil, invalid("content.result")
				}
				body, e := json.Marshal(f.Response)
				if e != nil {
					return nil, invalid("content.result")
				}
				results = append(results, map[string]any{"role": "tool", "tool_call_id": f.ID, "content": string(body)})
			}
		}
		if len(results) > 0 {
			if text.Len() > 0 || len(calls) > 0 {
				return nil, unsupported("content.mixed_result")
			}
			for _, v := range results {
				out = append(out, param.Override[openai.ChatCompletionMessageParamUnion](v))
			}
			continue
		}
		v := map[string]any{"role": role, "content": text.String()}
		if len(calls) > 0 {
			v["tool_calls"] = calls
		}
		out = append(out, param.Override[openai.ChatCompletionMessageParamUnion](v))
	}
	return out, nil
}

// GenerateContent 对 SSE 增量逐片背压，结束时仅输出一个完整 LLMResponse。
// 缺失 stop、截断、拒绝及中途错误都不伪造成功；关闭流释放 HTTP 连接。
// 工具轮次要求完整 tool_calls 响应，参数解码成功后才交给 ADK 执行。
func (m *OpenAIModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		ctx, finish := observability.Default.Start(ctx, "model")
		var failure error
		defer func() { finish(failure) }()
		original := yield
		yield = func(r *model.LLMResponse, e error) bool {
			if e != nil {
				failure = e
			}
			return original(r, e)
		}
		release, capacityErr := m.capacity.Try(ctx)
		if capacityErr != nil {
			yield(nil, capacityErr)
			return
		}
		defer release()
		messages, err := wireMessages(req)
		if err != nil {
			yield(nil, err)
			return
		}
		if req.Model != "" && req.Model != m.name {
			yield(nil, invalid("model.name"))
			return
		}
		definitions, e := wireTools(req)
		if e != nil {
			yield(nil, e)
			return
		}
		params := openai.ChatCompletionNewParams{Model: m.name, Messages: messages}
		for _, d := range definitions {
			params.Tools = append(params.Tools, param.Override[openai.ChatCompletionToolUnionParam](d))
		}
		// 工具轮次使用完整响应，避免在参数 JSON 尚未接收完整时执行副作用。
		// 无工具的普通文本仍保留原有 SSE 增量路径。
		if len(definitions) > 0 {
			stream = false
		}
		var requestOptions []option.RequestOption
		if m.keys != nil {
			key, err := m.keys.Pick()
			if err != nil {
				yield(nil, err)
				return
			}
			requestOptions = append(requestOptions, option.WithAPIKey(key))
		}
		if m.thinking != nil {
			requestOptions = append(requestOptions, option.WithJSONSet(m.thinking.Key, m.thinking.Value))
		}
		if !stream {
			resp, e := m.client.Chat.Completions.New(ctx, params, requestOptions...)
			if e != nil {
				yield(nil, safeError(e))
				return
			}
			if len(resp.Choices) == 1 && resp.Choices[0].FinishReason == "tool_calls" && len(definitions) > 0 {
				choice := resp.Choices[0]
				if choice.Message.Refusal != "" || len(choice.Message.ToolCalls) == 0 || len(choice.Message.ToolCalls) > 64 {
					yield(nil, safeError(ErrModel))
					return
				}
				content := &genai.Content{Role: "model"}
				seen := map[string]bool{}
				for _, tc := range choice.Message.ToolCalls {
					f := tc.AsFunction()
					known := false
					for _, d := range definitions {
						if d["function"].(map[string]any)["name"] == f.Function.Name {
							known = true
							break
						}
					}
					if !known || f.ID == "" || f.Function.Name == "" || seen[f.ID] || len(f.Function.Arguments) > 1<<20 {
						yield(nil, invalid("model.tool_call"))
						return
					}
					seen[f.ID] = true
					args, err := object([]byte(f.Function.Arguments))
					if err != nil {
						yield(nil, err)
						return
					}
					content.Parts = append(content.Parts, &genai.Part{FunctionCall: &genai.FunctionCall{ID: f.ID, Name: f.Function.Name, Args: args}})
				}
				response := finalResponse("", resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
				response.Content = content
				response.FinishReason = ""
				yield(response, nil)
				return
			}
			if len(resp.Choices) != 1 || resp.Choices[0].FinishReason != "stop" || len(resp.Choices[0].Message.ToolCalls) > 0 || resp.Choices[0].Message.Refusal != "" || len(resp.Choices[0].Message.Content) > 8<<20 {
				yield(nil, safeError(ErrModel))
				return
			}
			yield(finalResponse(resp.Choices[0].Message.Content, resp.Usage.PromptTokens, resp.Usage.CompletionTokens), nil)
			return
		}
		params.StreamOptions = openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)}
		s := m.client.Chat.Completions.NewStreaming(ctx, params, requestOptions...)
		defer s.Close()
		var answer strings.Builder
		stopped := false
		var prompt, completion int64
		for s.Next() {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			chunk := s.Current()
			if chunk.Usage.PromptTokens > 0 {
				prompt = chunk.Usage.PromptTokens
				completion = chunk.Usage.CompletionTokens
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			if len(chunk.Choices) != 1 || chunk.Choices[0].Index != 0 || stopped {
				yield(nil, safeError(ErrModel))
				return
			}
			c := chunk.Choices[0]
			if c.Delta.Refusal != "" || len(c.Delta.ToolCalls) > 0 || c.Delta.FunctionCall.Name != "" {
				yield(nil, unsupported("model.output"))
				return
			}
			if c.Delta.Content != "" {
				if answer.Len()+len(c.Delta.Content) > 8<<20 {
					yield(nil, invalid("model.output_size"))
					return
				}
				answer.WriteString(c.Delta.Content)
				if !yield(&model.LLMResponse{Content: genai.NewContentFromText(c.Delta.Content, "model"), Partial: true}, nil) {
					return
				}
			}
			if c.FinishReason != "" {
				if c.FinishReason != "stop" {
					yield(nil, safeError(ErrModel))
					return
				}
				stopped = true
			}
		}
		if err := s.Err(); err != nil {
			yield(nil, safeError(err))
			return
		}
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		if !stopped || answer.Len() == 0 {
			yield(nil, safeError(ErrModel))
			return
		}
		yield(finalResponse(answer.String(), prompt, completion), nil)
	}
}

// wireTools 将工具声明映射为实际发送的 OpenAI 请求结构。
func wireTools(req *model.LLMRequest) ([]map[string]any, error) {
	var out []map[string]any
	for _, group := range req.Config.Tools {
		if group == nil || len(group.FunctionDeclarations) == 0 {
			return nil, unsupported("model.tools")
		}
		for _, f := range group.FunctionDeclarations {
			if f == nil || f.Name == "" {
				return nil, invalid("model.tools")
			}
			schema := f.ParametersJsonSchema
			if schema == nil {
				schema = f.Parameters
			}
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			out = append(out, map[string]any{"type": "function", "function": map[string]any{"name": f.Name, "description": f.Description, "parameters": schema}})
		}
	}
	return out, nil
}
func finalResponse(text string, prompt, completion int64) *model.LLMResponse {
	return &model.LLMResponse{Content: genai.NewContentFromText(text, "model"), TurnComplete: true, FinishReason: genai.FinishReasonStop, UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: int32(min(prompt, math.MaxInt32)), CandidatesTokenCount: int32(min(completion, math.MaxInt32))}}
}

// Format 不展开密钥或提供方诊断；公共日志只展示稳定说明。
func (e *modelError) Format(s fmt.State, _ rune) { fmt.Fprint(s, e.Error()) }
