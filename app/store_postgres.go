package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const schema = `
CREATE TABLE IF NOT EXISTS visits (
    pod       TEXT        PRIMARY KEY,
    count     BIGINT      NOT NULL DEFAULT 0,
    last_seen TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS messages (
    id         BIGSERIAL   PRIMARY KEY,
    author     TEXT        NOT NULL,
    text       TEXT        NOT NULL,
    pod        TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS snapshots (
    id         BIGSERIAL   PRIMARY KEY,
    pod        TEXT        NOT NULL,
    messages   BIGINT      NOT NULL,
    max_id     BIGINT      NOT NULL,
    digest     TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);`

// Произвольный ключ для pg_advisory_xact_lock: реплики стартуют одновременно,
// и без лока параллельные CREATE TABLE IF NOT EXISTS могут подраться.
const migrationLockKey = 20240901

type postgresStore struct {
	pool *pgxpool.Pool
}

// newPostgresStore подключается к базе и накатывает схему, повторяя попытки
// до истечения ctx. Если база так и не ответила — под упадёт и Kubernetes
// перезапустит его (CrashLoopBackOff тоже часть демо).
func newPostgresStore(ctx context.Context, dsn string) (*postgresStore, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 5

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	s := &postgresStore{pool: pool}

	backoff := 500 * time.Millisecond
	for {
		err = s.migrate(ctx)
		if err == nil {
			return s, nil
		}
		slog.Warn("postgres is not ready yet, retrying", "err", err, "retryIn", backoff.String())
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func (s *postgresStore) migrate(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op после Commit

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, schema); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *postgresStore) Kind() string { return "postgres" }

func (s *postgresStore) RecordVisit(ctx context.Context, pod string) (podVisits, total int64, err error) {
	// Один запрос: upsert счётчика пода + сумма по остальным подам.
	err = s.pool.QueryRow(ctx, `
		WITH up AS (
		    INSERT INTO visits (pod, count) VALUES ($1, 1)
		    ON CONFLICT (pod) DO UPDATE SET count = visits.count + 1, last_seen = now()
		    RETURNING count
		)
		SELECT up.count,
		       up.count + COALESCE((SELECT sum(count) FROM visits WHERE pod <> $1), 0)::bigint
		FROM up`, pod).Scan(&podVisits, &total)
	return podVisits, total, err
}

func (s *postgresStore) Stats(ctx context.Context) ([]PodStat, error) {
	rows, err := s.pool.Query(ctx, `SELECT pod, count, last_seen FROM visits ORDER BY pod`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []PodStat{}
	for rows.Next() {
		var st PodStat
		if err := rows.Scan(&st.Pod, &st.Visits, &st.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *postgresStore) ResetStats(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `TRUNCATE visits`)
	return err
}

func (s *postgresStore) AddMessage(ctx context.Context, m Message) (Message, error) {
	err := s.pool.QueryRow(ctx,
		`INSERT INTO messages (author, text, pod) VALUES ($1, $2, $3) RETURNING id, created_at`,
		m.Author, m.Text, m.Pod,
	).Scan(&m.ID, &m.CreatedAt)
	return m, err
}

func (s *postgresStore) Messages(ctx context.Context, limit int) ([]Message, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, author, text, pod, created_at FROM messages ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Message{}
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Author, &m.Text, &m.Pod, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *postgresStore) MessagesUpTo(ctx context.Context, maxID int64) ([]Message, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, author, text, pod, created_at FROM messages WHERE id <= $1 ORDER BY id`, maxID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Message, error) {
		var m Message
		err := row.Scan(&m.ID, &m.Author, &m.Text, &m.Pod, &m.CreatedAt)
		return m, err
	})
}

func (s *postgresStore) CreateSnapshot(ctx context.Context, pod string) (Snapshot, error) {
	msgs, err := s.MessagesUpTo(ctx, math.MaxInt64)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Pod: pod}
	snap.Messages, snap.MaxID, snap.Digest = digestMessages(msgs)
	err = s.pool.QueryRow(ctx,
		`INSERT INTO snapshots (pod, messages, max_id, digest) VALUES ($1, $2, $3, $4) RETURNING id, created_at`,
		snap.Pod, snap.Messages, snap.MaxID, snap.Digest,
	).Scan(&snap.ID, &snap.CreatedAt)
	return snap, err
}

func (s *postgresStore) GetSnapshot(ctx context.Context, id int64) (Snapshot, error) {
	var snap Snapshot
	err := s.pool.QueryRow(ctx,
		`SELECT id, pod, messages, max_id, digest, created_at FROM snapshots WHERE id = $1`, id,
	).Scan(&snap.ID, &snap.Pod, &snap.Messages, &snap.MaxID, &snap.Digest, &snap.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrSnapshotNotFound
	}
	return snap, err
}

func (s *postgresStore) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *postgresStore) Close() { s.pool.Close() }
