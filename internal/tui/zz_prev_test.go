package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/JohnStarlight/gitea2github/internal/relink"
)

func frame(label string, s string) {
	fmt.Printf("\n\033[1m%s\033[0m\n", label)
	for _, l := range strings.Split(s, "\r\n") {
		if strings.TrimSpace(stripANSI(l)) != "" {
			fmt.Println(l)
		}
	}
}

func TestPrevFlow(t *testing.T) {
	// --- Βήμα 2: η οθόνη επιλογής της μετακίνησης ---------------------------
	rows := []Row{
		{Name: "ivogiake/ascii-art-web", SourcePrivate: true, Private: true, Target: "ascii-art-web"},
		{Name: "ivogiake/lem-in", SourcePrivate: true, Private: true, Target: "lem-in"},
	}
	m := NewModel(rows, false, false, false, false, "")
	m.SetSize(94, 14)
	m.Update(Key{Kind: KeyRune, Rune: 'E'}) // redact everything
	frame("── ΒΗΜΑ 2 · η οθόνη της μετακίνησης, με redact ενεργό ──",
		m.View("gitea.zone01.gr  ->  github.com/ivogiake"))

	// --- Βήμα 4: η οθόνη επανασύνδεσης, ΟΠΩΣ ΘΑ ΓΙΝΟΤΑΝ --------------------
	clones := []Clone{
		{Path: "/a", Display: "~/Git/Zone01/ascii-art-web", Redacted: true},
		{Path: "/b", Display: "~/Git/Zone01/lem-in", Redacted: true},
		{Path: "/c", Display: "~/Git/Zone01/net-cat", Public: true},
	}
	r := NewRelinkModel(clones, relink.ModeGitHub, "gitea")
	r.SetSize(94, 16)
	frame("── ΒΗΜΑ 4 · η οθόνη επανασύνδεσης ──", r.View("github.com/ivogiake"))
}
