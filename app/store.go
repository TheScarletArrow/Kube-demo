package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

type PodStat struct {
	Pod      string    `json:"pod"`
	Visits   int64     `json:"visits"`
	LastSeen time.Time `json:"lastSeen"`
}

type Message struct {
	ID        int64     `json:"id"`
	Author    string    `json:"author"`
	Text      string    `json:"text"`
	Pod       string    `json:"pod"`
	CreatedAt time.Time `json:"createdAt"`
}

// Snapshot — «снимок» гостевой книги: сколько сообщений было и контрольная
// сумма их содержимого. Сравнив снимок с текущими данными, можно доказать,
// что после удаления подов (и даже пода с базой) ничего не потерялось.
type Snapshot struct {
	ID        int64     `json:"id"`
	Pod       string    `json:"pod"` // под, который сделал снимок
	Messages  int64     `json:"messages"`
	MaxID     int64     `json:"maxId"`
	Digest    string    `json:"digest"`
	CreatedAt time.Time `json:"createdAt"`
}

var ErrSnapshotNotFound = errors.New("снимок не найден")

// digestMessages считает количество, максимальный id и sha256 по сообщениям,
// отсортированным по id. Одинаково для обоих хранилищ.
func digestMessages(msgs []Message) (count, maxID int64, digest string) {
	h := sha256.New()
	for _, m := range msgs {
		fmt.Fprintf(h, "%d\x00%s\x00%s\x00%s\n", m.ID, m.Author, m.Text, m.Pod)
		maxID = max(maxID, m.ID)
	}
	return int64(len(msgs)), maxID, hex.EncodeToString(h.Sum(nil))
}

// Store — хранилище счётчика визитов и гостевой книги.
// Есть две реализации: в памяти пода и в Postgres. Разница между ними —
// главный «вау-момент» демки: в памяти у каждого пода своя правда,
// и она умирает вместе с подом.
type Store interface {
	Kind() string
	RecordVisit(ctx context.Context, pod string) (podVisits, totalVisits int64, err error)
	Stats(ctx context.Context) ([]PodStat, error)
	ResetStats(ctx context.Context) error
	AddMessage(ctx context.Context, m Message) (Message, error)
	Messages(ctx context.Context, limit int) ([]Message, error)
	// MessagesUpTo — все сообщения с id <= maxID по возрастанию id.
	MessagesUpTo(ctx context.Context, maxID int64) ([]Message, error)
	CreateSnapshot(ctx context.Context, pod string) (Snapshot, error)
	GetSnapshot(ctx context.Context, id int64) (Snapshot, error)
	Ping(ctx context.Context) error
	Close()
}

const memoryMessagesCap = 100

type memoryStore struct {
	mu       sync.Mutex
	visits   map[string]*PodStat
	messages []Message // от старых к новым
	nextID   int64

	snapshots  map[int64]Snapshot
	nextSnapID int64
}

func newMemoryStore() *memoryStore {
	return &memoryStore{visits: map[string]*PodStat{}, snapshots: map[int64]Snapshot{}}
}

func (s *memoryStore) Kind() string { return "memory" }

func (s *memoryStore) RecordVisit(_ context.Context, pod string) (int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.visits[pod]
	if !ok {
		st = &PodStat{Pod: pod}
		s.visits[pod] = st
	}
	st.Visits++
	st.LastSeen = time.Now()

	var total int64
	for _, v := range s.visits {
		total += v.Visits
	}
	return st.Visits, total, nil
}

func (s *memoryStore) Stats(context.Context) ([]PodStat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]PodStat, 0, len(s.visits))
	for _, v := range s.visits {
		out = append(out, *v)
	}
	slices.SortFunc(out, func(a, b PodStat) int { return strings.Compare(a.Pod, b.Pod) })
	return out, nil
}

func (s *memoryStore) ResetStats(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.visits)
	return nil
}

func (s *memoryStore) AddMessage(_ context.Context, m Message) (Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.nextID++
	m.ID = s.nextID
	m.CreatedAt = time.Now()
	s.messages = append(s.messages, m)
	if len(s.messages) > memoryMessagesCap {
		s.messages = slices.Clone(s.messages[len(s.messages)-memoryMessagesCap:])
	}
	return m, nil
}

func (s *memoryStore) Messages(_ context.Context, limit int) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n := min(limit, len(s.messages))
	out := make([]Message, 0, n)
	for i := len(s.messages) - 1; i >= len(s.messages)-n; i-- {
		out = append(out, s.messages[i])
	}
	return out, nil
}

func (s *memoryStore) MessagesUpTo(_ context.Context, maxID int64) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.messagesUpTo(maxID), nil
}

func (s *memoryStore) messagesUpTo(maxID int64) []Message {
	out := []Message{}
	for _, m := range s.messages {
		if m.ID <= maxID {
			out = append(out, m)
		}
	}
	return out
}

func (s *memoryStore) CreateSnapshot(_ context.Context, pod string) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	count, maxID, digest := digestMessages(s.messagesUpTo(s.nextID))
	s.nextSnapID++
	snap := Snapshot{ID: s.nextSnapID, Pod: pod, Messages: count, MaxID: maxID, Digest: digest, CreatedAt: time.Now()}
	s.snapshots[snap.ID] = snap
	return snap, nil
}

func (s *memoryStore) GetSnapshot(_ context.Context, id int64) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap, ok := s.snapshots[id]
	if !ok {
		return Snapshot{}, ErrSnapshotNotFound
	}
	return snap, nil
}

func (s *memoryStore) Ping(context.Context) error { return nil }

func (s *memoryStore) Close() {}
