package monitor

import (
	"sort"
	"sync"
	"time"
)

type Event struct {
	RequestID    string        `json:"request_id,omitempty"`
	Model        string        `json:"model,omitempty"`
	Provider     string        `json:"provider,omitempty"`
	Streaming    bool          `json:"streaming"`
	Success      bool          `json:"success"`
	StatusCode   int           `json:"status_code,omitempty"`
	InputTokens  int           `json:"input_tokens"`
	OutputTokens int           `json:"output_tokens"`
	CacheTokens  int           `json:"cache_tokens"`
	Duration     time.Duration `json:"duration"`
	At           time.Time     `json:"at"`
}

type Totals struct {
	Requests       int64         `json:"requests"`
	Success        int64         `json:"success"`
	Failure        int64         `json:"failure"`
	Streaming      int64         `json:"streaming"`
	NonStreaming   int64         `json:"non_streaming"`
	InputTokens    int64         `json:"input_tokens"`
	OutputTokens   int64         `json:"output_tokens"`
	CacheTokens    int64         `json:"cache_tokens"`
	TotalDuration  time.Duration `json:"total_duration"`
	ActiveRequests int64         `json:"active_requests"`
	LastEventAt    time.Time     `json:"last_event_at"`
}

type Snapshot struct {
	Totals Totals  `json:"totals"`
	Recent []Event `json:"recent"`
}

type Stats struct {
	mu        sync.RWMutex
	totals    Totals
	recent    []Event
	recentCap int
}

func NewStats(recentLimit int) *Stats {
	if recentLimit < 1 {
		recentLimit = 1
	}
	return &Stats{recentCap: recentLimit}
}

func (s *Stats) IncActive() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.totals.ActiveRequests++
	s.mu.Unlock()
}

func (s *Stats) DecActive() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.totals.ActiveRequests > 0 {
		s.totals.ActiveRequests--
	}
	s.mu.Unlock()
}

func (s *Stats) Record(event Event) {
	if s == nil {
		return
	}
	if event.InputTokens < 0 {
		event.InputTokens = 0
	}
	if event.OutputTokens < 0 {
		event.OutputTokens = 0
	}
	if event.CacheTokens < 0 {
		event.CacheTokens = 0
	}
	if event.Duration < 0 {
		event.Duration = 0
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.totals.Requests++
	if event.Success {
		s.totals.Success++
	} else {
		s.totals.Failure++
	}
	if event.Streaming {
		s.totals.Streaming++
	} else {
		s.totals.NonStreaming++
	}
	s.totals.InputTokens += int64(event.InputTokens)
	s.totals.OutputTokens += int64(event.OutputTokens)
	s.totals.CacheTokens += int64(event.CacheTokens)
	s.totals.TotalDuration += event.Duration
	s.totals.LastEventAt = event.At
	s.recent = append(s.recent, event)
	if len(s.recent) > s.recentCap {
		s.recent = append([]Event(nil), s.recent[len(s.recent)-s.recentCap:]...)
	}
}

func (s *Stats) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	recent := append([]Event(nil), s.recent...)
	sort.Slice(recent, func(i, j int) bool { return recent[i].At.Before(recent[j].At) })
	return Snapshot{Totals: s.totals, Recent: recent}
}

func (s *Stats) Reset() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.totals = Totals{}
	s.recent = nil
	s.mu.Unlock()
}
