package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gwenneg/next-gen-review/backend/internal/github"
	"github.com/gwenneg/next-gen-review/backend/internal/review"
)

// A review's comments for GitHub: drafted by the AI, edited while the review is ended and not
// posted, then fixed once posted.
func TestDrafts(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	if err := s.ReplaceQueue(ctx, []github.PullRequest{pr("PR_1", 0), pr("PR_2", 0)}); err != nil {
		t.Fatal(err)
	}
	r, _ := s.CreateReview(ctx, request)
	other := request
	other.PRID = "PR_2"
	active, _ := s.CreateReview(ctx, other)
	if _, _, err := s.ClaimReview(ctx); err != nil {
		t.Fatal(err)
	}
	lines := review.DiffLines{"a.go": {review.SideRight: {{1, 10}}}}
	if _, err := s.StartReview(ctx, r.ID, "prompt", lines); err != nil {
		t.Fatal(err)
	}
	drafts := review.Drafts{Main: "Please fix the retry.", Comments: []review.Comment{
		{Path: "b.go", Line: 3, Side: review.SideRight, Body: "[Low] Not in the diff."},
		{Path: "a.go", StartLine: 2, Line: 4, Side: review.SideRight, Body: "[High] Retries\x00 forever."},
	}}
	if err := s.FinishReview(ctx, r.ID, review.Outcome{Status: review.StatusDone, Result: "## Findings", Drafts: &drafts}); err != nil {
		t.Fatal(err)
	}

	p, err := s.Posting(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []review.Comment{
		{ID: 2, Path: "a.go", StartLine: 2, Line: 4, Side: review.SideRight, Body: "[High] Retries forever.", InDiff: true},
		{ID: 1, Path: "b.go", Line: 3, Side: review.SideRight, Body: "[Low] Not in the diff."},
	}
	if p.Main != drafts.Main || !reflect.DeepEqual(p.Comments, want) || p.Posted || p.PostedURL != "" || !reflect.DeepEqual(p.Lines, lines) {
		t.Fatalf("Posting() = %+v\nwant main %q and %+v", p, drafts.Main, want)
	}

	if err := s.SetMainComment(ctx, r.ID, "Fix the retry, please."); err != nil {
		t.Fatal(err)
	}
	if err := s.SetComment(ctx, r.ID, 1, "[Low] Rename this."); err != nil {
		t.Fatal(err)
	}
	added, err := s.AddComment(ctx, r.ID, review.Comment{Path: "a.go", Line: 9, Side: review.SideRight, Body: "Mine."})
	if err != nil || added.ID != 3 {
		t.Fatalf("AddComment() = %+v, %v", added, err)
	}
	if err := s.DeleteComment(ctx, r.ID, 2); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Posting(ctx, r.ID)
	if p.Main != "Fix the retry, please." || len(p.Comments) != 2 || p.Comments[0].ID != 3 || !p.Comments[0].InDiff ||
		p.Comments[1].Body != "[Low] Rename this." {
		t.Errorf("after the edits: %+v", p)
	}
	for name, err := range map[string]error{
		"SetComment(unknown)":    s.SetComment(ctx, r.ID, 99, "x"),
		"DeleteComment(unknown)": s.DeleteComment(ctx, r.ID, 99),
	} {
		if !errors.Is(err, review.ErrUnknownComment) {
			t.Errorf("%s = %v, want ErrUnknownComment", name, err)
		}
	}
	if err := s.SetMainComment(ctx, 999, "x"); !errors.Is(err, review.ErrUnknownReview) {
		t.Errorf("SetMainComment(unknown review) = %v", err)
	}
	if _, err := s.Posting(ctx, 999); !errors.Is(err, review.ErrUnknownReview) {
		t.Errorf("Posting(unknown review) = %v", err)
	}
	// A queued review has no comments to edit yet.
	if err := s.SetMainComment(ctx, active.ID, "x"); !errors.Is(err, review.ErrActive) {
		t.Errorf("SetMainComment(active) = %v, want ErrActive", err)
	}

	if err := s.MarkPosted(ctx, r.ID, "https://github.com/o/r/pull/7#pullrequestreview-1", "APPROVE"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Posting(ctx, r.ID); !p.Posted || p.PostedURL == "" || p.PostedEvent != "APPROVE" {
		t.Errorf("Posting() after posting = %+v", p)
	}
	if list, _ := s.Reviews(ctx, false); list[len(list)-1].PostedURL == "" {
		t.Error("Reviews() doesn't show the review was posted")
	}
	for name, err := range map[string]error{
		"SetMainComment": s.SetMainComment(ctx, r.ID, "x"),
		"AddComment":     func() error { _, err := s.AddComment(ctx, r.ID, added); return err }(),
		"SetComment":     s.SetComment(ctx, r.ID, 1, "x"),
		"DeleteComment":  s.DeleteComment(ctx, r.ID, 1),
		"MarkPosted":     s.MarkPosted(ctx, r.ID, "again", "COMMENT"),
	} {
		if !errors.Is(err, review.ErrPosted) {
			t.Errorf("%s after posting = %v, want ErrPosted", name, err)
		}
	}
}
