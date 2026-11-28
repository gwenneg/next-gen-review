package review

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A git diff as GitHub sends it: two hunks, a new file, a deleted file, a rename and a binary file.
const sampleDiff = `diff --git a/main.go b/main.go
index 1111111..2222222 100644
--- a/main.go
+++ b/main.go
@@ -3,3 +3,4 @@ package main
 import "fmt"
-var a = 1
+var a = 2
+var b = 3
 func main() {
@@ -20,3 +21,3 @@ func f() {
 x := 1
-y := 2
+y := 3
 return
diff --git a/new.txt b/new.txt
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/new.txt
@@ -0,0 +1,2 @@
+one
+two
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index 4444444..0000000
--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-bye
diff --git a/old/name.go b/new/name.go
similarity index 90%
rename from old/name.go
rename to new/name.go
index 5555555..6666666 100644
--- a/old/name.go
+++ b/new/name.go
@@ -1,2 +1,2 @@
-package old
+package name
 // x
diff --git a/logo.png b/logo.png
index 7777777..8888888 100644
Binary files a/logo.png and b/logo.png differ
`

func TestParseDiffLines(t *testing.T) {
	got, err := ParseDiffLines(sampleDiff)
	if err != nil {
		t.Fatal(err)
	}
	want := DiffLines{
		"main.go":     {SideRight: {{3, 6}, {21, 23}}, SideLeft: {{3, 5}, {20, 22}}},
		"new.txt":     {SideRight: {{1, 2}}},
		"gone.txt":    {SideLeft: {{1, 1}}},
		"new/name.go": {SideRight: {{1, 2}}, SideLeft: {{1, 2}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseDiffLines() = %v\nwant %v", got, want)
	}
	if lines, err := ParseDiffLines(""); err != nil || len(lines) != 0 {
		t.Errorf("ParseDiffLines(empty) = %v, %v", lines, err)
	}
	// A hunk with the wrong line count, as in a diff cut short.
	if _, err := ParseDiffLines(strings.Replace(sampleDiff, "@@ -3,3 +3,4 @@", "@@ -3,9 +3,9 @@", 1)); err == nil {
		t.Error("ParseDiffLines() of a broken diff = nil, want an error")
	}
}

// GitHub takes an inline comment only on lines of one hunk of its file, on its side.
func TestAccepts(t *testing.T) {
	lines, _ := ParseDiffLines(sampleDiff)
	tests := []struct {
		c    Comment
		want bool
	}{
		{Comment{Path: "main.go", Line: 5, Side: SideRight}, true},
		{Comment{Path: "main.go", Line: 3, Side: SideRight}, true}, // a hunk's first line
		{Comment{Path: "main.go", Line: 6, Side: SideRight}, true}, // and its last
		{Comment{Path: "main.go", Line: 7, Side: SideRight}, false},
		{Comment{Path: "main.go", Line: 15, Side: SideRight}, false}, // between the hunks
		{Comment{Path: "main.go", StartLine: 4, Line: 6, Side: SideRight}, true},
		{Comment{Path: "main.go", StartLine: 6, Line: 22, Side: SideRight}, false}, // across hunks
		{Comment{Path: "main.go", Line: 22, Side: SideLeft}, true},
		{Comment{Path: "main.go", Line: 23, Side: SideLeft}, false},
		{Comment{Path: "gone.txt", Line: 1, Side: SideRight}, false}, // a deleted file has only old lines
		{Comment{Path: "gone.txt", Line: 1, Side: SideLeft}, true},
		{Comment{Path: "old/name.go", Line: 1, Side: SideRight}, false}, // a renamed file goes by its new name
		{Comment{Path: "logo.png", Line: 1, Side: SideRight}, false},
		{Comment{Path: "other.go", Line: 1, Side: SideRight}, false},
	}
	for _, tt := range tests {
		if got := lines.Accepts(tt.c); got != tt.want {
			t.Errorf("Accepts(%+v) = %v, want %v", tt.c, got, tt.want)
		}
	}
}

func TestBodies(t *testing.T) {
	if got := CommentBody("  [High] Fix it.\n"); got != "[High] Fix it.\n\n"+aiLabel {
		t.Errorf("CommentBody() = %q", got)
	}
	offDiff := []Comment{
		{Path: "a.go", Line: 9, Body: "[Low] Rename."},
		{Path: "b.go", StartLine: 2, Line: 4, Body: "Unused."},
	}
	want := "Please add a test.\n\n**Outside the diff**\n\n- `a.go:9`: [Low] Rename.\n- `b.go:2-4`: Unused.\n\n" + aiLabel
	if got := MainBody(" Please add a test. ", offDiff); got != want {
		t.Errorf("MainBody() = %q\nwant %q", got, want)
	}
	// A comment of several lines stays in its list item: GitHub renders unindented lines outside it.
	multi := []Comment{{Path: "a.go", Line: 9, Body: "First.\n\nSecond:\n```go\nx := 1\n```"}}
	if got := MainBody("", multi); !strings.Contains(got, "- `a.go:9`: First.\n  \n  Second:\n  ```go\n  x := 1\n  ```\n") {
		t.Errorf("MainBody(multi-line) = %q", got)
	}
	// An empty main comment still says the review was drafted with AI.
	if got := MainBody("", nil); got != aiLabel {
		t.Errorf("MainBody(empty) = %q", got)
	}
}

// claude's answer becomes the review text and drafts that start with their severity.
func TestOutputDrafts(t *testing.T) {
	var out Output
	answer := `{"review":"## Findings","summary":"Fix the retry.","comments":[
		{"path":"a.go","line":4,"start_line":2,"side":"RIGHT","severity":"High","body":" Retries forever. "},
		{"path":"b.go","line":1,"side":"LEFT","severity":"Low","body":"Keep this."}]}`
	if err := json.Unmarshal([]byte(answer), &out); err != nil {
		t.Fatal(err)
	}
	want := Drafts{Main: "Fix the retry.", Comments: []Comment{
		{Path: "a.go", Line: 4, StartLine: 2, Side: SideRight, Body: "[High] Retries forever."},
		{Path: "b.go", Line: 1, Side: SideLeft, Body: "[Low] Keep this."},
	}}
	if got := out.Drafts(); !reflect.DeepEqual(got, want) || out.Review != "## Findings" {
		t.Errorf("Drafts() = %+v, review %q\nwant %+v", got, out.Review, want)
	}
	// The schema asks for what Output reads.
	var schema struct {
		Required   []string `json:"required"`
		Properties struct {
			Review struct {
				MinLength int `json:"minLength"`
			} `json:"review"`
			Comments struct {
				Items struct {
					Required []string `json:"required"`
				} `json:"items"`
			} `json:"comments"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(OutputSchema), &schema); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(schema.Required, []string{"review", "summary", "comments"}) || schema.Properties.Review.MinLength != 1 ||
		!strings.Contains(strings.Join(schema.Properties.Comments.Items.Required, ","), "severity") {
		t.Errorf("schema = %s", OutputSchema)
	}
}
