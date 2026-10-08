package bootstrap

import (
	"context"

	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

type modelReader interface {
	ListModels(context.Context) ([]schema.Model, error)
}

// loadModels 只装配模型目录，启动不决定主对话使用哪个模型。
func loadModels(ctx context.Context, db modelReader, cfg config.Config, ring *config.Keyring) (config.Config, error) {
	docs, err := db.ListModels(ctx)
	if err != nil {
		return config.Config{}, err
	}
	catalog, err := config.NewModelCatalog(docs, cfg.Agent.Name, ring)
	if err != nil {
		return config.Config{}, err
	}
	for _, doc := range docs {
		check := cfg
		check.Agent, err = config.ResolveModelMetadata(doc, cfg.Agent.Name)
		if err != nil {
			return config.Config{}, err
		}
		check.Context.WindowTokens, check.Context.OutputTokens = doc.Options.WindowTokens, doc.Options.OutputTokens
		check.Context.SummaryWindowTokens = doc.Options.WindowTokens
		check.Context.SummaryOutputTokens = min(check.Context.SummaryOutputTokens, doc.Options.OutputTokens)
		if err := check.Validate(); err != nil {
			return config.Config{}, err
		}
	}
	cfg.Models = catalog
	return cfg, nil
}
