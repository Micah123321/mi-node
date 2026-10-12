package discovery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestEnrollmentStatusStrictRead(t *testing.T) {
	valid := EnrollmentStatus{State: "retrying", Stage: "enroll", Code: "invalid_credentials", UpdatedAt: "2026-10-12T08:09:10Z"}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, EnrollmentStatusFile)
	if ReadEnrollmentStatus(directory) != nil {
		t.Fatal("missing status accepted")
	}
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if got := ReadEnrollmentStatus(directory); !reflect.DeepEqual(got, &valid) {
		t.Fatalf("valid status: %+v", got)
	}
	cases := map[string]string{
		"broken":        "{",
		"legacy":        `{"state":"retrying"}`,
		"state":         strings.Replace(string(encoded), "retrying", "server-error-token", 1),
		"stage":         strings.Replace(string(encoded), "enroll", "private-request", 1),
		"code":          strings.Replace(string(encoded), "invalid_credentials", "injected-token-error", 1),
		"fraction":      strings.Replace(string(encoded), "08:09:10Z", "08:09:10.123Z", 1),
		"offset":        strings.Replace(string(encoded), "08:09:10Z", "08:09:10+00:00", 1),
		"invalid-time":  strings.Replace(string(encoded), "08:09:10Z", "24:09:10Z", 1),
		"null":          strings.Replace(string(encoded), `"code":"invalid_credentials"`, `"code":null`, 1),
		"unknown-field": strings.TrimSuffix(string(encoded), "}") + `,"token":"injected-token-error"}`,
		"duplicate":     strings.TrimSuffix(string(encoded), "}") + `,"state":"registered"}`,
		"trailing":      string(encoded) + " {}",
		"oversize":      string(encoded) + strings.Repeat(" ", 4097),
		"array":         "[" + string(encoded) + "]",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if got := ReadEnrollmentStatus(directory); got != nil {
				t.Fatalf("bad status accepted: %+v", got)
			}
			if _, err := Load(directory, "dev"); err != nil {
				t.Fatalf("bad optional status blocked discovery: %v", err)
			}
		})
	}
}

func TestEnrollmentStatusPrivateRegularFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, EnrollmentStatusFile)
	data := []byte(`{"state":"registered","stage":"units","code":"","updated_at":"2026-10-12T08:09:10Z"}`)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if ReadEnrollmentStatus(directory) != nil {
		t.Fatal("directory accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err == nil {
		if ReadEnrollmentStatus(directory) != nil {
			t.Fatal("symlink accepted")
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if ReadEnrollmentStatus(directory) != nil {
			t.Fatal("public status accepted")
		}
		if _, err := Load(directory, "dev"); err != nil {
			t.Fatalf("public optional status blocked discovery: %v", err)
		}
	}
}
