package main

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestMemoryStoreKeepsLastMessages(t *testing.T) {
	s := newMemoryStore()
	ctx := context.Background()
	for i := 0; i < memoryMessagesCap+10; i++ {
		if _, err := s.AddMessage(ctx, Message{Text: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	msgs, _ := s.Messages(ctx, 1000)
	if len(msgs) != memoryMessagesCap {
		t.Fatalf("want %d messages, got %d", memoryMessagesCap, len(msgs))
	}
	if msgs[0].ID != memoryMessagesCap+10 {
		t.Fatalf("newest first expected, got id %d", msgs[0].ID)
	}
}

// Интеграционный тест: запускается, только если задан TEST_DATABASE_URL, например
//
//	docker run -d --rm -p 5432:5432 -e POSTGRES_PASSWORD=demo postgres:16-alpine
//	TEST_DATABASE_URL=postgres://postgres:demo@localhost:5432/postgres?sslmode=disable go test ./...
func TestPostgresStore(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Несколько «реплик» мигрируют одновременно — advisory lock не даёт им подраться.
	errs := make(chan error, 5)
	for range 5 {
		go func() {
			s, err := newPostgresStore(ctx, dsn)
			if err == nil {
				s.Close()
			}
			errs <- err
		}()
	}
	for range 5 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent migration: %v", err)
		}
	}

	s, err := newPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.ResetStats(ctx); err != nil {
		t.Fatal(err)
	}
	s.RecordVisit(ctx, "pod-a")
	s.RecordVisit(ctx, "pod-a")
	podVisits, total, err := s.RecordVisit(ctx, "pod-b")
	if err != nil {
		t.Fatal(err)
	}
	if podVisits != 1 || total != 3 {
		t.Fatalf("pod-b: got pod=%d total=%d, want 1/3", podVisits, total)
	}

	stats, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 || stats[0].Pod != "pod-a" || stats[0].Visits != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	m, err := s.AddMessage(ctx, Message{Author: "a", Text: "hello", Pod: "pod-a"})
	if err != nil || m.ID == 0 || m.CreatedAt.IsZero() {
		t.Fatalf("add message: %+v, %v", m, err)
	}
	msgs, err := s.Messages(ctx, 1)
	if err != nil || len(msgs) != 1 || msgs[0].ID != m.ID {
		t.Fatalf("messages: %+v, %v", msgs, err)
	}
}
