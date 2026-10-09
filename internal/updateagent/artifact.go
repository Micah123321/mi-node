package updateagent

import (
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"
)

type VersionInfo struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

func PrintVersion(version string) error {
	return json.NewEncoder(os.Stdout).Encode(VersionInfo{version, runtime.GOOS, runtime.GOARCH})
}
func Probe(path string) (VersionInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, path, "version", "--json").Output()
	var v VersionInfo
	if e != nil {
		return v, fmt.Errorf("binary_version_mismatch")
	}
	if e = json.Unmarshal(b, &v); e != nil || !versionRE.MatchString(v.Version) {
		return v, fmt.Errorf("binary_version_mismatch")
	}
	return v, nil
}
func ValidateELF(path, arch string) error {
	f, e := elf.Open(path)
	if e != nil {
		return fmt.Errorf("arch_mismatch")
	}
	defer f.Close()
	expected := elf.EM_X86_64
	if arch == "arm64" {
		expected = elf.EM_AARCH64
	} else if arch != "amd64" {
		return fmt.Errorf("arch_mismatch")
	}
	if f.Machine != expected || f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || (f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN) {
		return fmt.Errorf("arch_mismatch")
	}
	return nil
}
func DownloadClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return fmt.Errorf("too many redirects")
		}
		req.Header.Del("Authorization")
		return validHTTPS(req.URL.String())
	}}
}
func Download(ctx context.Context, c *http.Client, a Artifact, dest string) error {
	if e := validHTTPS(a.URL); e != nil {
		return e
	}
	if a.Size <= 0 || a.Size > 512<<20 || !hashRE.MatchString(a.SHA) {
		return fmt.Errorf("invalid artifact")
	}
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		last = downloadOnce(ctx, c, a, dest)
		if last == nil {
			return nil
		}
		_ = os.Remove(dest)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return last
}
func downloadOnce(parent context.Context, c *http.Client, a Artifact, dest string) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "GET", a.URL, nil)
	if e != nil {
		return fmt.Errorf("download_failed")
	}
	res, e := c.Do(req)
	if e != nil {
		return fmt.Errorf("download_failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("download_failed")
	}
	if res.ContentLength >= 0 && res.ContentLength != a.Size {
		return fmt.Errorf("size_mismatch")
	}
	f, e := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if e != nil {
		return e
	}
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, a.Size+1))
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return fmt.Errorf("download_failed")
	}
	if n != a.Size {
		return fmt.Errorf("size_mismatch")
	}
	if hex.EncodeToString(h.Sum(nil)) != a.SHA {
		return fmt.Errorf("checksum_mismatch")
	}
	return nil
}
