package discovery

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const EnrollmentStatusFile = "enrollment-status.json"

type EnrollmentStatus struct {
	State     string `json:"state"`
	Stage     string `json:"stage"`
	Code      string `json:"code"`
	UpdatedAt string `json:"updated_at"`
}

func (s EnrollmentStatus) Valid() bool {
	switch s.State {
	case "starting", "retrying", "registered", "disabled", "unsupported":
	default:
		return false
	}
	switch s.Stage {
	case "bootstrap", "layout", "lock", "config", "authority", "enroll", "units", "poll":
	default:
		return false
	}
	switch s.Code {
	case "", "systemd_failed", "shared_binary_layout", "unsupported_platform", "local_disabled", "update_busy", "config_invalid", "authority_unavailable", "credential_missing", "scope_conflict", "scope_mismatch", "invalid_credentials", "validation_failed", "rate_limited", "api_unavailable", "transport_failed", "invalid_api_response", "unknown_error":
	default:
		return false
	}
	t, err := time.Parse(time.RFC3339, s.UpdatedAt)
	return err == nil && len(s.UpdatedAt) == 20 && strings.HasSuffix(s.UpdatedAt, "Z") && t.UTC().Format(time.RFC3339) == s.UpdatedAt
}

// ReadEnrollmentStatus is optional telemetry; invalid or legacy files are ignored.
func ReadEnrollmentStatus(directory string) *EnrollmentStatus {
	if directory == "" {
		directory = DefaultDirectory
	}
	path := filepath.Join(directory, EnrollmentStatusFile)
	info, err := os.Lstat(path)
	if err != nil || !privateStatusFile(info) {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil || !os.SameFile(info, actual) || !privateStatusFile(actual) {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(data) > 4096 {
		return nil
	}
	// Reject missing, duplicate, unknown and non-string fields, including null.
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil
	}
	fields := make(map[string]string, 4)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil
		}
		name, ok := key.(string)
		if !ok {
			return nil
		}
		switch name {
		case "state", "stage", "code", "updated_at":
		default:
			return nil
		}
		if _, exists := fields[name]; exists {
			return nil
		}
		var value *string
		if decoder.Decode(&value) != nil || value == nil {
			return nil
		}
		fields[name] = *value
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') || len(fields) != 4 {
		return nil
	}
	if _, err = decoder.Token(); err != io.EOF {
		return nil
	}
	s := EnrollmentStatus{State: fields["state"], Stage: fields["stage"], Code: fields["code"], UpdatedAt: fields["updated_at"]}
	if !s.Valid() {
		return nil
	}
	return &s
}

func privateStatusFile(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Size() <= 4096 && (runtime.GOOS == "windows" || info.Mode().Perm() == 0600)
}
