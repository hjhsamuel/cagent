package bootstrap

import (
	"context"

	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
)

type modelReader interface {
	ListModels(context.Context) ([]schema.Model, error)
}

// loadModels 只装配模型目录，允许目录为空；模型缺失在实际选择或绑定时处理。
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
		if err := validateModel(cfg, doc); err != nil {
			return config.Config{}, err
		}
	}
	cfg.Models = catalog
	return cfg, nil
}

func validateModel(cfg config.Config, doc schema.Model) error {
	check := cfg
	var err error
	check.Agent, err = config.ResolveModelMetadata(doc, cfg.Agent.Name)
	if err != nil {
		return err
	}
	return check.Validate()
}

type modelWriter interface {
	WriteModel(context.Context, *schema.Model, *schema.Model) error
}

type modelManagement struct {
	cfg config.Config
	db  modelWriter
}

func (m modelManagement) List(ctx context.Context) ([]config.ModelView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.cfg.Models.ListModelConfigurations(), nil
}
func (m modelManagement) Get(ctx context.Context, id string) (config.ModelView, error) {
	if err := ctx.Err(); err != nil {
		return config.ModelView{}, err
	}
	return m.cfg.Models.GetModelConfiguration(id)
}
func (m modelManagement) Put(ctx context.Context, id string, input config.ModelInput) (config.ModelView, error) {
	return m.cfg.Models.PutModelConfiguration(id, input, func(doc schema.Model) error { return validateModel(m.cfg, doc) }, func(old, updated *schema.Model) error { return m.db.WriteModel(ctx, old, updated) })
}
func (m modelManagement) Delete(ctx context.Context, id string) error {
	return m.cfg.Models.DeleteModelConfiguration(id, func(old, updated *schema.Model) error { return m.db.WriteModel(ctx, old, updated) })
}
