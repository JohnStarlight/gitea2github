package ui

import (
	"strings"
	"testing"
)

// TestNonInteractiveNeverBlocks is the property the package exists for: with no
// terminal, every question returns its default immediately. If this breaks, the
// tool hangs in CI instead of failing.
func TestNonInteractiveNeverBlocks(t *testing.T) {
	// Empty input: anything that tried to read would hit EOF at once, and
	// anything that blocked would hang the test.
	p := NewWith(strings.NewReader(""), &strings.Builder{}, false)

	if got := p.Confirm("proceed?", true); got != true {
		t.Error("Confirm ignored its default without a terminal")
	}
	if got := p.Confirm("proceed?", false); got != false {
		t.Error("Confirm ignored its default without a terminal")
	}
	if got := p.Line("address?", "me@example.com"); got != "me@example.com" {
		t.Errorf("Line = %q, want the default", got)
	}
	opts := []Option{{Label: "a"}, {Label: "b"}, {Label: "c"}}
	if got := p.Choose("which?", opts, 2); got != 2 {
		t.Errorf("Choose = %d, want the default 2", got)
	}
}

func TestConfirmAnswers(t *testing.T) {
	cases := []struct {
		input string
		def   bool
		want  bool
	}{
		{"y\n", false, true},
		{"yes\n", false, true},
		{"Y\n", false, true},
		{"n\n", true, false},
		{"no\n", true, false},
		{"\n", true, true},   // bare enter takes the default
		{"\n", false, false}, // ...in both directions
		// Nonsense is re-asked rather than guessed at; the second answer counts.
		{"maybe\ny\n", false, true},
	}
	for _, c := range cases {
		p := NewWith(strings.NewReader(c.input), &strings.Builder{}, true)
		if got := p.Confirm("proceed?", c.def); got != c.want {
			t.Errorf("Confirm(%q, def=%v) = %v, want %v", c.input, c.def, got, c.want)
		}
	}
}

// TestConfirmTreatsEOFAsDefault covers input ending mid-question, such as a
// closed pipe. Silence is not consent, so it must not be read as yes.
func TestConfirmTreatsEOFAsDefault(t *testing.T) {
	p := NewWith(strings.NewReader(""), &strings.Builder{}, true)
	if p.Confirm("destroy everything?", false) {
		t.Error("EOF was treated as confirmation")
	}
}

func TestChooseValidatesRange(t *testing.T) {
	opts := []Option{{Label: "github"}, {Label: "both"}, {Label: "gitea"}}
	cases := []struct {
		input string
		want  int
	}{
		{"2\n", 1},
		{"\n", 0},     // default
		{"9\n1\n", 0}, // out of range, then valid
		{"x\n3\n", 2}, // not a number, then valid
	}
	for _, c := range cases {
		var out strings.Builder
		p := NewWith(strings.NewReader(c.input), &out, true)
		if got := p.Choose("where?", opts, 0); got != c.want {
			t.Errorf("Choose(%q) = %d, want %d", c.input, got, c.want)
		}
	}
}

func TestLineFallsBackToDefault(t *testing.T) {
	p := NewWith(strings.NewReader("\n"), &strings.Builder{}, true)
	if got := p.Line("address?", "me@example.com"); got != "me@example.com" {
		t.Errorf("Line = %q, want the default on a bare enter", got)
	}
	p = NewWith(strings.NewReader("you@example.com\n"), &strings.Builder{}, true)
	if got := p.Line("address?", "me@example.com"); got != "you@example.com" {
		t.Errorf("Line = %q, want the typed answer", got)
	}
}

func TestParseSelection(t *testing.T) {
	cases := []struct {
		input   string
		count   int
		want    []int
		wantErr bool
	}{
		{"1 3", 5, []int{0, 2}, false},
		{"1,3", 5, []int{0, 2}, false},   // commas are accepted too
		{"  2  ", 5, []int{1}, false},    // stray whitespace
		{"", 5, nil, false},              // nothing selected
		{"3 1 3", 5, []int{2, 0}, false}, // a repeat is a typo, not two actions
		{"0", 5, nil, true},              // the list is 1-based
		{"6", 5, nil, true},              // past the end
		{"-1", 5, nil, true},
		{"x", 5, nil, true},
		// One bad entry rejects the whole line: acting on part of a selection
		// means acting on a set the user never chose.
		{"1 9", 5, nil, true},
	}
	for _, c := range cases {
		got, err := ParseSelection(c.input, c.count)
		if (err != nil) != c.wantErr {
			t.Errorf("ParseSelection(%q): err = %v, wantErr %v", c.input, err, c.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("ParseSelection(%q) = %v, want %v", c.input, got, c.want)
			continue
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Errorf("ParseSelection(%q) = %v, want %v", c.input, got, c.want)
				break
			}
		}
	}
}

func TestSelectRetriesOnBadInput(t *testing.T) {
	var out strings.Builder
	p := NewWith(strings.NewReader("9\n2\n"), &out, true)
	got := p.Select("which?", 3)
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("Select = %v, want [1] after the out-of-range answer was rejected", got)
	}
	if !strings.Contains(out.String(), "not between 1 and 3") {
		t.Errorf("no explanation shown for the rejected answer:\n%s", out.String())
	}
}

func TestSelectEmptyAnswerChangesNothing(t *testing.T) {
	p := NewWith(strings.NewReader("\n"), &strings.Builder{}, true)
	if got := p.Select("which?", 3); got != nil {
		t.Errorf("Select = %v, want nil for a bare enter", got)
	}
}
