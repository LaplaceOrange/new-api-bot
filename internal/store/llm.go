package store

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	llmConfigBucket   = []byte("llm_config")
	llmGroupsBucket   = []byte("llm_groups")
	llmSessionsBucket = []byte("llm_sessions")
	llmJobsBucket     = []byte("llm_jobs")
	llmRatesBucket    = []byte("llm_rates")
	ErrLLMLimited     = errors.New("对话次数已达限额")
	ErrLLMQueueFull   = errors.New("对话队列已满，请稍后重试")
	ErrLLMVersion     = errors.New("会话已清空或过期")
)

// Only opaque ciphertext reaches these records; authentication lives in bot.
type LLMSession struct {
	Version    uint64    `json:"version"`
	Ciphertext string    `json:"ciphertext,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
}
type LLMJob struct {
	ID        string    `json:"id"`
	Session   string    `json:"session"`
	Group     string    `json:"group,omitempty"`
	Actor     string    `json:"actor"`
	Version   uint64    `json:"version"`
	Payload   string    `json:"payload"`
	Result    string    `json:"result,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	ExpiresAt time.Time `json:"expires_at"`
}
type LLMRate struct {
	Day    string      `json:"day"`
	Count  int         `json:"count"`
	Recent []time.Time `json:"recent"`
}

func llmDecode(b *bolt.Bucket, key string, out any) error {
	data := b.Get([]byte(key))
	if data == nil {
		return ErrNotFound
	}
	return json.Unmarshal(data, out)
}
func (s *Store) LLMConfig() (string, error) {
	var out string
	err := s.db.View(func(tx *bolt.Tx) error { return llmDecode(tx.Bucket(llmConfigBucket), "v1", &out) })
	return out, err
}
func (s *Store) PutLLMConfig(ciphertext string) error {
	if len(ciphertext) > 256<<10 {
		return errors.New("LLM 配置过大")
	}
	return s.db.Update(func(tx *bolt.Tx) error { return rssPut(tx.Bucket(llmConfigBucket), "v1", ciphertext) })
}
func (s *Store) LLMGroupEnabled(group string) (bool, error) {
	var enabled bool
	err := s.db.View(func(tx *bolt.Tx) error { return llmDecode(tx.Bucket(llmGroupsBucket), group, &enabled) })
	if errors.Is(err, ErrNotFound) {
		err = nil
	}
	return enabled, err
}
func (s *Store) SetLLMGroup(group string, enabled bool, now time.Time) error {
	if group == "" {
		return errors.New("群 ID 不能为空")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := rssPut(tx.Bucket(llmGroupsBucket), group, enabled); err != nil {
			return err
		}
		if enabled {
			return nil
		}
		b := tx.Bucket(llmJobsBucket)
		var jobs []LLMJob
		if err := b.ForEach(func(_, data []byte) error {
			var j LLMJob
			if e := json.Unmarshal(data, &j); e != nil {
				return e
			}
			if j.Group == group && j.Status == "queued" {
				j.Status = "canceled"
				j.UpdatedAt = now
				jobs = append(jobs, j)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, j := range jobs {
			if err := rssPut(b, j.ID, j); err != nil {
				return err
			}
		}
		return nil
	})
}
func llmSessionTx(tx *bolt.Tx, key string, now time.Time) (LLMSession, error) {
	var session LLMSession
	err := llmDecode(tx.Bucket(llmSessionsBucket), key, &session)
	if errors.Is(err, ErrNotFound) {
		return session, nil
	}
	if err != nil {
		return session, err
	}
	if session.Ciphertext != "" && !session.ExpiresAt.After(now) {
		session.Version++
		session.Ciphertext = ""
		if err = rssPut(tx.Bucket(llmSessionsBucket), key, session); err != nil {
			return session, err
		}
	}
	return session, nil
}
func (s *Store) LLMSession(key string, now time.Time) (LLMSession, error) {
	var out LLMSession
	err := s.db.View(func(tx *bolt.Tx) error {
		e := llmDecode(tx.Bucket(llmSessionsBucket), key, &out)
		if errors.Is(e, ErrNotFound) {
			return nil
		}
		return e
	})
	if err != nil || out.Ciphertext == "" || out.ExpiresAt.After(now) {
		return out, err
	}
	err = s.db.Update(func(tx *bolt.Tx) (err error) { out, err = llmSessionTx(tx, key, now); return })
	return out, err
}
func (s *Store) ResetLLMSession(key string, now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		session, err := llmSessionTx(tx, key, now)
		if err != nil {
			return err
		}
		session.Version++
		session.Ciphertext = ""
		session.ExpiresAt = now
		return rssPut(tx.Bucket(llmSessionsBucket), key, session)
	})
}
func (s *Store) PutLLMSession(key string, version uint64, ciphertext string, expires, now time.Time) error {
	if len(ciphertext) > 512<<10 {
		return errors.New("会话过大")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		session, err := llmSessionTx(tx, key, now)
		if err != nil {
			return err
		}
		if session.Version != version {
			return ErrLLMVersion
		}
		session.Ciphertext = ciphertext
		session.ExpiresAt = expires
		return rssPut(tx.Bucket(llmSessionsBucket), key, session)
	})
}
func llmRateTx(tx *bolt.Tx, actor, day string, now time.Time) (LLMRate, error) {
	var rate LLMRate
	err := llmDecode(tx.Bucket(llmRatesBucket), actor, &rate)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return rate, err
	}
	if rate.Day != day {
		rate.Day = day
		rate.Count = 0
	}
	var recent []time.Time
	for _, at := range rate.Recent {
		if at.After(now.Add(-time.Minute)) {
			recent = append(recent, at)
		}
	}
	rate.Recent = recent
	return rate, nil
}
func (s *Store) LLMRate(actor, day string, now time.Time) (LLMRate, error) {
	var rate LLMRate
	err := s.db.View(func(tx *bolt.Tx) (err error) { rate, err = llmRateTx(tx, actor, day, now); return })
	return rate, err
}

// AdmitLLMJob atomically deduplicates, reserves quota, enforces queue capacity
// and captures the session revision. A rejected admission never consumes quota.
func (s *Store) AdmitLLMJob(job LLMJob, day string, minute, daily, capacity int) (bool, error) {
	if job.ID == "" || job.Actor == "" || job.Session == "" || job.Payload == "" || len(job.Payload) > 256<<10 {
		return false, errors.New("无效对话任务")
	}
	created := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(llmJobsBucket)
		if b.Get([]byte(job.ID)) != nil {
			return nil
		}
		count := 0
		if err := b.ForEach(func(_, v []byte) error {
			var j LLMJob
			if e := json.Unmarshal(v, &j); e != nil {
				return e
			}
			switch j.Status {
			case "queued", "running", "ready", "sending":
				count++
			}
			return nil
		}); err != nil {
			return err
		}
		if count >= capacity {
			return ErrLLMQueueFull
		}
		rate, err := llmRateTx(tx, job.Actor, day, job.CreatedAt)
		if err != nil {
			return err
		}
		if len(rate.Recent) >= minute || rate.Count >= daily {
			return ErrLLMLimited
		}
		session, err := llmSessionTx(tx, job.Session, job.CreatedAt)
		if err != nil {
			return err
		}
		job.Version = session.Version
		job.Status = "queued"
		job.UpdatedAt = job.CreatedAt
		rate.Count++
		rate.Recent = append(rate.Recent, job.CreatedAt)
		if err = rssPut(tx.Bucket(llmRatesBucket), job.Actor, rate); err != nil {
			return err
		}
		if err = rssPut(b, job.ID, job); err != nil {
			return err
		}
		created = true
		return nil
	})
	return created, err
}
func (s *Store) LLMJob(id string) (LLMJob, error) {
	var job LLMJob
	err := s.db.View(func(tx *bolt.Tx) error { return llmDecode(tx.Bucket(llmJobsBucket), id, &job) })
	return job, err
}
func (s *Store) PendingLLMJobs() ([]LLMJob, error) {
	var jobs []LLMJob
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(llmJobsBucket).ForEach(func(_, v []byte) error {
			var j LLMJob
			if err := json.Unmarshal(v, &j); err != nil {
				return err
			}
			if j.Status == "queued" || j.Status == "ready" {
				jobs = append(jobs, j)
			}
			return nil
		})
	})
	sort.Slice(jobs, func(i, j int) bool {
		if jobs[i].CreatedAt.Equal(jobs[j].CreatedAt) {
			return jobs[i].ID < jobs[j].ID
		}
		return jobs[i].CreatedAt.Before(jobs[j].CreatedAt)
	})
	return jobs, err
}
func (s *Store) TransitionLLMJob(id, from, to, result string, now time.Time) (bool, error) {
	changed := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		var j LLMJob
		b := tx.Bucket(llmJobsBucket)
		if err := llmDecode(b, id, &j); err != nil {
			return err
		}
		if j.Status != from {
			return nil
		}
		j.Status = to
		j.UpdatedAt = now
		if result != "" {
			j.Result = result
		}
		if err := rssPut(b, id, j); err != nil {
			return err
		}
		changed = true
		return nil
	})
	return changed, err
}

// A process crash cannot prove whether a provider charged or QQ sent a reply.
// These jobs must never be automatically regenerated or resent.
func (s *Store) RecoverLLMJobs(now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(llmJobsBucket)
		var jobs []LLMJob
		if err := b.ForEach(func(_, v []byte) error {
			var j LLMJob
			if e := json.Unmarshal(v, &j); e != nil {
				return e
			}
			if j.Status == "running" || j.Status == "sending" {
				j.Status = "uncertain"
				j.UpdatedAt = now
				jobs = append(jobs, j)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, j := range jobs {
			if err := rssPut(b, j.ID, j); err != nil {
				return err
			}
		}
		return nil
	})
}
func (s *Store) PruneLLM(now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(llmSessionsBucket)
		var sessions map[string]LLMSession = map[string]LLMSession{}
		if err := b.ForEach(func(k, v []byte) error {
			var x LLMSession
			if e := json.Unmarshal(v, &x); e != nil {
				return e
			}
			if x.Ciphertext != "" && !x.ExpiresAt.After(now) {
				x.Ciphertext = ""
				x.Version++
				sessions[string(k)] = x
			}
			return nil
		}); err != nil {
			return err
		}
		for k, x := range sessions {
			if err := rssPut(b, k, x); err != nil {
				return err
			}
		}
		jobs := tx.Bucket(llmJobsBucket)
		var remove [][]byte
		if err := jobs.ForEach(func(k, v []byte) error {
			var j LLMJob
			if e := json.Unmarshal(v, &j); e != nil {
				return e
			}
			switch j.Status {
			case "queued", "ready", "running", "sending":
			default:
				if j.UpdatedAt.Before(now.Add(-48 * time.Hour)) {
					remove = append(remove, append([]byte(nil), k...))
				}
			}
			return nil
		}); err != nil {
			return err
		}
		for _, k := range remove {
			if err := jobs.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}
