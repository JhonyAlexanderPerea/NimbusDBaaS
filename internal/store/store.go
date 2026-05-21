// Package store provides a JSON-file-backed persistent store for NimbusDBaaS.
// It uses only the Go standard library (no CGO, no SQLite driver needed).
package store

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"nimbusDBaaS/internal/models"
)

type data struct {
	Instances []*models.Instance `json:"instances"`
	Logs      []*models.LogEntry `json:"logs"`
	LogSeq    int64              `json:"log_seq"`
}

// Store is a JSON-file-backed, mutex-protected store.
type Store struct {
	mu   sync.RWMutex
	path string
	d    data
}

func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.save() }

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		s.d = data{}
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, &s.d)
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, b, 0644)
}

// ── Instance methods ──────────────────────────────────────────────────────

func (s *Store) CreateInstance(inst *models.Instance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Instances = append(s.d.Instances, inst)
	return s.save()
}

func (s *Store) UpdateInstance(inst *models.Instance) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, v := range s.d.Instances {
		if v.ID == inst.ID {
			s.d.Instances[i] = inst
			return s.save()
		}
	}
	return nil
}

func (s *Store) GetInstance(id string) (*models.Instance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.d.Instances {
		if v.ID == id {
			cp := *v
			return &cp, nil
		}
	}
	return nil, os.ErrNotExist
}

func (s *Store) ListInstances() ([]*models.Instance, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*models.Instance
	for _, v := range s.d.Instances {
		if v.Status != models.StatusDeleted {
			cp := *v
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (s *Store) DeleteInstance(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, v := range s.d.Instances {
		if v.ID == id {
			s.d.Instances[i].Status = models.StatusDeleted
			return s.save()
		}
	}
	return nil
}

// ── Log methods ───────────────────────────────────────────────────────────

func (s *Store) AddLog(level, message, instanceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.LogSeq++
	entry := &models.LogEntry{
		ID:         s.d.LogSeq,
		Level:      level,
		Message:    message,
		InstanceID: instanceID,
		CreatedAt:  time.Now().UTC(),
	}
	s.d.Logs = append(s.d.Logs, entry)
	// Keep only last 500
	if len(s.d.Logs) > 500 {
		s.d.Logs = s.d.Logs[len(s.d.Logs)-500:]
	}
	return s.save()
}

func (s *Store) ListLogs(limit int) ([]*models.LogEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.d.Logs
	if limit > 0 && len(src) > limit {
		src = src[len(src)-limit:]
	}
	result := make([]*models.LogEntry, len(src))
	for i, v := range src {
		cp := *v
		result[i] = &cp
	}
	return result, nil
}
