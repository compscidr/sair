package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
