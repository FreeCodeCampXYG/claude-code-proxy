package monitor

import (
	"sync"
	"testing"
	"time"
)

func TestStatsRecordAndRecentLimit(t *testing.T) {
	stats := NewStats(2)
	stats.Record(Event{RequestID: "1", Success: true, InputTokens: 1, OutputTokens: 2, At: time.Unix(1, 0)})
	stats.Record(Event{RequestID: "2", Success: false, Streaming: true, InputTokens: 3, OutputTokens: 4, CacheTokens: 1, Duration: time.Second, At: time.Unix(2, 0)})
	stats.Record(Event{RequestID: "3", Success: true, At: time.Unix(3, 0)})
	snapshot := stats.Snapshot()
	if snapshot.Totals.Requests != 3 || snapshot.Totals.Success != 2 || snapshot.Totals.Failure != 1 {
		t.Fatalf("unexpected totals: %#v", snapshot.Totals)
	}
	if len(snapshot.Recent) != 2 || snapshot.Recent[0].RequestID != "2" || snapshot.Recent[1].RequestID != "3" {
		t.Fatalf("unexpected recent events: %#v", snapshot.Recent)
	}
}

func TestStatsSanitizesNegativeValues(t *testing.T) {
	stats := NewStats(1)
	stats.Record(Event{Success: true, InputTokens: -1, OutputTokens: -2, CacheTokens: -3, Duration: -time.Second})
	snapshot := stats.Snapshot()
	if snapshot.Totals.InputTokens != 0 || snapshot.Totals.OutputTokens != 0 || snapshot.Totals.CacheTokens != 0 || snapshot.Totals.TotalDuration != 0 {
		t.Fatalf("unexpected sanitized totals: %#v", snapshot.Totals)
	}
}

func TestStatsReset(t *testing.T) {
	stats := NewStats(2)
	stats.Record(Event{Success: true})
	stats.Reset()
	snapshot := stats.Snapshot()
	if snapshot.Totals.Requests != 0 || len(snapshot.Recent) != 0 {
		t.Fatalf("reset failed: %#v", snapshot)
	}
}

func TestStatsConcurrentRecord(t *testing.T) {
	stats := NewStats(8)
	const workers = 32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats.IncActive()
			stats.Record(Event{Success: true, Streaming: true, InputTokens: 1, OutputTokens: 1})
			stats.DecActive()
		}()
	}
	wg.Wait()
	snapshot := stats.Snapshot()
	if snapshot.Totals.Requests != workers || snapshot.Totals.Success != workers || snapshot.Totals.ActiveRequests != 0 {
		t.Fatalf("unexpected concurrent totals: %#v", snapshot.Totals)
	}
}
