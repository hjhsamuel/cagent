package apperrors_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hjhsamuel/cagent/internal/apperrors"
)

// providerError 模拟携带敏感连接诊断的适配器错误，用于验证原始类型仍可提取。
type providerError struct{ diagnostic string }

func (e *providerError) Error() string { return e.diagnostic }

func TestCategoriesAndDetailsSurviveWrapping(t *testing.T) {
	kinds := []apperrors.Kind{apperrors.ErrInvalidArgument, apperrors.ErrNotFound, apperrors.ErrConflict, apperrors.ErrUnsupported}
	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			detail := apperrors.New(kind, "run.version", "operation rejected")
			err := fmt.Errorf("application: %w", fmt.Errorf("repository: %w", detail))
			for _, target := range kinds {
				if got := errors.Is(err, target); got != (kind == target) {
					t.Errorf("Is(%v) = %v for %v", target, got, kind)
				}
			}
			var got *apperrors.Error
			if !errors.As(err, &got) || got != detail || got.Kind() != kind || got.Field() != "run.version" || got.Message() != "operation rejected" {
				t.Fatalf("lost structured detail: %v", err)
			}
			if errors.Unwrap(detail) != nil || errors.Is(detail, nil) || errors.Is(detail, errors.New(string(kind))) {
				t.Fatal("unexpected cause or match by text")
			}
		})
	}
}

func TestCauseIsPreservedButNotDisplayed(t *testing.T) {
	secret := "mongodb://user:private-password@internal-host prompt=private-prompt token=private-token"
	cause := &providerError{diagnostic: secret}
	err := apperrors.Wrap(apperrors.ErrConflict, "run.version", "resource version conflict", cause)
	wrapped := fmt.Errorf("save: %w", err)
	if !errors.Is(wrapped, cause) || !errors.Is(wrapped, apperrors.ErrConflict) || errors.Unwrap(err) != cause {
		t.Fatal("lost cause identity or category")
	}
	var provider *providerError
	if !errors.As(wrapped, &provider) || provider != cause {
		t.Fatal("lost provider error type")
	}
	// 常规输出、调试输出及标准 %w 包装均不得展开底层秘密。
	for _, value := range []error{err, wrapped} {
		for _, format := range []string{"%s", "%v", "%+v", "%#v", "%q"} {
			output := fmt.Sprintf(format, value)
			for _, forbidden := range []string{"private-password", "internal-host", "private-prompt", "private-token"} {
				if strings.Contains(output, forbidden) {
					t.Fatalf("sensitive diagnostic leaked through %s", format)
				}
			}
			if !strings.Contains(output, "run.version") || !strings.Contains(output, "resource version conflict") {
				t.Fatalf("safe diagnostic missing through %s: %s", format, output)
			}
		}
	}
}

func TestContextSignalsSurviveCauseChain(t *testing.T) {
	for _, signal := range []error{context.Canceled, context.DeadlineExceeded} {
		// 模拟底层观察中断；公共层只保留调用方指定的分类，不推断远端任务终态。
		cause := fmt.Errorf("provider observation: %w", signal)
		err := fmt.Errorf("application: %w", apperrors.Wrap(apperrors.ErrUnsupported, "", "observation unavailable", cause))
		if !errors.Is(err, signal) || !errors.Is(err, apperrors.ErrUnsupported) {
			t.Fatalf("lost context signal: %v", signal)
		}
		other := context.Canceled
		if signal == other {
			other = context.DeadlineExceeded
		}
		if errors.Is(err, other) {
			t.Fatal("cancellation and deadline conflated")
		}
	}
}

func TestNilCauseAndOptionalDetails(t *testing.T) {
	if err := apperrors.Wrap(apperrors.ErrConflict, "", "save failed", nil); err != nil {
		t.Fatal("wrapping success manufactured a failure")
	}
	for _, tc := range []struct{ field, message, want string }{
		{"", "", "not_found"},
		{"", "resource not found", "not_found: resource not found"},
		{"session.id", "", "not_found: session.id"},
	} {
		if got := apperrors.New(apperrors.ErrNotFound, tc.field, tc.message).Error(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func TestNestedCategoriesKeepOutermostDetails(t *testing.T) {
	inner := apperrors.New(apperrors.ErrNotFound, "", "resource not found")
	outer := apperrors.Wrap(apperrors.ErrConflict, "run.version", "save rejected", inner)
	joined := errors.Join(errors.New("unrelated"), fmt.Errorf("save: %w", outer))
	var detail *apperrors.Error
	if !errors.Is(joined, apperrors.ErrConflict) || !errors.Is(joined, apperrors.ErrNotFound) || !errors.As(joined, &detail) || detail.Kind() != apperrors.ErrConflict {
		t.Fatal("nested error traversal changed")
	}
	if errors.Is(inner, apperrors.ErrConflict) || errors.Is(outer, apperrors.New(apperrors.ErrConflict, "", "")) {
		t.Fatal("details matched as category sentinels")
	}
}
