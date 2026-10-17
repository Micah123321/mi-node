package tracker

import (
	"strconv"
	"testing"
)

func TestChildIdentityAliveHashUsesAllIDBits(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("child identities require 64-bit ints")
	}
	base := int64(1) << 40
	parent := map[int]map[string]bool{7: {"192.0.2.1": true}}
	child := map[int]map[string]bool{int(base + 7): {"192.0.2.1": true}}
	if calcAliveIPsHash(parent) == calcAliveIPsHash(child) {
		t.Fatal("high auth-ID bits collide")
	}
	a := map[int]map[string]bool{7: {"::1": true, "192.0.2.1": true}, int(base + 1): {"192.0.2.2": true}}
	b := map[int]map[string]bool{int(base + 1): {"192.0.2.2": true}, 7: {"192.0.2.1": true, "::1": true}}
	if calcAliveIPsHash(a) != calcAliveIPsHash(b) {
		t.Fatal("hash depends on map order")
	}
	tr := New()
	tr.Process(map[int][2]int64{7: {1, 2}, int(base + 1): {3, 4}, int(base + 2): {5, 6}}, parent, 3)
	tr.Process(map[int][2]int64{7: {1, 2}, int(base + 1): {10, 12}, int(base + 2): {5, 6}}, nil, 0)
	got := tr.FlushTraffic()
	if len(got) != 3 || got[int(base+1)] != [2]int64{10, 12} {
		t.Fatalf("auth traffic merged: %v", got)
	}
}
