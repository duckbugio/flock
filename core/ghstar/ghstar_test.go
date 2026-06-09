package ghstar_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/duckbugio/flock/core/ghstar"
)

const (
	testOwner = "duckbugio"
	testRepo  = "flock"
	testToken = "tok-123"
)

// assertHeaders checks the shared GitHub headers every request must carry.
func assertHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
		t.Errorf("Authorization = %q; want Bearer %s", got, testToken)
	}
	if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("Accept = %q; want application/vnd.github+json", got)
	}
	if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q; want 2022-11-28", got)
	}
}

func TestIsStarred(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		status  int
		want    bool
		wantErr bool
	}{
		{name: "starred", status: http.StatusNoContent, want: true},
		{name: "not starred", status: http.StatusNotFound, want: false},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %s; want GET", r.Method)
				}
				if r.URL.Path != "/user/starred/"+testOwner+"/"+testRepo {
					t.Errorf("path = %s", r.URL.Path)
				}
				assertHeaders(t, r)
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			c := ghstar.New(ghstar.Config{BaseURL: srv.URL, Token: testToken})
			got, err := c.IsStarred(context.Background(), testOwner, testRepo)
			if tc.wantErr {
				if err == nil {
					t.Fatal("IsStarred: want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("IsStarred: unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("IsStarred = %v; want %v", got, tc.want)
			}
		})
	}
}

func TestStarSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s; want PUT", r.Method)
		}
		assertHeaders(t, r)
		if got := r.Header.Get("Content-Length"); got != "0" {
			t.Errorf("Content-Length = %q; want 0", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := ghstar.New(ghstar.Config{BaseURL: srv.URL, Token: testToken})
	if err := c.Star(context.Background(), testOwner, testRepo); err != nil {
		t.Fatalf("Star: unexpected error: %v", err)
	}
}

func TestStarUnauthorized(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := ghstar.New(ghstar.Config{BaseURL: srv.URL, Token: testToken})
	err := c.Star(context.Background(), testOwner, testRepo)
	if !errors.Is(err, ghstar.ErrUnauthorized) {
		t.Fatalf("Star(403) error = %v; want ErrUnauthorized", err)
	}
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := ghstar.New(ghstar.Config{BaseURL: srv.URL, Token: testToken})
	if _, err := c.IsStarred(ctx, testOwner, testRepo); err == nil {
		t.Fatal("IsStarred with cancelled context: want error, got nil")
	}
}
