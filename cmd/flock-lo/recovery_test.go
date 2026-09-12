package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/duckbugio/flock/core/pending"
)

type recoveryRecorder struct {
	markers []pending.Marker
	deletes int
}

func (r *recoveryRecorder) ResumePending(_ string, marker pending.Marker) {
	r.markers = append(r.markers, marker)
}

func (r *recoveryRecorder) Delete(_ context.Context, _, _ string) error {
	r.deletes++
	return errors.New("already deleted")
}

func TestResumePendingKeepsDurableFIFODespiteStaleAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	store, err := pending.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"first", "second"} {
		if _, err := store.Enqueue("42", pending.Marker{Prompt: prompt, AnchorMsgID: "12"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Enqueue("invalid", pending.Marker{Prompt: "retain invalid"}); err != nil {
		t.Fatal(err)
	}
	restarted, err := pending.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &recoveryRecorder{}
	resumePending(context.Background(), recorder, recorder, restarted, slog.New(slog.DiscardHandler))
	if len(recorder.markers) != 2 || recorder.markers[0].Prompt != "first" ||
		recorder.markers[1].Prompt != "second" || recorder.deletes != 2 {
		t.Fatalf("replay = %+v", recorder)
	}
	retained, err := pending.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(retained.All()["42"]) != 2 || len(retained.All()["invalid"]) != 1 {
		t.Fatal("replay removed durable markers before clean terminal")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resumePending(ctx, recorder, recorder, restarted, slog.New(slog.DiscardHandler))
	if len(recorder.markers) != 2 || recorder.deletes != 2 {
		t.Fatal("shutdown replayed more work")
	}
}
