package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReviewOwnershipRequiresLOCheckoutAndExactBranch(t *testing.T) {
	base := t.TempDir()
	for _, adapter := range []string{"telegram", "lo"} {
		gitDir := filepath.Join(base, adapter, "chat_42", "repo", ".git")
		if err := os.MkdirAll(gitDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/duck/42/"+adapter+"-task\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		config := []byte("[remote \"origin\"]\n url = https://git.example/owner/repo.git\n")
		if err := os.WriteFile(filepath.Join(gitDir, "config"), config, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loBase := filepath.Join(base, "lo")
	if !ownsReview(loBase, "42", "owner/repo", "duck/42/lo-task") {
		t.Fatal("own LO checkout not recognized")
	}
	for _, candidate := range []struct{ chatID, repo, branch string }{
		{"42", "owner/repo", "duck/42/telegram-task"},
		{"42", "other/repo", "duck/42/lo-task"},
		{"43", "owner/repo", "duck/42/lo-task"},
	} {
		if ownsReview(loBase, candidate.chatID, candidate.repo, candidate.branch) {
			t.Fatalf("accepted unrelated checkout: %+v", candidate)
		}
	}
}
