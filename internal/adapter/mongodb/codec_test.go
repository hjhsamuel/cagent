package mongodb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hjhsamuel/cagent/internal/apperrors"
	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/store"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestDocumentRoundTripRetainsNilAndBinaryContent(t *testing.T) {
	for _, parts := range [][]domain.Part{nil, {}, {{Text: string([]byte{0xff, 0x00, 0xfe}), Data: nil}}, {{Data: []byte{}}}, {{Data: []byte{0, 0xff}}}} {
		value := domain.Task{Scope: testScope, ID: "task", Progress: parts, Result: &domain.ToolResult{CallID: "call", Parts: parts}}
		d, err := pack(testScope, "task", value, 1)
		check(t, err)
		var decoded domain.Task
		err = d.decode(&decoded)
		check(t, err)
		if !reflect.DeepEqual(decoded, value) {
			t.Fatal("BSON mapping changed nil/empty or binary content")
		}
		d.Schema = 2
		var storedTask domain.Task
		err = d.decode(&storedTask)
		if err == nil {
			t.Fatal("unknown schema accepted")
		}
	}
}

func TestMutationDigestIsStableAndSensitiveToContent(t *testing.T) {
	cp := domain.Checkpoint{Scope: testScope, RunID: "run", Format: "v1", Data: []byte{1}, Version: 1}
	ms := []domain.Message{{ID: "id", Scope: testScope, SessionID: "session", RunID: "run", Parts: []domain.Part{{Text: "a"}}}}
	events := []domain.Event{{Scope: testScope, RunID: "run", Kind: domain.EventTextDelta, Data: []byte{0xff}}}
	first, err := mutationDigest(store.MutationRunCommit, "", domain.RunRunning, ms, events, &cp)
	check(t, err)
	// 固定样本捕获字段编码或驱动升级导致的协议漂移，不能只比较同一函数的两次输出。
	if fmt.Sprintf("%x", first) != "a69ab8254e0883c0b164e30789f17f938688b4b16d79c4e2d01fa7140c6346d5" {
		t.Fatal("mutation v1 encoding changed; migrate receipts before changing the format")
	}
	cp.Version = 99
	cp.UpdatedAt = time.Now()
	ms[0].Sequence = 10
	ms[0].CreatedAt = time.Now()
	events[0].Sequence = 20
	events[0].CreatedAt = time.Now()
	again, err := mutationDigest(store.MutationRunCommit, "", domain.RunRunning, ms, events, &cp)
	check(t, err)
	if first != again {
		t.Fatal("storage fields affected retry digest")
	}
	ms[0].ID = "changed"
	different, err := mutationDigest(store.MutationRunCommit, "", domain.RunRunning, ms, events, &cp)
	check(t, err)
	if different == first {
		t.Fatal("message identity absent from digest")
	}
	for _, pair := range [][2][]domain.Message{{nil, {}}, {{{Parts: []domain.Part{{Text: string([]byte{0xff})}}}}, {{Parts: []domain.Part{{Text: string([]byte{0xfe})}}}}}} {
		a, err := mutationDigest(store.MutationRunCommit, "", domain.RunRunning, pair[0], nil, nil)
		check(t, err)
		b, err := mutationDigest(store.MutationRunCommit, "", domain.RunRunning, pair[1], nil, nil)
		check(t, err)
		if a == b {
			t.Fatal("distinct bytes merged")
		}
	}
}

func TestSafeDriverErrorsPreserveIdentity(t *testing.T) {
	secret := errors.New("private-mongodb-credential")
	for _, original := range []error{secret, fmt.Errorf("private wrapping: %w", context.Canceled), fmt.Errorf("private wrapping: %w", context.DeadlineExceeded), mongo.WriteException{WriteErrors: mongo.WriteErrors{{Code: 11000, Message: "private duplicate value"}}}} {
		err := safeError(original)
		for _, format := range []string{"%v", "%+v", "%#v", "%q"} {
			if strings.Contains(fmt.Sprintf(format, err), "private") {
				t.Fatal("driver diagnostic leaked")
			}
		}
		if errors.Is(original, context.Canceled) && !errors.Is(err, context.Canceled) {
			t.Fatal("lost cancellation")
		}
		if errors.Is(original, context.DeadlineExceeded) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("lost deadline")
		}
		if original == secret && !errors.Is(err, secret) {
			t.Fatal("lost cause")
		}
	}
	wantKind(t, safeError(mongo.ErrNoDocuments), apperrors.ErrNotFound)
	if !errors.Is(safeError(mongo.ErrNoDocuments), mongo.ErrNoDocuments) {
		t.Fatal("lost not-found driver cause")
	}
	wantKind(t, safeError(mongo.WriteException{WriteErrors: mongo.WriteErrors{{Code: 11000}}}), apperrors.ErrConflict)
	_, err := Open(context.Background(), Options{URI: "private-invalid-uri", Database: "test", Timeout: time.Second})
	wantKind(t, err, apperrors.ErrInvalidArgument)
	if strings.Contains(err.Error(), "private") {
		t.Fatal("URI leaked")
	}
}
