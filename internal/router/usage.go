package router

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jerryjuche/padi/internal/router/config"
)

// UsageRow is one router attempt. It deliberately carries no request body,
// header map, API key or base URL: only counters and opaque task/session IDs.
type UsageRow struct {
	Time          time.Time
	SessionID     string
	TaskID        string
	Alias         string
	Provider      string
	Model         string
	Status        int
	FallbackDepth int
	FirstByte     time.Duration
	Total         time.Duration
	TokensIn      int
	TokensOut     int
	CostMicros    int64
	QuotaTokens   int
}

// Recorder is the narrow interface the handler needs. Tests use a capturing
// recorder; production uses the buffered PG writer below.
type Recorder interface {
	Record(UsageRow)
	Close()
}

type noopRecorder struct{}

func (noopRecorder) Record(UsageRow) {}
func (noopRecorder) Close()          {}

// MemoryRecorder captures rows for tests without a database.
type MemoryRecorder struct {
	mu   sync.Mutex
	rows []UsageRow
}

func (m *MemoryRecorder) Record(row UsageRow) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.rows = append(m.rows, row)
	m.mu.Unlock()
}

func (m *MemoryRecorder) Rows() []UsageRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]UsageRow(nil), m.rows...)
}

func (m *MemoryRecorder) Close() {}

const (
	usageBuffer   = 1024
	usageBatch    = 100
	usageFlushFor = 2 * time.Second
)

// PGUsageWriter batches rows into agent.llm_usage with agent_router's insert
// grant. A full buffer drops rows rather than slowing model requests; the drop
// count is exposed on /status.
type PGUsageWriter struct {
	pool    *pgxpool.Pool
	rows    chan UsageRow
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	closed  sync.Once
	dropped atomic.Uint64
}

func NewPGUsageWriter(ctx context.Context, dsn string) (*PGUsageWriter, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	writerCtx, cancel := context.WithCancel(context.Background())
	w := &PGUsageWriter{
		pool: pool, rows: make(chan UsageRow, usageBuffer), done: make(chan struct{}),
		ctx: writerCtx, cancel: cancel,
	}
	go w.loop()
	return w, nil
}

func (w *PGUsageWriter) Record(row UsageRow) {
	select {
	case w.rows <- row:
	default:
		w.dropped.Add(1)
	}
}

func (w *PGUsageWriter) Dropped() uint64 { return w.dropped.Load() }

func (w *PGUsageWriter) Close() {
	if w == nil {
		return
	}
	w.closed.Do(func() {
		w.cancel()
		<-w.done
		w.pool.Close()
	})
}

func (w *PGUsageWriter) loop() {
	defer close(w.done)
	ticker := time.NewTicker(usageFlushFor)
	defer ticker.Stop()
	batch := make([]UsageRow, 0, usageBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := w.flush(batch); err != nil {
			w.dropped.Add(uint64(len(batch)))
		}
		batch = batch[:0]
	}
	for {
		select {
		case row := <-w.rows:
			batch = append(batch, row)
			if len(batch) >= usageBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-w.ctx.Done():
			flush()
			return
		}
	}
}

var usageColumns = []string{
	"ts", "session_id", "task_id", "alias", "provider", "model", "status",
	"fallback_depth", "first_byte_ms", "total_ms", "tokens_in", "tokens_out",
	"cost_usd_micros", "quota_tokens",
}

func (w *PGUsageWriter) flush(rows []UsageRow) error {
	values := make([][]any, 0, len(rows))
	for _, row := range rows {
		values = append(values, []any{
			row.Time, uuidPtr(row.SessionID), uuidPtr(row.TaskID), row.Alias,
			row.Provider, row.Model, row.Status, row.FallbackDepth,
			int(row.FirstByte / time.Millisecond), int(row.Total / time.Millisecond),
			row.TokensIn, row.TokensOut, row.CostMicros, row.QuotaTokens,
		})
	}
	_, err := w.pool.CopyFrom(w.ctx, pgx.Identifier{"agent", "llm_usage"}, usageColumns, pgx.CopyFromRows(values))
	return err
}

func uuidPtr(raw string) *uuid.UUID {
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil
	}
	return &id
}

// costMicros converts token counts to USD microdollars using configured
// per-million rates. Unpriced providers record zero rather than guessing.
func costMicros(tokensIn, tokensOut int, p config.Provider) int64 {
	if tokensIn <= 0 && tokensOut <= 0 {
		return 0
	}
	in := int64(tokensIn) * p.CostPer1MInMicros / 1_000_000
	out := int64(tokensOut) * p.CostPer1MOutMicros / 1_000_000
	return in + out
}
