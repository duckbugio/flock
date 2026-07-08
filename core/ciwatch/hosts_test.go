//nolint:testpackage // intentionally whitebox to test unexported ciwatch internals
package ciwatch

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// giteaServer serves the three Gitea endpoints the host client touches.
func giteaServer(t *testing.T, combinedState string, contexts []string) (*httptest.Server, *[]string) {
	t.Helper()
	var merges []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/svc/commits", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("sha"); got != "duck/100/x" {
			t.Errorf("headSHA branch = %q", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{{"sha": "abc123"}})
	})
	mux.HandleFunc("/repos/acme/svc/commits/abc123/status", func(w http.ResponseWriter, _ *http.Request) {
		statuses := make([]map[string]string, 0, len(contexts))
		for _, c := range contexts {
			statuses = append(statuses, map[string]string{"status": "failure", "context": c})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"state": combinedState, "statuses": statuses})
	})
	mux.HandleFunc("/repos/acme/svc/pulls", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"number": 3, "merged": false, "head": map[string]string{"ref": "duck/100/x"}},
			{"number": 4, "merged": false, "head": map[string]string{"ref": "other"}},
		})
	})
	mux.HandleFunc("/repos/acme/svc/pulls/3/merge", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		merges = append(merges, r.Method+" "+strings.TrimSpace(string(body)))
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(mux), &merges
}

func TestGiteaHost(t *testing.T) {
	srv, merges := giteaServer(t, "failure", []string{"build", "lint"})
	defer srv.Close()
	h := NewGitea(srv.URL, "tok", srv.Client())

	st, err := h.Status(context.Background(), "acme/svc", "duck/100/x")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != stateFailure || st.SHA != "abc123" || !strings.Contains(st.Detail, "build") {
		t.Fatalf("Status = %+v", st)
	}

	idx, ok, err := h.OpenPR(context.Background(), "acme/svc", "duck/100/x")
	if err != nil || !ok || idx != 3 {
		t.Fatalf("OpenPR = %d, %v, %v", idx, ok, err)
	}
	if _, ok, _ := h.OpenPR(context.Background(), "acme/svc", "duck/999/none"); ok {
		t.Fatal("OpenPR must miss an unknown branch")
	}

	if err := h.Merge(context.Background(), "acme/svc", 3); err != nil {
		t.Fatal(err)
	}
	if len(*merges) != 1 || !strings.Contains((*merges)[0], "POST") || !strings.Contains((*merges)[0], "merge") {
		t.Fatalf("merge calls = %v", *merges)
	}
}

func TestGiteaHostNoCI(t *testing.T) {
	srv, _ := giteaServer(t, "", nil)
	defer srv.Close()
	h := NewGitea(srv.URL, "tok", srv.Client())
	st, err := h.Status(context.Background(), "acme/svc", "duck/100/x")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != stateNone {
		t.Fatalf("no statuses must map to none, got %+v", st)
	}
}

// githubServer serves the GitHub endpoints the host client touches.
func githubServer(t *testing.T, runs []map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/svc/commits", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("sha"); got != "duck/100/x" {
			t.Errorf("headSHA branch = %q", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{{"sha": "abc123"}})
	})
	mux.HandleFunc("/repos/acme/svc/commits/abc123/check-runs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
	})
	mux.HandleFunc("/repos/acme/svc/commits/abc123/status", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "statuses": []map[string]string{
			{"state": "success", "context": "legacy-ci"},
		}})
	})
	mux.HandleFunc("/repos/acme/svc/pulls", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("head"); got != "acme:duck/100/x" {
			t.Errorf("OpenPR head = %q", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"number": 9}})
	})
	mux.HandleFunc("/repos/acme/svc/pulls/9/merge", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("merge method = %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(mux)
}

// githubHostFor builds the GitHub host pointed at the test server (the real
// constructor pins api.github.com).
func githubHostFor(srv *httptest.Server) Host {
	return &githubHost{api: api{base: srv.URL, token: "tok", scheme: "Bearer", client: srv.Client()}}
}

func TestGitHubHostFailureWins(t *testing.T) {
	srv := githubServer(t, []map[string]string{
		{"name": "build", "status": "completed", "conclusion": "success"},
		{"name": "test", "status": "completed", "conclusion": "failure"},
		{"name": "lint", "status": "in_progress", "conclusion": ""},
	})
	defer srv.Close()
	st, err := githubHostFor(srv).Status(context.Background(), "acme/svc", "duck/100/x")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != stateFailure || !strings.Contains(st.Detail, "test") {
		t.Fatalf("Status = %+v", st)
	}
}

func TestGitHubHostPendingAndSuccess(t *testing.T) {
	pending := githubServer(t, []map[string]string{
		{"name": "build", "status": "in_progress", "conclusion": ""},
	})
	defer pending.Close()
	st, err := githubHostFor(pending).Status(context.Background(), "acme/svc", "duck/100/x")
	if err != nil || st.State != statePending {
		t.Fatalf("Status = %+v, err %v", st, err)
	}

	success := githubServer(t, []map[string]string{
		{"name": "build", "status": "completed", "conclusion": "success"},
		{"name": "meta", "status": "completed", "conclusion": "skipped"},
	})
	defer success.Close()
	st, err = githubHostFor(success).Status(context.Background(), "acme/svc", "duck/100/x")
	if err != nil || st.State != stateSuccess {
		t.Fatalf("Status = %+v, err %v", st, err)
	}
}

func TestGitHubHostLegacyFallbackAndPR(t *testing.T) {
	srv := githubServer(t, nil) // zero check runs → legacy combined status (success)
	defer srv.Close()
	h := githubHostFor(srv)
	st, err := h.Status(context.Background(), "acme/svc", "duck/100/x")
	if err != nil || st.State != stateSuccess {
		t.Fatalf("Status = %+v, err %v", st, err)
	}
	idx, ok, err := h.OpenPR(context.Background(), "acme/svc", "duck/100/x")
	if err != nil || !ok || idx != 9 {
		t.Fatalf("OpenPR = %d, %v, %v", idx, ok, err)
	}
	if err := h.Merge(context.Background(), "acme/svc", 9); err != nil {
		t.Fatal(err)
	}
}
