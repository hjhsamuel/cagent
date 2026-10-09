package openai

import (
	"context"
	"fmt"

	"github.com/hjhsamuel/cagent/pkg/llm/openai/completions"
	"github.com/hjhsamuel/cagent/pkg/llm/openai/responses"
	"github.com/hjhsamuel/cagent/pkg/llm/openai/shared"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"google.golang.org/adk/v2/model"
)

type Config struct {
	ApiKey string
	Url    string

	Options []option.RequestOption

	API API
}

type API string

const (
	APIResponses       API = "responses"
	APIChatCompletions API = "chat_completions"
)

func NewModel(ctx context.Context, modelName string, cfg *Config) (model.LLM, error) {
	if modelName == "" {
		return nil, shared.ErrModelNameRequired
	}

	if cfg == nil {
		cfg = &Config{}
	}

	var opts []option.RequestOption
	if cfg.ApiKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.ApiKey))
	}
	if cfg.Url != "" {
		opts = append(opts, option.WithBaseURL(cfg.Url))
	}

	opts = append(opts, cfg.Options...)

	client := openai.NewClient(opts...)
	switch cfg.API {
	case "", APIResponses:
		return responses.New(&client, modelName), nil
	case APIChatCompletions:
		return completions.New(&client, modelName), nil
	default:
		return nil, fmt.Errorf("%w: %q", shared.ErrUnsupportedAPI, cfg.API)
	}
}
