package gamestate

import (
	"context"

	"github.com/redis/go-redis/v9"
	"github.com/rotisserie/eris"
	"go.opentelemetry.io/otel/codes"
	ddotel "gopkg.in/DataDog/dd-trace-go.v1/ddtrace/opentelemetry"
	ddtracer "gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"

	"pkg.world.dev/world-engine/cardinal/codec"
	"pkg.world.dev/world-engine/cardinal/types"
	"pkg.world.dev/world-engine/cardinal/types/txpool"
	"pkg.world.dev/world-engine/sign"
)

// The engine tick must be updated in the same atomic transaction as all the state changes
// associated with that tick. This means the manager here must also implement the TickStore interface.
var _ TickStorage = &EntityCommandBuffer{}

type pendingTransaction struct {
	TypeID types.MessageID
	TxHash types.TxHash
	Data   []byte
	Tx     *sign.Transaction
}

// GetTickNumbers returns the last tick that was started and the last tick that was ended. If start == end, it means
// the last tick that was attempted completed successfully. If start != end, it means a tick was started but did not
// complete successfully; Recover must be used to recover the pending transactions so the previously started tick can
// be completed.
func (m *EntityCommandBuffer) GetTickNumbers() (start, end uint64, err error) {
	ctx := context.Background()
	start, err = m.dbStorage.GetUInt64(ctx, storageStartTickKey())
	err = eris.Wrap(err, "")
	if eris.Is(eris.Cause(err), redis.Nil) {
		start = 0
	} else if err != nil {
		return 0, 0, err
	}
	end, err = m.dbStorage.GetUInt64(ctx, storageEndTickKey())
	err = eris.Wrap(err, "")
	if eris.Is(eris.Cause(err), redis.Nil) {
		end = 0
	} else if err != nil {
		return 0, 0, err
	}
	return start, end, nil
}

// StartNextTick saves the given transactions to the DB and sets the tick trackers to indicate we are in the middle
// of a tick. While transactions are saved to the DB, no state changes take place at this time.
func (m *EntityCommandBuffer) StartNextTick(ctx context.Context, txs []types.Message, pool *txpool.TxPool) error {
	ctx, span := m.tracer.Start(ddotel.ContextWithStartOptions(ctx, ddtracer.Measured()), "ecb.tick.start")
	defer span.End()

	pipe, err := m.dbStorage.StartTransaction(ctx)
	if err != nil {
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		return eris.Wrap(err, "failed to start transaction")
	}

	if err := m.addPendingTransactionToPipe(ctx, pipe, txs, pool); err != nil {
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		return eris.Wrap(err, "failed to add pending transaction to pipe")
	}

	if err := pipe.Incr(ctx, storageStartTickKey()); err != nil {
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		return eris.Wrap(err, "failed to increment start tick key")
	}

	if err := pipe.EndTransaction(ctx); err != nil {
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		return eris.Wrap(err, "failed to end transaction")
	}

	return nil
}

// FinalizeTick combines all pending state changes into a single multi/exec redis transactions and commits them
// to the DB.
func (m *EntityCommandBuffer) FinalizeTick(ctx context.Context) error {
	ctx, span := m.tracer.Start(ddotel.ContextWithStartOptions(ctx, ddtracer.Measured()), "ecb.tick.finalize")
	defer span.End()

	pipe, err := m.makePipeOfRedisCommands(ctx)
	if err != nil {
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		return eris.Wrap(err, "failed to make redis commands pipe")
	}

	if err := pipe.Incr(ctx, storageEndTickKey()); err != nil {
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		return eris.Wrap(err, "failed to increment end tick key")
	}

	if err := pipe.EndTransaction(ctx); err != nil {
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		return eris.Wrap(err, "failed to end transaction")
	}

	m.pendingArchIDs = nil

	if err := m.DiscardPending(); err != nil {
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		return eris.Wrap(err, "failed to discard pending transaction")
	}

	return nil
}

// Recover fetches the pending transactions for an incomplete tick. This should only be called if GetTickNumbers
// indicates that the previous tick was started, but never completed.
func (m *EntityCommandBuffer) Recover(txs []types.Message) (*txpool.TxPool, error) {
	ctx := context.Background()
	key := storagePendingTransactionKey()
	bz, err := m.dbStorage.GetBytes(ctx, key)
	if err != nil {
		return nil, eris.Wrap(err, "")
	}
	pending, err := codec.Decode[[]pendingTransaction](bz)
	if err != nil {
		return nil, err
	}
	idToTx := map[types.MessageID]types.Message{}
	for _, tx := range txs {
		idToTx[tx.ID()] = tx
	}

	txPool := txpool.New()
	for _, p := range pending {
		tx := idToTx[p.TypeID]
		var txData any
		txData, err = tx.Decode(p.Data)
		if err != nil {
			return nil, err
		}
		txPool.AddTransaction(tx.ID(), txData, p.Tx)
	}
	return txPool, nil
}

func (m *EntityCommandBuffer) addPendingTransactionToPipe(
	ctx context.Context, pipe PrimitiveStorage[string], txs []types.Message,
	pool *txpool.TxPool,
) error {
	ctx, span := m.tracer.Start(ddotel.ContextWithStartOptions(ctx, ddtracer.Measured()),
		"ecb.tick.start.add-pending-transaction")
	defer span.End()

	var pending []pendingTransaction
	for _, tx := range txs {
		currList := pool.ForID(tx.ID())
		for _, txData := range currList {
			buf, err := tx.Encode(txData.Msg)
			if err != nil {
				span.SetStatus(codes.Error, eris.ToString(err, true))
				span.RecordError(err)
				return err
			}

			currItem := pendingTransaction{
				TypeID: tx.ID(),
				TxHash: txData.TxHash,
				Tx:     txData.Tx,
				Data:   buf,
			}
			pending = append(pending, currItem)
		}
	}

	buf, err := codec.Encode(pending)
	if err != nil {
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		return err
	}

	if err := pipe.Set(ctx, storagePendingTransactionKey(), buf); err != nil {
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		return eris.Wrap(err, "failed to set pending transaction")
	}

	return nil
}
