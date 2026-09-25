package migrate

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/JohnStarlight/gitea2github/internal/redact"
)

// FileAddress is an email address found inside a repository's files.
//
// Redaction rewrites who made each commit and what its message says. It does
// not touch what the files contain -- rewriting file contents is how files
// get corrupted -- so an address written into a package.json or a README is
// published with the repository however carefully the history was redacted.
type FileAddress struct {
	Address string
	Current []string // files that have it as they stand on some branch now
	Older   []string // files that had it in an earlier version only
}

// Where says where the address is, in plain words.
func (a FileAddress) Where() string {
	var parts []string
	if len(a.Current) > 0 {
		parts = append(parts, "in "+strings.Join(a.Current, ", "))
	}
	if len(a.Older) > 0 {
		parts = append(parts, "in older versions of "+strings.Join(a.Older, ", "))
	}
	return strings.Join(parts, "; ")
}

// contentScan is what a look through every version of every file found.
type contentScan struct {
	addresses []FileAddress
	lfsFiles  []string // files stored with Git LFS: pointers here, not content
}

const (
	// maxScannedBlob bounds what is read looking for addresses. Addresses
	// live in text -- manifests, READMEs, licences -- and a file past this
	// size is data, not something anybody typed an address into.
	maxScannedBlob = 1 << 20

	// lfsPointerMax is larger than any Git LFS pointer, which is three short
	// lines of text.
	lfsPointerMax = 1024
	lfsPointer    = "version https://git-lfs.github.com/spec/v1"
)

// scanContents looks through every version of every file in a repository,
// for Git LFS pointers always and for email addresses when asked.
//
// Every version, not only the latest: a migration publishes the whole
// history, and an address deleted from a file a year ago is still in the
// commit that had it.
func scanContents(ctx context.Context, repo string, addresses bool) (contentScan, error) {
	var scan contentScan

	// Every object reachable from any ref, with the path it was first seen
	// at. Commits have no path and are left out.
	listed, err := runGit(ctx, repo, "", "rev-list", "--all", "--objects")
	if err != nil {
		return scan, fmt.Errorf("listing files: %v: %s", err, listed)
	}
	pathOf := map[string]string{}
	var shas []string
	for _, line := range strings.Split(listed, "\n") {
		sha, path, ok := strings.Cut(line, " ")
		if !ok || path == "" {
			continue
		}
		if _, dup := pathOf[sha]; !dup {
			pathOf[sha] = path
			shas = append(shas, sha)
		}
	}
	if len(shas) == 0 {
		return scan, nil
	}

	// Which of those are file contents, and how large -- so that only what
	// is worth reading is read.
	limit := int64(lfsPointerMax)
	if addresses {
		limit = maxScannedBlob
	}
	var wanted []string
	checked, err := catFile(ctx, repo, "--batch-check=%(objectname) %(objecttype) %(objectsize)", shas)
	if err != nil {
		return scan, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(checked)), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || f[1] != "blob" {
			continue
		}
		if size, err := strconv.ParseInt(f[2], 10, 64); err == nil && size <= limit {
			wanted = append(wanted, f[0])
		}
	}

	// The latest version: what the tip of any branch has. Not only HEAD's,
	// since a file as it stands on another branch is just as current there --
	// and not dependent on HEAD, which in a bare repository can name a branch
	// that does not exist.
	current := map[string]bool{}
	heads, _ := runGit(ctx, repo, "", "for-each-ref", "--format=%(refname)", "refs/heads")
	for _, ref := range strings.Fields(heads) {
		tree, err := runGit(ctx, repo, "", "ls-tree", "-r", ref)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(tree, "\n") {
			if f := strings.Fields(line); len(f) >= 3 && f[1] == "blob" {
				current[f[2]] = true
			}
		}
	}

	type found struct{ current, older map[string]bool }
	byAddress := map[string]*found{}
	var order []string
	lfs := map[string]bool{}

	contents, err := catFile(ctx, repo, "--batch", wanted)
	if err != nil {
		return scan, err
	}
	r := bufio.NewReader(bytes.NewReader(contents))
	for {
		header, err := r.ReadString('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			return scan, err
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			return scan, fmt.Errorf("unexpected cat-file output %q", header)
		}
		size, _ := strconv.Atoi(f[2])
		body := make([]byte, size+1) // the content, then a newline
		if _, err := io.ReadFull(r, body); err != nil {
			return scan, err
		}
		body = body[:size]
		sha, path := f[0], pathOf[f[0]]

		if size <= lfsPointerMax && bytes.HasPrefix(body, []byte(lfsPointer)) {
			lfs[path] = true
			continue
		}
		if !addresses || bytes.IndexByte(body, 0) >= 0 {
			continue // binary
		}
		for _, addr := range redact.FindAddresses(body) {
			if !personal(addr) {
				continue
			}
			key := strings.ToLower(addr)
			hit := byAddress[key]
			if hit == nil {
				hit = &found{current: map[string]bool{}, older: map[string]bool{}}
				byAddress[key] = hit
				order = append(order, addr)
			}
			if current[sha] {
				hit.current[path] = true
			} else {
				hit.older[path] = true
			}
		}
	}

	sort.Slice(order, func(i, j int) bool { return strings.ToLower(order[i]) < strings.ToLower(order[j]) })
	for _, addr := range order {
		hit := byAddress[strings.ToLower(addr)]
		fa := FileAddress{Address: addr, Current: sortedKeys(hit.current)}
		for _, p := range sortedKeys(hit.older) {
			if !hit.current[p] {
				fa.Older = append(fa.Older, p)
			}
		}
		scan.addresses = append(scan.addresses, fa)
	}
	scan.lfsFiles = sortedKeys(lfs)
	return scan, nil
}

// catFile feeds object names to git cat-file and returns what it prints.
func catFile(ctx context.Context, repo, mode string, shas []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "cat-file", mode)
	cmd.Stdin = strings.NewReader(strings.Join(shas, "\n") + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file %s: %v: %s", mode, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// personal reports whether an address found in a file could be somebody's.
//
// Left out: addresses that are reserved for examples or can never be
// delivered; no-reply addresses; git's own SSH user on the big forges, which
// appears in every clone URL written into a README; and the "name@2x.png"
// that the pattern mistakes for an address.
func personal(addr string) bool {
	addr = strings.ToLower(addr)
	local, domain, ok := strings.Cut(addr, "@")
	if !ok {
		return false
	}
	if redact.IsRedacted(addr) || strings.Contains(local, "noreply") || strings.Contains(domain, "noreply") ||
		strings.Contains(local, "no-reply") {
		return false
	}
	switch domain {
	case "example.com", "example.org", "example.net", "localhost":
		return false
	case "github.com", "gitlab.com", "bitbucket.org":
		if local == "git" {
			return false
		}
	}
	tld := domain[strings.LastIndex(domain, ".")+1:]
	switch tld {
	case "invalid", "test", "example", "localhost",
		"png", "jpg", "jpeg", "gif", "svg", "webp", "ico",
		"js", "ts", "css", "go", "txt", "json", "html", "yml", "yaml":
		return false
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
