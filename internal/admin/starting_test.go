package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStartingServesStateAs503ElsewhereThenForwards(t *testing.T) {
	s := NewStarting()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "127.0.0.1:7657"
	s.ServeHTTP(rec, req)
	var st struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || rec.Code != 200 || st.Status != StatusStarting {
		t.Fatalf("/api/state = %d %q (%v), want 200 starting", rec.Code, rec.Body.String(), err)
	}

	for _, path := range []string{"/api/accounts", "/api/requests", "/"} {
		rec = httptest.NewRecorder()
		req = httptest.NewRequest("GET", path, nil)
		req.Host = "localhost:7657"
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s while starting = %d, want 503 + Retry-After", path, rec.Code)
		}
	}

	// DNS-rebinding guard applies here too.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "evil.example.com"
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-loopback Host while starting = %d, want 403", rec.Code)
	}

	s.Ready(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/accounts", nil)
	req.Host = "127.0.0.1"
	s.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Errorf("after Ready got %d, want the real handler's 204", rec.Code)
	}
}
