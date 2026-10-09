package tracker

import "testing"

func TestAliveSnapshotSurvivesNextFlush(t *testing.T) {
	tr := New()
	tr.Process(nil, map[int]map[string]bool{1: {"192.0.2.1": true}}, 1)
	first := tr.FlushAliveIPs()
	tr.Process(nil, map[int]map[string]bool{2: {"192.0.2.2": true}}, 1)
	second := tr.FlushAliveIPs()
	if len(first) != 1 || len(first[1]) != 1 || first[1][0] != "192.0.2.1" {
		t.Fatalf("retained snapshot mutated: %v", first)
	}
	second[2][0] = "changed"
	if first[1][0] != "192.0.2.1" {
		t.Fatal("snapshots share storage")
	}
}
