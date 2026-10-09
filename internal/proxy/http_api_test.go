package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestHTTPApiAuthRequired(t *testing.T) {
	// Create an HTTPApi with a known API key
	api := &HTTPApi{
		apiKey: "test-key-123",
	}

	tests := []struct {
		name       string
		method     string
		path       string
		apiKey     string
		wantStatus int
	}{
		{"acquire no key", "POST", "/acquire", "", http.StatusUnauthorized},
		{"acquire wrong key", "POST", "/acquire", "wrong-key", http.StatusUnauthorized},
		{"release no key", "POST", "/release", "", http.StatusUnauthorized},
		{"status no key", "GET", "/status", "", http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.apiKey != "" {
				req.Header.Set("x-api-key", tt.apiKey)
			}
			w := httptest.NewRecorder()

			switch {
			case tt.path == "/acquire":
				api.handleAcquire(w, req)
			case tt.path == "/release":
				api.handleRelease(w, req)
			case tt.path == "/status":
				api.handleStatus(w, req)
			}

			if w.Code != tt.wantStatus {
				t.Errorf("got status %d, want %d", w.Code, tt.wantStatus)
			}

			var resp map[string]string
			json.Unmarshal(w.Body.Bytes(), &resp)
			if resp["error"] == "" {
				t.Error("expected error in response body")
			}
		})
	}
}

func TestHTTPApiAcquireCountValidation(t *testing.T) {
	// Validation happens before the scoped port manager is touched, so a bare
	// HTTPApi is enough to exercise these paths.
	api := &HTTPApi{
		apiKey: "test-key",
	}

	tests := []struct {
		name      string
		query     string
		wantError string
	}{
		{"non-numeric count", "?count=abc", "count parameter must be a non-negative integer"},
		{"negative count", "?count=-1", "count parameter must be a non-negative integer"},
		{"count with serial", "?count=1&serial=DEVICE_A", "count and serial parameters are mutually exclusive"},
		{"non-numeric priority", "?priority=high", "priority parameter must be a 32-bit integer"},
		{"oversized priority", "?priority=2147483648", "priority parameter must be a 32-bit integer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/acquire"+tt.query, nil)
			req.Header.Set("x-api-key", "test-key")
			w := httptest.NewRecorder()

			api.handleAcquire(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
			}

			var resp map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("response body is not JSON (%v): %s", err, w.Body.String())
			}
			if resp["error"] != tt.wantError {
				t.Errorf("got error %q, want %q", resp["error"], tt.wantError)
			}
		})
	}
}

func TestHTTPApiAcquireForwardsPriority(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  int32
	}{
		{"omitted defaults to zero", "", 0},
		{"negative", "?priority=-1", -1},
		{"positive", "?priority=5", 5},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The orchestrator refuses the lock, so no scoped port is opened; we
			// only care what the request carried.
			fake := &fakeOrchClient{err: status.Error(codes.ResourceExhausted, "busy")}
			router := &CommandRouter{orchClient: fake, apiKey: "test-key"}
			api := &HTTPApi{apiKey: "test-key", scopedPortManager: NewScopedPortManager(router, nil, 0)}

			req := httptest.NewRequest("POST", "/acquire"+tt.query, nil)
			req.Header.Set("x-api-key", "test-key")
			api.handleAcquire(httptest.NewRecorder(), req)

			if fake.lastAcquire == nil {
				t.Fatal("acquire never reached the orchestrator")
			}
			if got := fake.lastAcquire.Priority; got != tt.want {
				t.Errorf("got priority %d, want %d", got, tt.want)
			}
		})
	}
}

func TestHTTPApiReleaseMissingLockID(t *testing.T) {
	api := &HTTPApi{
		apiKey: "test-key",
	}

	req := httptest.NewRequest("POST", "/release", nil)
	req.Header.Set("x-api-key", "test-key")
	w := httptest.NewRecorder()

	api.handleRelease(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("got status %d, want %d", w.Code, http.StatusBadRequest)
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["error"] != "lock_id parameter required" {
		t.Errorf("got error %q, want %q", resp["error"], "lock_id parameter required")
	}
}

func TestHTTPStatusForAcquireError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
	}{
		{"quota", status.Error(codes.ResourceExhausted, "Daily job limit reached: 10 of 10 jobs used today"), http.StatusTooManyRequests, "Daily job limit reached: 10 of 10 jobs used today"},
		{"bad request", status.Error(codes.InvalidArgument, "count and serials are mutually exclusive"), http.StatusBadRequest, "count and serials are mutually exclusive"},
		{"forbidden", status.Error(codes.PermissionDenied, "nope"), http.StatusForbidden, "nope"},
		{"timeout", status.Error(codes.DeadlineExceeded, "deadline"), http.StatusGatewayTimeout, "deadline"},
		{"wifi unmanaged", status.Error(codes.FailedPrecondition, "WIFI_SSID is not set"), http.StatusNotImplemented, "WIFI_SSID is not set"},
		{"old device source", status.Error(codes.Unimplemented, "unknown method SetWifi"), http.StatusNotImplemented, "unknown method SetWifi"},
		{"relayed device", status.Error(codes.NotFound, "device R is not attached to this proxy"), http.StatusNotFound, "device R is not attached to this proxy"},
		{"other grpc", status.Error(codes.Unavailable, "lock manager not configured"), http.StatusInternalServerError, "lock manager not configured"},
		{"plain error", errors.New("boom"), http.StatusInternalServerError, "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, msg := httpStatusForAcquireError(tt.err)
			if code != tt.wantCode || msg != tt.wantMsg {
				t.Errorf("got (%d, %q), want (%d, %q)", code, msg, tt.wantCode, tt.wantMsg)
			}
		})
	}
}
