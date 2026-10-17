package access

import (
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"
)

func entryTotals(s Snapshot) (u, d, connections int64) {
	for _, e := range s.Entries {
		u += e.Upload
		d += e.Download
		connections += e.Connections
	}
	return
}

func TestUnspecifiedTargetsPreserveBytesWithoutInventingDestinations(t *testing.T) {
	for _, ip := range []string{"::", "0.0.0.0", "::ffff:0.0.0.0"} {
		c := NewCollector(Config{Enabled: true})
		f := c.NewFlow(123, Target{IP: ip, Port: 30525, Network: "tcp", DomainSource: "ip"})
		f.Add(3, 5)
		s := c.Drain()
		if len(s.Entries) != 1 || s.Entries[0].IP != "" || s.Entries[0].Port != 0 || s.Entries[0].DomainSource != "unknown" {
			t.Fatalf("placeholder treated as destination: %+v", s)
		}
		if u, d, _ := entryTotals(s); u != 3 || d != 5 {
			t.Fatalf("accounting changed: %d/%d", u, d)
		}
		target := (Target{Domain: "example.com", IP: ip, Port: 443, Network: "tcp", DomainSource: "request"}).Normalize()
		if target.Domain != "example.com" || target.IP != "" || target.Port != 443 || target.DomainSource != "request" {
			t.Fatalf("request domain lost: %+v", target)
		}
	}
}

func TestCollectorSlicesLiveFlowAndKeepsDisabledTail(t *testing.T) {
	c := NewCollector(Config{BucketSeconds: 300, MaxEntries: 2})
	at := int64(901)
	c.now = func() time.Time { return time.Unix(at, 0) }
	flow := c.NewFlow(123, Target{Domain: "VIDEO.Example.", Port: 443, Network: "tcp", DomainSource: "request"})
	flow.Add(99, 99)
	if len(c.Drain().Entries) != 0 {
		t.Fatal("disabled collector recorded bytes")
	}
	c.Configure(Config{Enabled: true, BucketSeconds: 300, MaxEntries: 2})
	flow.Add(3, 5)
	at = 1201
	flow.Add(7, 11)
	s := c.Drain()
	if len(s.Entries) != 2 || s.Entries[0].BucketAt != 900 || s.Entries[1].BucketAt != 1200 {
		t.Fatalf("buckets: %+v", s)
	}
	if u, d, n := entryTotals(s); u != 10 || d != 16 || n != 1 {
		t.Fatalf("totals %d/%d connections=%d", u, d, n)
	}
	if e := s.Entries[0]; e.Domain != "video.example" || e.UserID != 123 || e.DomainSource != "request" || e.FirstSeen != 901 || e.LastSeen != 901 {
		t.Fatalf("metadata %+v", e)
	}
	flow.Add(13, 17)
	c.Configure(Config{})
	flow.Add(99, 99)
	if u, d, n := entryTotals(c.Drain()); u != 13 || d != 17 || n != 0 {
		t.Fatalf("disable lost tail: %d/%d %d", u, d, n)
	}
	if len(c.Drain().Entries) != 0 {
		t.Fatal("drain repeated bytes")
	}
	c.Configure(Config{Enabled: true})
	flow.Add(19, 23)
	if u, d, _ := entryTotals(c.Drain()); u != 19 || d != 23 {
		t.Fatalf("old flow after reenable %d/%d", u, d)
	}
}

func TestCollectorConcurrentDrainNoLostOrDuplicatedBytes(t *testing.T) {
	c := NewCollector(Config{Enabled: true})
	const workers = 8
	const rounds = 1000
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := c.NewFlow(i+1, Target{IP: "192.0.2.1", Port: 443, Network: "tcp"})
			for j := 0; j < rounds; j++ {
				f.Add(3, 5)
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	var u, d, n int64
	collect := func() { a, b, v := entryTotals(c.Drain()); u += a; d += b; n += v }
	for {
		select {
		case <-done:
			collect()
			if u != workers*rounds*3 || d != workers*rounds*5 || n != workers {
				t.Fatalf("concurrent total %d/%d starts=%d", u, d, n)
			}
			return
		default:
			collect()
			runtime.Gosched()
		}
	}
}

func TestCollectorBoundsUnknownUsersAndDestinationCardinality(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		c := NewCollector(Config{Enabled: true, MaxEntries: 2})
		for i := 1; i <= 1000; i++ {
			target := Target{Network: "udp"}
			if !unknown {
				target.Domain = fmt.Sprintf("d%d.example", i)
				target.DomainSource = "request"
			}
			c.NewFlow(i, target).Add(1, 2)
		}
		s := c.Drain()
		if len(s.Entries) != 2 || s.DroppedEntries != 998 {
			t.Fatalf("bound unknown=%v %+v", unknown, s)
		}
		if unknown {
			for _, e := range s.Entries {
				if e.Domain != "" || e.IP != "" || e.Port != 0 || e.DomainSource != "unknown" {
					t.Fatalf("unknown attribution %+v", e)
				}
			}
		}
		c.NewFlow(1001, Target{}).Add(3, 4)
		if len(c.Drain().Entries) != 1 {
			t.Fatal("capacity not recovered by drain")
		}
	}
}

func TestCollectorResizeRetainsPendingBytes(t *testing.T) {
	c := NewCollector(Config{Enabled: true, MaxEntries: 3})
	for i := 1; i <= 3; i++ {
		c.NewFlow(i, Target{}).Add(1, 1)
	}
	c.Configure(Config{Enabled: false, MaxEntries: 1})
	if u, d, _ := entryTotals(c.Drain()); u != 3 || d != 3 {
		t.Fatal("resize dropped pending tail")
	}
	c.Configure(Config{Enabled: true, MaxEntries: 1})
	c.NewFlow(1, Target{}).Add(1, 1)
	c.NewFlow(2, Target{}).Add(1, 1)
	if s := c.Drain(); len(s.Entries) != 1 || s.DroppedEntries != 1 {
		t.Fatalf("new limit %+v", s)
	}
}

func TestAccessWireContractAndMetadataNormalization(t *testing.T) {
	cfg := Config{Enabled: true, BucketSeconds: 300, MaxEntries: 2000}
	data, _ := json.Marshal(cfg)
	if string(data) != "{\"enabled\":true,\"bucket_seconds\":300,\"max_entries\":2000}" {
		t.Fatalf("config JSON %s", data)
	}
	data, _ = json.Marshal(Entry{})
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	want := []string{"bucket_at", "connections", "d", "domain", "domain_source", "first_seen", "ip", "last_seen", "network", "port", "u", "user_id"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("entry fields %v", names)
	}
	data, _ = json.Marshal((*Collector)(nil).Drain())
	if string(data) != "{\"entries\":[],\"dropped_entries\":0}" {
		t.Fatalf("snapshot JSON %s", data)
	}
	normalized := (Target{IP: "::ffff:192.0.2.1", Network: "UDP", Port: 53}).Normalize()
	if normalized.IP != "192.0.2.1" || normalized.Network != "udp" || normalized.DomainSource != "ip" {
		t.Fatalf("normalized %+v", normalized)
	}
	invalid := (Target{Domain: "http://client.example/a", IP: "client-IP", Port: 123, Network: "bad"}).Normalize()
	if invalid.Domain != "" || invalid.IP != "" || invalid.Port != 0 || invalid.DomainSource != "unknown" || invalid.Network != "unknown" {
		t.Fatalf("invalid metadata %+v", invalid)
	}
}
