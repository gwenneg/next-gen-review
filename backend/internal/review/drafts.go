package review

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

// Sides of a diff an inline comment can be on, as GitHub names them.
const (
	SideRight = "RIGHT" // the new code: added or unchanged lines
	SideLeft  = "LEFT"  // the old code: deleted or unchanged lines
)

// Events of a GitHub review: the verdict the maintainer picks when posting.
var Events = []string{"COMMENT", "APPROVE", "REQUEST_CHANGES"}

// Comment is an inline comment for the GitHub review, drafted by the AI or added by the maintainer.
type Comment struct {
	ID        int64  `json:"id"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine int    `json:"startLine,omitempty"` // the first line of a range
	Side      string `json:"side"`
	Body      string `json:"body"`
	InDiff    bool   `json:"inDiff"` // GitHub accepts it inline; otherwise it's posted in the main comment
}

// Posting is a review's comments for GitHub, and whether they were posted. Posted ones can't change.
type Posting struct {
	Main        string    `json:"main"`
	Comments    []Comment `json:"comments"`
	Posted      bool      `json:"posted"`    // posted to GitHub, even if its link wasn't recorded
	PostedURL   string    `json:"postedUrl"` // the GitHub review
	PostedEvent string    `json:"postedEvent"`
	Lines       DiffLines `json:"-"`
}

// Drafts are what the AI proposes to post: the review's main comment and inline comments.
type Drafts struct {
	Main     string    `json:"main"`
	Comments []Comment `json:"comments"`
}

// DiffLines are the lines of each file GitHub accepts inline comments on: the hunks of the PR's
// diff, by path and side, as ranges of line numbers.
type DiffLines map[string]map[string][][2]int

// ParseDiffLines reads the hunks of a git diff.
func ParseDiffLines(diff string) (DiffLines, error) {
	files, _, err := gitdiff.Parse(strings.NewReader(diff))
	if err != nil {
		return nil, err
	}
	lines := DiffLines{}
	for _, f := range files {
		path := f.NewName
		if f.IsDelete {
			path = f.OldName
		}
		for _, h := range f.TextFragments {
			if h.NewLines > 0 {
				lines.add(path, SideRight, int(h.NewPosition), int(h.NewPosition+h.NewLines-1))
			}
			if h.OldLines > 0 {
				lines.add(path, SideLeft, int(h.OldPosition), int(h.OldPosition+h.OldLines-1))
			}
		}
	}
	return lines, nil
}

func (d DiffLines) add(path, side string, first, last int) {
	if d[path] == nil {
		d[path] = map[string][][2]int{}
	}
	d[path][side] = append(d[path][side], [2]int{first, last})
}

// Accepts says whether GitHub accepts the comment inline: its lines are in one hunk of its file.
func (d DiffLines) Accepts(c Comment) bool {
	first := c.Line
	if c.StartLine != 0 {
		first = c.StartLine
	}
	for _, r := range d[c.Path][c.Side] {
		if r[0] <= first && first <= c.Line && c.Line <= r[1] {
			return true
		}
	}
	return false
}

// aiLabel ends every comment the tool posts: comments drafted with AI say so, even read alone.
const aiLabel = "<sub>Drafted with AI (Claude), reviewed and posted by the maintainer.</sub>"

// MaxBody is the longest comment GitHub accepts, in characters.
const MaxBody = 65536

// MaxComment is the longest comment text the maintainer can write: the AI label is added to it.
var MaxComment = MaxBody - utf8.RuneCountInString("\n\n"+aiLabel)

// CommentBody is an inline comment as posted.
func CommentBody(body string) string {
	return strings.TrimSpace(body) + "\n\n" + aiLabel
}

// MainBody is the review's main comment as posted. The comments GitHub doesn't accept inline go in
// it, under their file and line.
func MainBody(main string, offDiff []Comment) string {
	var b strings.Builder
	if s := strings.TrimSpace(main); s != "" {
		b.WriteString(s + "\n\n")
	}
	if len(offDiff) > 0 {
		b.WriteString("**Outside the diff**\n\n")
		for _, c := range offDiff {
			// Later lines are indented, so paragraphs and code blocks stay in the list item.
			body := strings.ReplaceAll(strings.TrimSpace(c.Body), "\n", "\n  ")
			fmt.Fprintf(&b, "- `%s:%s`: %s\n", c.Path, lineText(c), body)
		}
		b.WriteString("\n")
	}
	b.WriteString(aiLabel)
	return b.String()
}

func lineText(c Comment) string {
	if c.StartLine != 0 && c.StartLine != c.Line {
		return fmt.Sprintf("%d-%d", c.StartLine, c.Line)
	}
	return fmt.Sprint(c.Line)
}

// Output is what claude returns for a review, checked against OutputSchema.
type Output struct {
	Review   string `json:"review"`  // the Markdown review, for the maintainer
	Main     string `json:"summary"` // the draft main comment, for the PR author
	Comments []struct {
		Path      string `json:"path"`
		Line      int    `json:"line"`
		StartLine int    `json:"start_line"`
		Side      string `json:"side"`
		Severity  string `json:"severity"`
		Body      string `json:"body"`
	} `json:"comments"`
}

// Drafts turns claude's comments into drafts, each starting with its severity.
func (o Output) Drafts() Drafts {
	d := Drafts{Main: o.Main, Comments: []Comment{}}
	for _, c := range o.Comments {
		body := strings.TrimSpace(c.Body)
		if c.Severity != "" {
			body = "[" + c.Severity + "] " + body
		}
		d.Comments = append(d.Comments, Comment{Path: c.Path, Line: c.Line, StartLine: c.StartLine, Side: c.Side, Body: body})
	}
	return d
}

// OutputSchema is the JSON schema claude's answer must match (claude --json-schema).
var OutputSchema = func() string {
	str := map[string]any{"type": "string"}
	schema := map[string]any{
		"type":     "object",
		"required": []string{"review", "summary", "comments"},
		"properties": map[string]any{
			"review":  map[string]any{"type": "string", "minLength": 1},
			"summary": str,
			"comments": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":     "object",
					"required": []string{"path", "line", "side", "severity", "body"},
					"properties": map[string]any{
						"path":       str,
						"line":       map[string]any{"type": "integer", "minimum": 1},
						"start_line": map[string]any{"type": "integer", "minimum": 1},
						"side":       map[string]any{"enum": []string{SideRight, SideLeft}},
						"severity":   map[string]any{"enum": []string{"Critical", "High", "Medium", "Low"}},
						"body":       str,
					},
				},
			},
		},
	}
	b, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	return string(b)
}()
