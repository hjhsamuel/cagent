package mongodb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"github.com/hjhsamuel/cagent/internal/domain"
	"github.com/hjhsamuel/cagent/internal/storage/schema"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const payloadChunkSize = 4 << 20
const maxPayloadBytes = 64 << 20

// Chunks and references are written in the same transaction. An aborted attempt
// leaves no orphan, and committed receipt/checkpoint references pin their data.
func (b *Database) writePayload(ctx context.Context, scope domain.Scope, raw []byte) (*schema.PayloadRef, error) {
	if len(raw) == 0 || len(raw) > maxPayloadBytes {
		return nil, invalid("payload.size")
	}
	hash := sha256.Sum256(raw)
	ref := &schema.PayloadRef{Format: 1, Hash: hex.EncodeToString(hash[:]), Bytes: len(raw), Chunks: (len(raw) + payloadChunkSize - 1) / payloadChunkSize}
	for i := 0; i < ref.Chunks; i++ {
		end := min((i+1)*payloadChunkSize, len(raw))
		id := compositeID(ref.Hash, stringInt(int64(i)))
		d := schema.PayloadChunk{Tenant: scope.TenantID, User: scope.UserID, ID: id, Hash: ref.Hash, Index: i, Data: raw[i*payloadChunkSize : end]}
		if _, err := b.collection(PayloadCollection).UpdateOne(ctx, key(scope, id), bson.M{"$setOnInsert": d}, options.UpdateOne().SetUpsert(true)); err != nil {
			return nil, err
		}
	}
	return ref, nil
}
func (b *Database) readPayload(ctx context.Context, scope domain.Scope, ref *schema.PayloadRef) ([]byte, error) {
	if ref == nil || ref.Format != 1 || ref.Bytes <= 0 || ref.Bytes > maxPayloadBytes || ref.Chunks != (ref.Bytes+payloadChunkSize-1)/payloadChunkSize || len(ref.Hash) != 64 {
		return nil, invariant()
	}
	raw := make([]byte, 0, ref.Bytes)
	for i := 0; i < ref.Chunks; i++ {
		var chunk schema.PayloadChunk
		if err := b.collection(PayloadCollection).FindOne(ctx, key(scope, compositeID(ref.Hash, stringInt(int64(i))))).Decode(&chunk); err != nil {
			return nil, err
		}
		if chunk.Hash != ref.Hash || chunk.Index != i || len(chunk.Data) != min(payloadChunkSize, ref.Bytes-i*payloadChunkSize) {
			return nil, invariant()
		}
		raw = append(raw, chunk.Data...)
	}
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != ref.Hash {
		return nil, invariant()
	}
	return raw, nil
}
func (b *Database) decodeCheckpoint(ctx context.Context, d checkpointDocument, value *domain.Checkpoint) error {
	if d.Payload != nil {
		if d.Schema != schema.DocumentVersion || len(d.Data) > 0 {
			return invariant()
		}
		raw, err := b.readPayload(ctx, domain.Scope{TenantID: d.Tenant, UserID: d.User}, d.Payload)
		if err != nil {
			return err
		}
		d.Data = raw
	}
	return d.decode(value)
}
func (b *Database) packCheckpoint(ctx context.Context, scope domain.Scope, id string, cp domain.Checkpoint, version int64) (checkpointDocument, error) {
	d, err := packCheckpointRecord(scope, id, cp, version)
	if err != nil {
		return d, err
	}
	if len(d.Data) > payloadChunkSize {
		d.Payload, err = b.writePayload(ctx, scope, d.Data)
		d.Data = nil
	}
	return d, err
}
