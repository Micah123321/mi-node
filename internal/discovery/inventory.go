package discovery

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const DefaultDirectory = "/etc/mi-node/update-agent"

type Inventory struct {
	InstallationID string `json:"installation_id"`
	Version        string `json:"version"`
	OS             string `json:"os"`
	Arch           string `json:"arch"`
}

func Load(directory, version string) (Inventory, error) {
	if directory == "" {
		directory = DefaultDirectory
	}
	id, err := installationID(directory)
	if err != nil {
		return Inventory{}, err
	}
	return Inventory{id, version, runtime.GOOS, runtime.GOARCH}, nil
}

func installationID(directory string) (string, error) {
	path := filepath.Join(directory, "discovery-id")
	if id, err := readID(path); !errors.Is(err, os.ErrNotExist) {
		return id, err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
	f, err := os.CreateTemp(directory, ".discovery-id-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return "", err
	}
	if _, err := io.WriteString(f, id+"\n"); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	// Publish a complete file without replacing another process's winning ID.
	if err := os.Link(f.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	return readID(path)
}

func readID(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		return "", fmt.Errorf("discovery ID must be a regular private file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, actual) {
		return "", fmt.Errorf("discovery ID changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, 38))
	if err != nil {
		return "", err
	}
	id := strings.TrimSuffix(string(data), "\n")
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return "", fmt.Errorf("invalid discovery UUID")
	}
	if decoded, err := hex.DecodeString(strings.ReplaceAll(id, "-", "")); err != nil || len(decoded) != 16 {
		return "", fmt.Errorf("invalid discovery UUID")
	}
	return id, nil
}
