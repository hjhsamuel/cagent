package adk

import (
	"context"
	"encoding/json"
	"iter"
	"net/http"
	"strings"

	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/observability"
	openaimodel "github.com/hjhsamuel/cagent/pkg/llm/openai"
	"github.com/openai/openai-go/v3/option"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// NewOpenAI 装配 pkg/llm/openai 的 ADK 模型，保留应用的配置与容量策略。
// 构造不调用网络；显式配置凭据、地址、超时，关闭重试和重定向。
// 主模型与摘要模型可在构造时传入同一容量闸门。
func NewOpenAI(cfg config.Agent, client *http.Client, gates ...*observability.Gate) (model.LLM, error) {
	if err := cfg.ValidateOpenAI(); err != nil {
		return nil, err
	}
	if len(gates) > 1 {
		return nil, invalid("model.capacity")
	}
	capacity := observability.NewGate(config.Defaults().Capacity.Models, "model")
	if len(gates) == 1 {
		if gates[0] == nil {
			return nil, invalid("model.capacity")
		}
		capacity = gates[0]
	}
	httpClient := &http.Client{}
	if client != nil {
		*httpClient = *client
	}
	httpClient.Timeout = cfg.RequestTimeout
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	opts := []option.RequestOption{
		// 即使使用密钥池，也覆盖 SDK 的环境变量凭据。
		option.WithAPIKey(cfg.APIKey),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(0),
	}
	if keys := cfg.Keys; keys != nil {
		opts = append(opts, option.WithMiddleware(func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
			key, err := keys.Pick()
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+key)
			return next(req)
		}))
	}
	if cfg.Thinking != nil && cfg.Thinking.Enabled {
		// 深拷贝，避免调用方修改共享 JSON 参数影响并发请求。
		data, err := json.Marshal(cfg.Thinking)
		if err != nil {
			return nil, invalid("model.thinking")
		}
		var thinking config.Thinking
		if err := decodeJSON(data, &thinking); err != nil {
			return nil, invalid("model.thinking")
		}
		opts = append(opts, option.WithJSONSet(thinking.Key, thinking.Value))
	}
	llm, err := openaimodel.NewModel(context.Background(), cfg.Model, &openaimodel.Config{
		ApiKey:  cfg.APIKey,
		Url:     strings.TrimRight(cfg.BaseURL, "/") + "/",
		API:     openaimodel.APIChatCompletions,
		Options: opts,
	})
	if err != nil {
		return nil, safeError(err)
	}
	return &managedModel{LLM: llm, capacity: capacity}, nil
}

// managedModel 只负责应用策略；协议转换和 HTTP 调用由 model.LLM 实现承担。
type managedModel struct {
	model.LLM
	capacity *observability.Gate
}

func (m *managedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		ctx, finish := observability.Default.Start(ctx, "model")
		var failure error
		defer func() { finish(failure) }()
		fail := func(err error) {
			failure = err
			yield(nil, err)
		}
		release, err := m.capacity.Try(ctx)
		if err != nil {
			fail(err)
			return
		}
		defer release()
		if req == nil || req.Config == nil {
			fail(invalid("model.request"))
			return
		}
		if req.Model != "" && req.Model != m.Name() {
			fail(invalid("model.name"))
			return
		}
		// 工具轮次只消费完整响应，避免在参数尚未接收完整时执行副作用。
		if len(req.Config.Tools) > 0 {
			stream = false
		}
		var partialSize int
		for response, err := range m.LLM.GenerateContent(ctx, req, stream) {
			if err == nil {
				err = ctx.Err()
			}
			if err != nil {
				fail(safeError(err))
				return
			}
			if response == nil || response.Content == nil || response.CustomMetadata["openai_refusal"] == true {
				fail(safeError(ErrModel))
				return
			}
			if err := validateModelCalls(req, response); err != nil {
				fail(err)
				return
			}
			var size int
			for _, part := range response.Content.Parts {
				if part != nil {
					size += len(part.Text)
				}
			}
			if response.Partial {
				partialSize += size
			}
			if size > 8<<20 || partialSize > 8<<20 {
				fail(invalid("model.output_size"))
				return
			}
			if !response.Partial && response.FinishReason != genai.FinishReasonStop {
				fail(safeError(ErrModel))
				return
			}
			if !yield(response, nil) {
				return
			}
		}
	}
}

var _ model.LLM = (*managedModel)(nil)

// 整批校验后才交给 ADK，避免后面的无效调用使前面的工具产生部分副作用。
func validateModelCalls(req *model.LLMRequest, response *model.LLMResponse) error {
	known := map[string]bool{}
	for _, group := range req.Config.Tools {
		if group != nil {
			for _, declaration := range group.FunctionDeclarations {
				if declaration != nil {
					known[declaration.Name] = true
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, part := range response.Content.Parts {
		if part == nil {
			return invalid("model.output")
		}
		if call := part.FunctionCall; call != nil {
			if response.Partial || call.ID == "" || !known[call.Name] || seen[call.ID] || len(seen) >= 64 {
				return invalid("model.tool_call")
			}
			seen[call.ID] = true
			args, err := json.Marshal(call.Args)
			if err != nil || len(args) > 1<<20 {
				return invalid("model.tool_call")
			}
		}
	}
	return nil
}
