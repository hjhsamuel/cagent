package bootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/hjhsamuel/cagent/internal/config"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"github.com/joho/godotenv"
)

type modelKeyWriter interface {
	ReplaceModelKeys(context.Context, []schema.Model, []schema.Model) error
}

func modelKeyRotation(catalog *config.ModelCatalog, db modelKeyWriter) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		return catalog.RotateKeys(func(keys config.ModelEncryption) error {
			return persistModelKeys(".env", keys)
		}, func(old, updated []schema.Model) error { return db.ReplaceModelKeys(ctx, old, updated) })
	}
}

// 先写密钥再写密文；数据库失败时旧密文仍可解密。
func persistModelKeys(path string, keys config.ModelEncryption) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return errors.New("cannot read model encryption env file")
	}
	values, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return errors.New("cannot parse model encryption env file")
	}
	var additions strings.Builder
	for version, value := range keys.Keys {
		name := "CAGENT_MODEL_ENCRYPTION_KEY_V" + strings.TrimPrefix(version, "v")
		if existing, ok := values[name]; ok {
			if existing != value {
				return errors.New("model encryption env version conflict")
			}
			continue
		}
		additions.WriteString(name + "=" + value + "\n")
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, additions.String()...)
	temp, err := os.CreateTemp(filepath.Dir(path), ".model-keys-*")
	if err != nil {
		return errors.New("cannot create model encryption env file")
	}
	defer os.Remove(temp.Name())
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(data)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot write model encryption env file")
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return errors.New("cannot replace model encryption env file")
	}
	return nil
}
