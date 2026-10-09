package discovery

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"testing"
)

func TestPersistentConcurrentID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "discovery")
	const workers = 32
	results := make(chan Inventory, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			v, err := Load(dir, "dev")
			if err != nil {
				t.Error(err)
				return
			}
			results <- v
		})
	}
	wg.Wait()
	close(results)
	var id string
	count := 0
	for v := range results {
		count++
		if id == "" {
			id = v.InstallationID
		}
		if v.InstallationID != id || v.Version != "dev" || v.OS != runtime.GOOS || v.Arch != runtime.GOARCH {
			t.Fatalf("inconsistent inventory: %+v", v)
		}
	}
	if count != workers {
		t.Fatalf("successful loads: %d", count)
	}
	if !regexp.MustCompile("^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$").MatchString(id) {
		t.Fatalf("invalid v4 UUID: %q", id)
	}
	next, err := Load(dir, "next-build")
	if err != nil || next.InstallationID != id || next.Version != "next-build" {
		t.Fatalf("reload: %+v %v", next, err)
	}
	info, err := os.Stat(filepath.Join(dir, "discovery-id"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("mode: %v", info.Mode())
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("temporary file leak: %v %v", files, err)
	}
}

func TestInvalidIDPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "discovery-id")
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "dev"); err == nil {
		t.Fatal("invalid ID accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "broken" {
		t.Fatal("existing ID overwritten")
	}
	if _, err := Load(filepath.Join(path, "child"), "dev"); err == nil {
		t.Fatal("invalid directory accepted")
	}
}

func TestUnsafeID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "discovery-id")
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("27d50c1a-7e30-4d28-859c-c4b95691d5ce\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Load(dir, "dev"); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestUnsafePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "discovery-id")
	if err := os.WriteFile(path, []byte("27d50c1a-7e30-4d28-859c-c4b95691d5ce"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "dev"); err == nil {
		t.Fatal("public ID accepted")
	}
}
