package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func llmTestJob(id, actor string, now time.Time) LLMJob {
	return LLMJob{ID: id, Session: "s", Actor: actor, Payload: "ciphertext", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
}
func TestLLMAdmissionAtomicLimitsAndDedupe(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	day := "2026-10-03"
	job := llmTestJob("one", "actor", now)
	created, err := s.AdmitLLMJob(job, day, 2, 3, 3)
	if !created || err != nil {
		t.Fatal(created, err)
	}
	created, err = s.AdmitLLMJob(job, day, 2, 3, 3)
	if created || err != nil {
		t.Fatal(created, err)
	}
	if _, err = s.AdmitLLMJob(llmTestJob("two", "actor", now), day, 2, 3, 3); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitLLMJob(llmTestJob("three", "actor", now), day, 2, 3, 3); !errors.Is(err, ErrLLMLimited) {
		t.Fatal(err)
	}
	if _, err = s.AdmitLLMJob(llmTestJob("third", "other", now), day, 2, 3, 3); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitLLMJob(llmTestJob("four", "third", now), day, 2, 3, 3); !errors.Is(err, ErrLLMQueueFull) {
		t.Fatal(err)
	}
	rate, _ := s.LLMRate("third", day, now)
	if rate.Count != 0 {
		t.Fatal("queue rejection consumed quota")
	}
	for _, id := range []string{"one", "two", "third"} {
		_, _ = s.TransitionLLMJob(id, "queued", "sent", "", now)
	}
	if _, err = s.AdmitLLMJob(llmTestJob("later", "actor", now.Add(time.Minute)), day, 2, 3, 3); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AdmitLLMJob(llmTestJob("daily", "actor", now.Add(2*time.Minute)), day, 2, 3, 3); !errors.Is(err, ErrLLMLimited) {
		t.Fatal(err)
	}
	if _, err = s.AdmitLLMJob(llmTestJob("nextday", "actor", now.Add(24*time.Hour)), "2026-10-04", 2, 3, 3); err != nil {
		t.Fatal(err)
	}
}
func TestLLMConcurrentAdmission(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	var wg sync.WaitGroup
	var admitted atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			created, err := s.AdmitLLMJob(llmTestJob(fmt.Sprint(i), "a", now), "d", 6, 100, 32)
			if err == nil && created {
				admitted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if admitted.Load() != 6 {
		t.Fatal(admitted.Load())
	}
}
func TestLLMHistoryResetExpiryAndPrune(t *testing.T) {
	s := openTestStore(t)
	now := time.Now()
	session, err := s.LLMSession("s", now)
	if err != nil || session.Version != 0 {
		t.Fatal(session, err)
	}
	if err = s.PutLLMSession("s", 0, "encrypted", now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if err = s.ResetLLMSession("s", now); err != nil {
		t.Fatal(err)
	}
	if err = s.PutLLMSession("s", 0, "stale", now.Add(time.Hour), now); !errors.Is(err, ErrLLMVersion) {
		t.Fatal(err)
	}
	session, _ = s.LLMSession("s", now)
	if session.Ciphertext != "" || session.Version != 1 {
		t.Fatal(session)
	}
	if err = s.PutLLMSession("s", 1, "encrypted", now.Add(time.Second), now); err != nil {
		t.Fatal(err)
	}
	if err = s.PruneLLM(now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	session, _ = s.LLMSession("s", now.Add(time.Minute))
	if session.Ciphertext != "" || session.Version != 2 {
		t.Fatal(session)
	}
}
func TestLLMRecoveryPersistsAndGroupCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "llm.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, id := range []string{"queued", "running", "ready", "sending"} {
		j := llmTestJob(id, id, now)
		j.Group = "g"
		if _, err = s.AdmitLLMJob(j, "d", 6, 100, 32); err != nil {
			t.Fatal(err)
		}
		if id != "queued" {
			_, _ = s.TransitionLLMJob(id, "queued", id, "encrypted result", now)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.RecoverLLMJobs(now); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"running", "sending"} {
		j, _ := s.LLMJob(id)
		if j.Status != "uncertain" {
			t.Fatal(j)
		}
	}
	j, _ := s.LLMJob("ready")
	if j.Status != "ready" || j.Result != "encrypted result" {
		t.Fatal(j)
	}
	if err = s.SetLLMGroup("g", false, now); err != nil {
		t.Fatal(err)
	}
	j, _ = s.LLMJob("queued")
	if j.Status != "canceled" {
		t.Fatal(j)
	}
	if enabled, err := s.LLMGroupEnabled("unknown"); err != nil || enabled {
		t.Fatal(enabled, err)
	}
}
