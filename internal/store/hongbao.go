package store

import (
	"encoding/json"
	"errors"

	"github.com/fsykk/new-api-bot/internal/model"
	bolt "go.etcd.io/bbolt"
)

func (s *Store) GetHongbao(group string) (model.Hongbao, error) {
	var packet model.Hongbao
	err := s.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte("hongbao")).Get([]byte(group))
		if data == nil {
			return ErrNotFound
		}
		return json.Unmarshal(data, &packet)
	})
	return packet, err
}

// Callers serialize changes to a group's packet across reservation, API write,
// and finalization; reservations survive a process restart.
func (s *Store) PutHongbao(packet model.Hongbao) error {
	if packet.ID == "" || packet.GroupOpenID == "" || packet.QuotaPerUnit <= 0 ||
		packet.TotalQuota <= 0 || packet.TotalCount <= 0 ||
		packet.RemainingQuota < 0 || packet.RemainingCount < 0 ||
		packet.GrantedCount < 0 || packet.GrantedCount > packet.TotalCount {
		return errors.New("红包状态无效")
	}
	data, err := json.Marshal(packet)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("hongbao")).Put([]byte(packet.GroupOpenID), data)
	})
}

func (s *Store) ListPendingHongbaoSummaries() ([]model.Hongbao, error) {
	var packets []model.Hongbao
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("hongbao")).ForEach(func(_, data []byte) error {
			var packet model.Hongbao
			if err := json.Unmarshal(data, &packet); err != nil {
				return err
			}
			if !packet.CompletedAt.IsZero() && !packet.SummarySent {
				packets = append(packets, packet)
			}
			return nil
		})
	})
	return packets, err
}
