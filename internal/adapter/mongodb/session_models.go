package mongodb

import (
	"context"

	"github.com/hjhsamuel/cagent/internal/domain"
)

// BindSessionModel 为旧会话一次性补齐模型/凭据引用。事务重读保证并发调用不覆盖绑定。
func (b *Database) BindSessionModel(ctx context.Context, scope domain.Scope, id, modelID, keyID string) (domain.Session, error) {
	if err := validateKey(scope, id); err != nil {
		return domain.Session{}, err
	}
	if modelID == "" || keyID == "" {
		return domain.Session{}, invalid("session.model_binding")
	}
	var result domain.Session
	err := b.withTransaction(ctx, "session.bind_model", func(tx context.Context) error {
		result = domain.Session{}
		var old document
		if err := b.collection(SessionCollection).FindOne(tx, key(scope, id)).Decode(&old); err != nil {
			return err
		}
		if err := old.decode(&result); err != nil {
			return err
		}
		if result.ModelID != "" || result.APIKeyID != "" {
			return result.Validate()
		}
		result.ModelID, result.APIKeyID = modelID, keyID
		if err := result.Validate(); err != nil {
			return err
		}
		version, err := increment(result.Version)
		if err != nil {
			return err
		}
		result.Version = version
		result.UpdatedAt, err = b.now(tx)
		if err != nil {
			return err
		}
		doc, err := repack(old, result, version)
		if err != nil {
			return err
		}
		write, err := b.collection(SessionCollection).ReplaceOne(tx, old.versionKey(), doc)
		if err != nil {
			return err
		}
		if write.MatchedCount != 1 {
			return conflict("session.version")
		}
		return nil
	})
	if err != nil {
		return domain.Session{}, safeError(err)
	}
	return result, nil
}
