package resetradar

import (
	"sort"
	"strings"
	"time"
)

// Stage describes the confidence of a reset signal.
type Stage uint8

const (
	StageUnknown Stage = iota
	StagePossible
	StageImminent
	StageConfirmed
)

func (s Stage) String() string {
	switch s {
	case StagePossible:
		return "possible"
	case StageImminent:
		return "imminent"
	case StageConfirmed:
		return "confirmed"
	default:
		return "unknown"
	}
}

type Signal struct {
	ID                 string
	Source             string
	Text               string
	URL                string
	CreatedAt          time.Time
	Stage              Stage
	EventID            string
	Group              string
	Type               string
	Scope              string
	Preview            bool
	TimelineSource     string
	SourceLabel        string
	AnnouncementState  string
	ObservationResult  string
	VerificationStatus string
	OfficialWindow     *OfficialWindow
}

type OfficialWindow struct {
	Label    string
	StartAt  time.Time
	EndAt    time.Time
	TimeZone string
}

type SourceError struct {
	Source string
	Err    error
}

func (e SourceError) Error() string {
	if e.Err == nil {
		return e.Source
	}
	return e.Source + ": " + e.Err.Error()
}

func (e SourceError) Unwrap() error { return e.Err }

type Snapshot struct {
	CheckedAt    time.Time
	UpdatedAt    time.Time
	Signals      []Signal
	SourceErrors []SourceError
}

func (s Snapshot) HighestStage() Stage {
	highest := StageUnknown
	for _, signal := range s.Signals {
		if signal.Stage > highest {
			highest = signal.Stage
		}
	}
	return highest
}

func mergeSignals(dst map[string]Signal, signals []Signal) {
	for _, signal := range signals {
		signal.ID = strings.TrimSpace(signal.ID)
		if signal.ID == "" {
			continue
		}
		current, exists := dst[signal.ID]
		if !exists || !signal.CreatedAt.Before(current.CreatedAt) {
			dst[signal.ID] = signal
		}
	}
}

func sortedSignals(byID map[string]Signal) []Signal {
	result := make([]Signal, 0, len(byID))
	for _, signal := range byID {
		result = append(result, signal)
	}
	sort.Slice(result, func(i, j int) bool {
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return result
}
