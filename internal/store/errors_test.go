package store_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/store"
)

// 旧存储名称与公共类别必须双向兼容，适配器迁移后不要求业务层同时发布。
func TestLegacyErrorAliases(t *testing.T) {
	for _, tc := range []struct{ legacy, shared apperrors.Kind }{
		{store.ErrNotFound, apperrors.ErrNotFound},
		{store.ErrConflict, apperrors.ErrConflict},
	} {
		if tc.legacy != tc.shared || !errors.Is(fmt.Errorf("store: %w", tc.legacy), tc.shared) {
			t.Fatal("legacy error no longer matches shared category")
		}
		err := apperrors.Wrap(tc.shared, "", "storage operation failed", errors.New("driver failure"))
		if !errors.Is(fmt.Errorf("application: %w", err), tc.legacy) {
			t.Fatal("shared error no longer matches legacy category")
		}
	}
}
