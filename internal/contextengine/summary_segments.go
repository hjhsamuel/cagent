package contextengine

import (
	"context"
	"errors"
	"github.com/hjhsamuel/cagent/internal/domain"
)

// At most 32 bounded summary requests. Only a budget rejection may split source
// groups; network/model failures never trigger repeated generation.
func summarizeSegments(ctx context.Context, s Summarizer, source []domain.Message) (string, error) {
	remaining := 32
	var summarize func([]domain.Message, int) (string, error)
	summarize = func(messages []domain.Message, depth int) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if remaining == 0 || depth > 8 {
			return "", ErrBudgetExceeded
		}
		remaining--
		text, err := s.Summarize(ctx, messages)
		if err == nil {
			return text, nil
		}
		if !errors.Is(err, ErrBudgetExceeded) || len(messages) < 2 {
			return "", err
		}
		middle := len(messages) / 2
		left, err := summarize(messages[:middle], depth+1)
		if err != nil {
			return "", err
		}
		right, err := summarize(messages[middle:], depth+1)
		if err != nil {
			return "", err
		}
		merged := make([]domain.Message, 2)
		for i, text := range []string{left, right} {
			merged[i] = domain.Message{Scope: messages[0].Scope, ID: "summary-segment", SessionID: messages[0].SessionID, Role: domain.RoleUser, Parts: []domain.Part{{Kind: domain.PartText, Text: text}}}
		}
		return summarize(merged, depth+1)
	}
	return summarize(source, 0)
}
