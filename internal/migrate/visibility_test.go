package migrate

import (
	"testing"

	"github.com/JohnStarlight/gitea2github/internal/gitea"
)

// TestDestinationIsPrivate pins the visibility rules, including which one wins.
//
// The default has to mirror the source, because that is the only setting that
// cannot change how exposed a repository is; everything else is something the
// user asked for explicitly.
func TestDestinationIsPrivate(t *testing.T) {
	cases := []struct {
		name          string
		sourcePrivate bool
		mode          VisibilityMode
		override      map[string]bool
		wantPrivate   bool
	}{
		{"mirror keeps a private source private", true, VisibilityMirror, nil, true},
		{"mirror keeps a public source public", false, VisibilityMirror, nil, false},

		{"private mode overrides a public source", false, VisibilityPrivate, nil, true},
		{"public mode overrides a private source", true, VisibilityPublic, nil, false},

		// A per-repository choice is the most specific instruction given, so it
		// beats the blanket rule in both directions.
		{"override beats private mode", false, VisibilityPrivate,
			map[string]bool{"owner/repo": false}, false},
		{"override beats public mode", true, VisibilityPublic,
			map[string]bool{"owner/repo": true}, true},
		{"override beats mirror", false, VisibilityMirror,
			map[string]bool{"owner/repo": true}, true},

		// An override naming a different repository must not leak across.
		{"override for another repo is ignored", false, VisibilityMirror,
			map[string]bool{"owner/other": true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := gitea.Repo{FullName: "owner/repo", Private: c.sourcePrivate}
			if got := destinationIsPrivate(source, c.mode, c.override); got != c.wantPrivate {
				t.Errorf("destinationIsPrivate(private=%v, mode=%q) = %v, want %v",
					c.sourcePrivate, c.mode, got, c.wantPrivate)
			}
		})
	}
}
