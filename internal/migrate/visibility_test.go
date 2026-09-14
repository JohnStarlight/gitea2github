package migrate

import (
	"testing"

	"github.com/JohnStarlight/gitea2github/internal/gitea"
)

// TestDestinationIsPrivate pins the visibility rule. The default has to be
// private, and "allow public" must never widen a repository that was private on
// the source side.
func TestDestinationIsPrivate(t *testing.T) {
	cases := []struct {
		name          string
		sourcePrivate bool
		allowPublic   bool
		wantPrivate   bool
	}{
		// The zero value of Options must publish nothing.
		{"default keeps a public source private", false, false, true},
		{"default keeps a private source private", true, false, true},
		// Opting in carries the source visibility across, no more.
		{"allow public publishes a public source", false, true, false},
		{"allow public still protects a private source", true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := gitea.Repo{Private: c.sourcePrivate}
			if got := destinationIsPrivate(source, c.allowPublic); got != c.wantPrivate {
				t.Errorf("destinationIsPrivate(private=%v, allowPublic=%v) = %v, want %v",
					c.sourcePrivate, c.allowPublic, got, c.wantPrivate)
			}
		})
	}
}
