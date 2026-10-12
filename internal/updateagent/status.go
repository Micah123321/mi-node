package updateagent

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/micah123321/mi-node/internal/discovery"
)

// An attempt tracks stages locally and publishes once, before releasing its lock.
type enrollmentAttempt struct {
	directory        string
	state            string
	stage            string
	code             string
	preserveTerminal bool
	done             bool
}

func newEnrollmentAttempt(directory, stage string) *enrollmentAttempt {
	return &enrollmentAttempt{directory: directory, state: "starting", stage: stage}
}

func (s *enrollmentAttempt) finish(err error) {
	if s.done {
		return
	}
	s.done = true
	if err != nil && s.state != "disabled" && s.state != "unsupported" {
		s.state = "retrying"
		if s.code == "" {
			s.code = enrollmentErrorCode(err)
			if s.code == "unknown_error" && s.stage == "config" {
				s.code = "config_invalid"
			}
		}
	}
	if s.preserveTerminal {
		if previous := discovery.ReadEnrollmentStatus(s.directory); previous != nil && (previous.State == "registered" || previous.State == "disabled") {
			return
		}
	}
	s.write()
}

// Poll diagnostics describe authority contact, not subsequent update task results.
func (a *Agent) publishPollStatus(capability string, err error) {
	if !a.Config.Enrolled {
		return
	}
	status := newEnrollmentAttempt(a.Engine.Dir, "poll")
	status.state = "registered"
	switch {
	case a.Config.Disabled:
		status.state, status.code = "disabled", "local_disabled"
	case capability != "":
		status.state, status.code = "unsupported", "unsupported_platform"
		if capability == "shared_binary_layout" {
			status.code = "shared_binary_layout"
		}
	}
	status.finish(err)
}

func (s *enrollmentAttempt) write() {
	status := discovery.EnrollmentStatus{State: s.state, Stage: s.stage, Code: s.code, UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	if !status.Valid() {
		return
	}
	if os.MkdirAll(s.directory, 0700) == nil {
		_ = AtomicJSON(filepath.Join(s.directory, discovery.EnrollmentStatusFile), status)
	}
}

type enrollmentFailure struct {
	code  string
	cause error
}

func (e *enrollmentFailure) Error() string         { return e.cause.Error() }
func (e *enrollmentFailure) Unwrap() error         { return e.cause }
func enrollmentError(code string, err error) error { return &enrollmentFailure{code: code, cause: err} }

func enrollmentErrorCode(err error) string {
	var failure *enrollmentFailure
	if errors.As(err, &failure) {
		return failure.code
	}
	var api *APIError
	if errors.As(err, &api) {
		switch api.Code {
		case "credential_missing", "scope_conflict", "scope_mismatch", "invalid_credentials", "validation_failed", "rate_limited", "api_unavailable":
			return api.Code
		}
		switch api.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "invalid_credentials"
		case http.StatusConflict:
			return "scope_conflict"
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return "validation_failed"
		case http.StatusTooManyRequests:
			return "rate_limited"
		}
		if api.Status >= 500 {
			return "api_unavailable"
		}
		return "unknown_error"
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "systemd_failed"
	}
	var syntax *json.SyntaxError
	var types *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &types) {
		return "invalid_api_response"
	}
	// Protocol and layout currently return these exact, sanitized errors.
	// Never persist arbitrary error text or a server-supplied code.
	switch err.Error() {
	case "update API transport failed":
		return "transport_failed"
	case "invalid API response", "invalid automatic enrollment response":
		return "invalid_api_response"
	case "shared_binary_layout":
		return "shared_binary_layout"
	}
	return "unknown_error"
}
