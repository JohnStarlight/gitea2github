package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JohnStarlight/gitea2github/internal/gitea"
	"github.com/JohnStarlight/gitea2github/internal/redact"
)

// withFiles adds commits to the scene's Gitea repository, one per map, in
// order: each file written, then committed.
func withFiles(t *testing.T, s resumeScene, commits ...map[string]string) {
	t.Helper()
	work := filepath.Join(filepath.Dir(s.gitea), "editor")
	s.git(filepath.Dir(s.gitea), "clone", "-q", "-b", "main", s.gitea, work)
	for i, files := range commits {
		for name, body := range files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(work, name)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		s.git(work, "add", ".")
		s.git(work, "commit", "-qm", "change "+string(rune('a'+i)))
	}
	s.git(work, "push", "-q", "origin", "main")
}

const lfsPointerFile = "version https://git-lfs.github.com/spec/v1\n" +
	"oid sha256:4d7a214614ab2935c943f9e0ff69d22eadbb8f32b1258daaa5e2ca24d17e2393\nsize 12345\n"

// TestScanFindsAddressesInEveryVersion: an address deleted from a file is
// still in the commit that had it, and a migration publishes that commit.
func TestScanFindsAddressesInEveryVersion(t *testing.T) {
	s := newResumeScene(t)
	withFiles(t, s,
		map[string]string{
			"package.json": `{"author": "Maria <maria@mail.example.gr>"}`,
			"README.md": "Contact: kostas@uni.example.gr\nClone: git@github.com:me/demo.git\n" +
				"Example: someone@example.com\n![logo](logo@2x.png)\n",
		},
		map[string]string{"README.md": "No addresses here any more.\n"},
	)
	scan, err := scanContents(context.Background(), s.gitea, true)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range scan.addresses {
		got = append(got, a.Address+" "+a.Where())
	}
	want := []string{
		"kostas@uni.example.gr in older versions of README.md",
		"maria@mail.example.gr in package.json",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("found:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestScanFindsLFSPointers: the files a push leaves behind as pointers.
func TestScanFindsLFSPointers(t *testing.T) {
	s := newResumeScene(t)
	withFiles(t, s, map[string]string{"assets/video.mp4": lfsPointerFile, "notes.txt": "plain\n"})
	scan, err := scanContents(context.Background(), s.gitea, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(scan.lfsFiles, ",") != "assets/video.mp4" {
		t.Errorf("LFS files = %v", scan.lfsFiles)
	}
	if len(scan.addresses) != 0 {
		t.Errorf("looked for addresses although not asked: %v", scan.addresses)
	}
}

// migrateWithAddress runs a migration of a repository whose files contain an
// address, and returns the result and the GitHub calls it made.
func migrateWithAddress(t *testing.T, private, redacting, allow bool) (Result, []string) {
	s := newResumeScene(t)
	withFiles(t, s, map[string]string{"package.json": `{"author": "maria@mail.example.gr"}`})
	f := &fakeGitHub{exists: false}
	f.scene = s
	opts := Options{
		GiteaUser: "me", GitHubUser: "me", GitHubTok: "t0ken-for-tests", Concurrency: 1,
		AllowEmailsInFiles: allow, GitHub: newTestClient(f),
	}
	if redacting {
		opts.Mapper = redact.NewMapper(nil, "")
	}
	res := Run(context.Background(), []gitea.Repo{s.repo(private)}, opts)[0]
	return res, f.events
}

// TestPublicRepositoryWithAddressesIsHeldBack is the case that cannot be
// undone: redaction was asked for so that addresses would not be published,
// and the files would publish them anyway. Nothing is created.
func TestPublicRepositoryWithAddressesIsHeldBack(t *testing.T) {
	res, calls := migrateWithAddress(t, false, true, false)
	if res.Status != StatusHeld || len(res.FileAddresses) != 1 {
		t.Fatalf("want held with the address, got %s (%s) %v", res.Status, res.Reason, res.FileAddresses)
	}
	if len(calls) != 0 {
		t.Errorf("a held repository was created on GitHub: %v", calls)
	}
}

// The other three: private goes ahead and says so; asked explicitly, public
// goes ahead; and without redaction nobody asked for addresses to be hidden.
func TestAddressesInFilesOtherwiseGoAhead(t *testing.T) {
	for _, c := range []struct {
		name                      string
		private, redacting, allow bool
		wantAddresses             int
	}{
		{"private", true, true, false, 1},
		{"allowed", false, true, true, 1},
		{"not redacting", false, false, false, 0},
	} {
		res, _ := migrateWithAddress(t, c.private, c.redacting, c.allow)
		if res.Status != StatusMigrated {
			t.Errorf("%s: want migrated, got %s (%s)", c.name, res.Status, res.Reason)
		}
		if len(res.FileAddresses) != c.wantAddresses {
			t.Errorf("%s: %d addresses reported, want %d", c.name, len(res.FileAddresses), c.wantAddresses)
		}
	}
}
