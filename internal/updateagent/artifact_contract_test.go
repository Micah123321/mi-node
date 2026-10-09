package updateagent

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestContractDownloadIntegrityLeavesNoExecutable(t *testing.T) {
	payload := []byte("fixture binary bytes")
	sum := fmt.Sprintf("%x", sha256.Sum256(payload))
	for _, tc := range []struct {
		name, sha string
		size      int64
		chunked   bool
		code      string
	}{
		{"sha", strings.Repeat("0", 64), int64(len(payload)), false, "checksum_mismatch"},
		{"size_header", sum, int64(len(payload) + 1), false, "size_mismatch"},
		{"size_stream_short", sum, int64(len(payload) + 1), true, "size_mismatch"},
		{"size_stream_long", sum, int64(len(payload) - 1), true, "size_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if tc.chunked {
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write(payload)
			}))
			defer srv.Close()
			client := DownloadClient()
			client.Transport = srv.Client().Transport
			dir := t.TempDir()
			dest := filepath.Join(dir, "stage")
			err := Download(context.Background(), client, Artifact{URL: srv.URL, SHA: tc.sha, Size: tc.size}, dest)
			if err == nil || err.Error() != tc.code {
				t.Fatalf("error %v want %s", err, tc.code)
			}
			entries, e := os.ReadDir(dir)
			if e != nil {
				t.Fatal(e)
			}
			if len(entries) != 0 {
				t.Fatalf("failed artifact left residual files: %v", entries)
			}
			if hits.Load() != 2 {
				t.Fatalf("attempts=%d", hits.Load())
			}
		})
	}
}
func TestContractDownloadRejectsHTTPSDowngrade(t *testing.T) {
	var hits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); _, _ = w.Write([]byte("x")) }))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, plain.URL, http.StatusFound) }))
	defer tls.Close()
	client := DownloadClient()
	client.Transport = tls.Client().Transport
	dir := t.TempDir()
	err := Download(context.Background(), client, Artifact{URL: tls.URL, SHA: fmt.Sprintf("%x", sha256.Sum256([]byte("x"))), Size: 1}, filepath.Join(dir, "stage"))
	if err == nil {
		t.Fatal("HTTPS downgrade accepted")
	}
	if hits.Load() != 0 {
		t.Fatalf("HTTP destination contacted %d times", hits.Load())
	}
	entries, e := os.ReadDir(dir)
	if e != nil {
		t.Fatal(e)
	}
	if len(entries) != 0 {
		t.Fatalf("residual files %v", entries)
	}
}
func TestContractDownloadValidIntegrity(t *testing.T) {
	payload := []byte("verified fixture")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) }))
	defer srv.Close()
	client := DownloadClient()
	client.Transport = srv.Client().Transport
	dest := filepath.Join(t.TempDir(), "stage")
	if err := Download(context.Background(), client, Artifact{URL: srv.URL, SHA: fmt.Sprintf("%x", sha256.Sum256(payload)), Size: int64(len(payload))}, dest); err != nil {
		t.Fatal(err)
	}
	contractContents(t, dest, string(payload))
}
func TestContractELFArchitecture(t *testing.T) {
	for _, tc := range []struct {
		name, arch string
		machine    elf.Machine
		class      byte
		kind       elf.Type
		want       bool
	}{
		{"amd64", "amd64", elf.EM_X86_64, 2, elf.ET_EXEC, true},
		{"arm64", "arm64", elf.EM_AARCH64, 2, elf.ET_DYN, true},
		{"wrong_machine", "amd64", elf.EM_AARCH64, 2, elf.ET_EXEC, false},
		{"unsupported_arch", "386", elf.EM_X86_64, 2, elf.ET_EXEC, false},
		{"wrong_class", "amd64", elf.EM_X86_64, 1, elf.ET_EXEC, false},
		{"not_executable", "amd64", elf.EM_X86_64, 2, elf.ET_REL, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := make([]byte, 64)
			copy(b, []byte{0x7f, 'E', 'L', 'F', tc.class, 1, 1})
			binary.LittleEndian.PutUint16(b[16:], uint16(tc.kind))
			binary.LittleEndian.PutUint16(b[18:], uint16(tc.machine))
			binary.LittleEndian.PutUint32(b[20:], 1)
			binary.LittleEndian.PutUint16(b[52:], 64)
			path := filepath.Join(t.TempDir(), "fixture")
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			if err := ValidateELF(path, tc.arch); (err == nil) != tc.want {
				t.Fatalf("ValidateELF=%v want valid=%v", err, tc.want)
			}
		})
	}
	t.Run("non_elf", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "text")
		contractWrite(t, p, "not ELF")
		if err := ValidateELF(p, "amd64"); err == nil {
			t.Fatal("non ELF accepted")
		}
	})
}
