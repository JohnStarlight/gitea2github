# gitea2github

Move your repositories from a Gitea instance to GitHub — **with every branch and
tag intact** — and repoint your local clones at the new home.

Built for [Zone01](https://platform.zone01.gr) students who want their coursework
on GitHub for a portfolio, but it works with any Gitea instance.

[Install](#install) ·
[Platform support](#platform-support) ·
[Credentials](#credentials) ·
[Usage](#usage) ·
[Examples](#examples) ·
[What gets skipped](#what-gets-skipped-and-why) ·
[Redacting emails](#redacting-email-addresses) ·
[Flags](#flags) ·
[Limitations](#known-limitations)

## Why not just do it by hand?

The manual route — create repo, copy URL, `git remote add`, `git push` — is not
only tedious, it is lossy. It carries over just the branch you happen to have
checked out; every other branch and every tag stays behind on Gitea.

Knowing that, you might reach for `git push --mirror` yourself. That works right
up until the first repository that ever had a pull request: Gitea keeps those
under `refs/pull/*`, GitHub reserves that namespace and rejects the write, and
the whole push fails. This tool drops those refs first.

Beyond getting the git data across intact:

- **Your teammates' email addresses stay private.** Every commit carries its
  author's address, and a group project carries the address of everyone who ever
  worked on it. Publishing the repository publishes all of them, and no manual
  step strips them out. `--redact-emails` replaces them throughout the history —
  in the author and committer headers *and* in the `Co-authored-by:` trailers
  inside commit messages, which is where most people forget to look.
- **Nothing is republished by accident.** Repositories owned by someone else,
  forks and archived repositories are left alone unless you ask for them, so a
  teammate's group project does not quietly become yours.
- **An interrupted run is just re-run.** Anything already on GitHub is reported
  as `exists` and left untouched, so you never have to remember where you got to
  half way through thirty repositories.
- **Visibility and descriptions come along.** A private Gitea repository lands
  private rather than accidentally public, and its description comes with it
  instead of being retyped.
- **Your local clones end up pointing where you want them.** After a hand
  migration they still push to Gitea, silently, until you notice. `relink` fixes
  that — and if you are not done with Gitea, `--push-to=both` makes a single
  `git push` reach both servers.
- **Thirty repositories are one command.** Transfers run in parallel, and the run
  ends with a summary of what moved, what was skipped and why.

## Install

```sh
go install github.com/JohnStarlight/gitea2github@latest
```

Or from a checkout: `go build -o gitea2github .`

## Platform support

macOS, Linux, Windows, BSD — anywhere Go and git run. Pure Go, no third-party
dependencies, and no per-OS code: credential storage, the one part that really
differs, is left to git itself.

Needs **Go 1.21+** to build and **git** on `PATH` to run. The `gh` CLI is
optional.

## Credentials

Nothing to configure if you already use Gitea and GitHub from the shell:

| Service | Looked up in order |
| --- | --- |
| Gitea | `GITEA_TOKEN` env var → your git credential helper |
| GitHub | `GITHUB_TOKEN` env var → the `gh` CLI |

The credential helper step is `git credential fill`, git's own protocol, so it
uses whatever store you already have: **osxkeychain** on macOS, **Git Credential
Manager** or **wincred** on Windows, **libsecret**, **pass** or **store** on
Linux. If you can already `git push` to your Gitea from the shell, there is
nothing to set up. If no helper is configured, set `GITEA_TOKEN` instead — or
configure one:

```sh
git config --global credential.helper osxkeychain   # macOS
git config --global credential.helper manager       # Windows
git config --global credential.helper libsecret     # Linux
```

`doctor` names the helper that actually answered, so you can tell a missing
token from a missing helper.

### If you have not set anything up

Nothing bad happens: the tool refuses to start rather than doing half a
migration. `migrate` resolves both credentials **before** it contacts either
server, so a missing token means no repository is cloned, created or pushed, and
the process exits non-zero having changed nothing.

Run `doctor` and it tells you which half is missing and what to do:

```
Gitea
  credential   FAIL  no GITEA_TOKEN set and no git credential helper is configured for platform.zone01.gr.
                     Either set GITEA_TOKEN, or configure a helper, for example:
                       macOS    git config --global credential.helper osxkeychain
                       Windows  git config --global credential.helper manager
                       Linux    git config --global credential.helper libsecret

GitHub
  credential   FAIL  no GITHUB_TOKEN set and `gh auth token` failed (is the gh CLI installed and logged in?)

error: 2 check(s) failed; see above
```

The more common case is a helper that *is* configured but has nothing saved for
your Gitea yet — normal before your first token. That is reported separately,
with the command that fixes it:

```
  credential   FAIL  no credential stored for platform.zone01.gr by osxkeychain
                     Store one with:
                       printf 'protocol=https\nhost=platform.zone01.gr\nusername=<user>\npassword=<token>\n\n' | git credential approve
                     or set GITEA_TOKEN instead.
                     git said: fatal: could not read Username for '...': terminal prompts disabled
```

You are never left waiting at an invisible prompt: interactive prompting is
disabled on every git call, so a missing credential is always an error message
rather than a hang. `doctor` exits non-zero when any check fails, so it can be
used as a precondition in a script.

Your Gitea token needs **both** `read:user` and `write:repository` scopes.
`read:user` is the one people miss — without it the API refuses to *list* your
repositories, even though the token can read and write them individually. Create
one at `<your-gitea>/user/settings/applications`.

## Usage

```sh
gitea2github doctor                # verify credentials and scopes first
gitea2github list                  # see what is visible, and how it is classified
gitea2github migrate --dry-run     # see exactly what would happen
gitea2github migrate               # do it
gitea2github relink ~/Git          # repoint local clones at GitHub
```

`doctor` tells you precisely which half of the chain is broken:

```
Gitea
  credential   ok    user=ivogiake via git credential helper (osxkeychain)
  reachable    ok    https://platform.zone01.gr/git (Gitea 1.27.2)
  list repos   FAIL  token does not have at least one of required scope(s), ...
               fix   create a token with BOTH read:user and write:repository
```

### Examples

**Always start here.** Nothing below `doctor` touches anything until you drop
`--dry-run`:

```sh
gitea2github doctor                 # do my credentials work, and do they have the right scopes?
gitea2github list                   # what can it see, and how does it classify each one?
gitea2github migrate --dry-run      # what exactly would happen, repo by repo?
```

**Migrate everything you own.** Group projects, forks and archived repositories
are left alone — see the table below:

```sh
gitea2github migrate
```

**Migrate one repository**, which is the sane way to test the real path for the
first time:

```sh
gitea2github migrate --only linear-stats
```

Several at once, comma-separated:

```sh
gitea2github migrate --only linear-stats,go-reloaded,tetris-optimizer
```

**Include group projects, without publishing your teammates' addresses.** This
is the combination most Zone01 students actually want:

```sh
gitea2github migrate --collaborations --redact-emails --keep-email you@example.com
```

`--collaborations` opts in to repositories owned by someone else, and
`--redact-emails` makes sure that doing so does not publish their personal
addresses. `--keep-email` exempts your own, so your commits stay linked to your
GitHub profile.

**Keep everything private on the GitHub side**, regardless of how it was set on
Gitea — useful for coursework you are not ready to show yet:

```sh
gitea2github migrate --private
```

**Go slower.** Creating repositories in a burst can trip GitHub's secondary rate
limit; one at a time is the safe setting for a large account:

```sh
gitea2github migrate --jobs 1
```

**A Gitea that is not Zone01's:**

```sh
gitea2github migrate --gitea-url https://gitea.example.com
```

**Repoint your local clones** once the repositories are across. Check first,
then do it:

```sh
gitea2github relink --dry-run ~/Git      # which clones would be repointed, and where?
gitea2github relink ~/Git                # origin -> GitHub, old remote kept as "gitea"
```

**Still need to push to Gitea as well?** Zone01 audits happen on the Gitea
instance, so abandoning it mid-course is not an option. One push, both servers:

```sh
gitea2github relink --push-to=both ~/Git
```

`origin` keeps fetching from Gitea and gains a second push URL, so `git push`
sends to both. Separate `gitea` and `github` remotes are added as well, for when
you want to aim at one on purpose.

Or leave `origin` completely alone and add only a `github` remote, so pushing to
GitHub is always deliberate:

```sh
gitea2github relink --push-to=gitea ~/Git
```

Prefer a different name for the old remote, or skip the existence check:

```sh
gitea2github relink --keep-as zone01 ~/Git
gitea2github relink --verify=false ~/Git
```

**Everything, carefully, in one sequence:**

```sh
gitea2github doctor
gitea2github migrate --dry-run --collaborations --redact-emails --keep-email you@example.com
gitea2github migrate --collaborations --redact-emails --keep-email you@example.com --jobs 2
gitea2github relink --dry-run ~/Git
gitea2github relink ~/Git
```

### What gets skipped, and why

By default the migrator leaves alone anything where "copy it to my account" is
not obviously the right call:

| Skipped | Include it with |
| --- | --- |
| Repositories owned by another Gitea user | `--collaborations` |
| Forks | `--forks` |
| Archived repositories | `--archived` |
| Empty repositories | never — there is nothing to push |

The collaboration default is the important one. Zone01 group projects live under
one teammate's account, and republishing a teammate's repository under your own
name is a decision you should make deliberately, not a default.

### Redacting email addresses

A group project carries the personal email address of every teammate who ever
committed to it. Publishing it on GitHub publishes those addresses, and none of
those people agreed to that.

```sh
gitea2github migrate --redact-emails --keep-email you@example.com
```

This replaces every address in the history with a stable, non-reversible
stand-in such as `4f2a91c0de@redacted.invalid`. Addresses are replaced in **both**
places they occur:

- the author and committer headers, which is what `git log` shows;
- the commit message body, where `Co-authored-by: Name <addr>` trailers are very
  common and just as public.

`.invalid` is reserved by RFC 2606 and can never resolve, so a redacted address
can never become someone else's real mailbox. The replacement is derived from a
hash of the original, which means the same person maps to the same stand-in in
every repository you migrate — `git shortlog` still separates contributors
correctly — while nothing of the original address survives.

Use `--keep-email` (repeatable) for your own address, so your commits stay linked
to your GitHub profile.

**This rewrites history.** Every commit hash changes, because the author and
committer identities are part of what a commit hashes. The migrated repository
is a parallel copy of the history rather than the same history: commit hashes
referenced anywhere else will not match, and commit signatures, which cannot
survive an identity change, are dropped. Without `--redact-emails` the history is
transferred byte for byte and hashes are preserved.

### Safety properties

- **Idempotent.** A repository already on GitHub is reported as `exists` and left
  untouched, so an interrupted run is simply re-run.
- **Nothing is deleted.** `relink` *renames* the Gitea remote to `gitea` rather
  than removing it, so `git push gitea` still works.
- **Secrets never reach the logs.** Tokens are injected into clone URLs at exec
  time and redacted from all command output.
- **No hanging.** `GIT_TERMINAL_PROMPT=0` turns a bad token into an error message
  instead of a background worker blocked forever on an invisible prompt.
- **Ctrl-C is clean.** Interrupting stops new work and still prints the summary
  for what finished.

## Flags

**migrate**

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dry-run` | `false` | Resolve and filter everything, change nothing |
| `--only` | all | Comma-separated repository names |
| `--jobs` | `4` | Repositories transferred at once |
| `--private` | `false` | Force every destination private |
| `--redact-emails` | `false` | Replace every email address in the history |
| `--keep-email` | none | Address to leave untouched (repeatable) |
| `--redact-domain` | `redacted.invalid` | Domain for redacted addresses |
| `--gitea-url` | `https://platform.zone01.gr/git` | Source instance |

**relink**

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dry-run` | `false` | Report without changing |
| `--push-to` | `github` | Where clones push: `github`, `both`, or `gitea` |
| `--keep-as` | `gitea` | New name for the old remote (`--push-to=github` only) |
| `--verify` | `true` | Confirm the GitHub repo exists first |

`--push-to` in full:

| Value | `origin` fetches | `git push` goes to | Extra remotes |
| --- | --- | --- | --- |
| `github` | GitHub | GitHub | `gitea` (the old one, renamed) |
| `both` | Gitea | **both servers** | `gitea`, `github` |
| `gitea` | Gitea | Gitea | `github` |

## Known limitations

- **Git LFS objects are not carried across** by `--mirror`. Repositories using
  LFS need `git lfs fetch --all` / `git lfs push --all` in addition.
- **Issues, pull requests and wikis stay on Gitea.** This tool moves Git data,
  not the Gitea-side collaboration metadata.
- Destination repositories are created under the authenticated user's account,
  not under organisations.
- Pull-request refs (`refs/pull/*`) are dropped. GitHub owns that namespace and
  rejects writes to it, so a mirror push carrying Gitea's copies would fail.
- `--redact-emails` changes every commit hash and drops commit signatures. See
  the section above before using it on a repository whose hashes are referenced
  elsewhere.

## License

MIT — see [LICENSE](LICENSE).
