package tracker

import "testing"

func TestProcessRetiresMissingBaselineWithoutLosingPendingTraffic(t *testing.T) {
	tr := New()
	tr.Process(map[int][2]int64{1: {10, 20}}, nil, 0)
	tr.Process(nil, nil, 0)
	if len(tr.lastSeen) != 0 {
		t.Fatalf("retained %d missing user baselines", len(tr.lastSeen))
	}
	tr.Process(map[int][2]int64{1: {30, 40}}, nil, 0)
	if got := tr.FlushTraffic()[1]; got != [2]int64{40, 60} {
		t.Fatalf("reappearing user's new counters or pending bytes lost: %v", got)
	}
}

func TestProcessBaselineBoundedByCurrentUsers(t *testing.T) {
	tr := New()
	for uid := 1; uid <= 1000; uid++ {
		tr.Process(map[int][2]int64{uid: {1, 2}}, nil, 0)
		tr.FlushTraffic()
	}
	if len(tr.lastSeen) != 1 {
		t.Fatalf("user churn retained %d baselines, want 1", len(tr.lastSeen))
	}
}

func TestProcessAndConcurrentFlushPreserveEveryByte(t *testing.T) {
	tr := New()
	done := make(chan struct{})
	result := make(chan [2]int64, 1)
	go func() {
		var total [2]int64
		for {
			for _, bytes := range tr.FlushTraffic() {
				total[0] += bytes[0]
				total[1] += bytes[1]
			}
			select {
			case <-done:
				for _, bytes := range tr.FlushTraffic() {
					total[0] += bytes[0]
					total[1] += bytes[1]
				}
				result <- total
				return
			default:
				tr.CurrentOnline()
			}
		}
	}()
	for i := int64(1); i <= 10000; i++ {
		tr.Process(map[int][2]int64{1: {i, i * 2}}, nil, 0)
	}
	close(done)
	if got := <-result; got != [2]int64{10000, 20000} {
		t.Fatalf("concurrent flush changed accounting totals: %v", got)
	}
}

func BenchmarkProcessRetainedUsers(b *testing.B) {
	tr := New()
	traffic := make(map[int][2]int64, 10000)
	for uid := 1; uid <= 10000; uid++ {
		traffic[uid] = [2]int64{100, 200}
	}
	tr.Process(traffic, nil, 0)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		tr.Process(traffic, nil, 0)
	}
}
