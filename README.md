# gitea2github

Move your repositories from a Gitea instance to GitHub — **with every branch and
tag intact** — and repoint your local clones at the new home.

Built for [Zone01](https://platform.zone01.gr) students putting their coursework
on GitHub, but it works with any Gitea instance.

[Install](#install) ·
[Credentials](#credentials) ·
[Commands](#commands) ·
[Examples](#examples) ·
[Redacting emails](#redacting-email-addresses) ·
[Flags](#flags) ·
[Safety](#safety-properties) ·
[Limitations](#known-limitations)

## Why not just do it by hand?

The manual route — create repo, copy URL, `git remote add`, `git push` — carries
over only the branch you have checked out. Every other branch and tag stays
behind.

Reaching for `git push --mirror` yourself gets you further, until the first
repository that ever had a pull request: Gitea keeps those under `refs/pull/*`,
GitHub reserves that namespace, and the push is rejected. This tool drops them
first. Beyond that:

- **Teammates' email addresses stay private.** Every commit carries its author's
  address, and publishing a group project publishes all of them.
  [`--redact-emails`](#redacting-email-addresses) strips them from the history.
- **Nothing is republished by accident.** Other people's repositories, forks and
  archived ones are [left alone](#what-gets-skipped-and-why) unless you ask.
- **An interrupted run is just re-run.** Anything already on GitHub is reported
  as `exists` and left untouched.
- **Everything lands private by default**, and descriptions come along instead
  of being retyped.
- **Your clones get repointed** — including [pushing to both
  servers](#flags) if you are not done with Gitea.
- **Thirty repositories are one command**, run in parallel, with a summary.

## Install

```sh
go install github.com/JohnStarlight/gitea2github@latest
```

Or from a checkout: `go build -o gitea2github .`

Runs on macOS, Linux, Windows and BSD — anywhere Go and git run. Pure Go, no
third-party dependencies, no per-OS code. Needs **Go 1.21+** to build and
**git** on `PATH` to run; the `gh` CLI is optional.

## Credentials

Nothing to configure if you already use Gitea and GitHub from the shell:

| Service | Looked up in order |
| --- | --- |
| Gitea | `GITEA_TOKEN` env var → your git credential helper |
| GitHub | `GITHUB_TOKEN` env var → the `gh` CLI |

The helper step is `git credential fill`, git's own protocol, so it uses whatever
store you have — osxkeychain, Git Credential Manager, libsecret, pass. If none is
configured, set `GITEA_TOKEN`, or configure one:

```sh
git config --global credential.helper osxkeychain   # macOS
git config --global credential.helper manager       # Windows
git config --global credential.helper libsecret     # Linux
```

Your Gitea token needs **both `read:user` and `write:repository`** scopes.
`read:user` is the one people miss — without it the API refuses to *list* your
repositories even though the token can read and write them individually. Create
one at `<your-gitea>/user/settings/applications`.

**With nothing set up, nothing bad happens.** Credentials are resolved before
either server is contacted, so a missing token means no repository is cloned,
created or pushed. `doctor` names which half is missing and what to do about it:

```
Gitea
  credential   FAIL  no credential stored for platform.zone01.gr by osxkeychain
                     Store one with:
                       printf 'protocol=https\nhost=platform.zone01.gr\nusername=<user>\npassword=<token>\n\n' | git credential approve
                     or set GITEA_TOKEN instead.
GitHub
  credential   FAIL  no GITHUB_TOKEN set and `gh auth token` failed

error: 2 check(s) failed; see above
```

Interactive prompting is disabled on every git call, so a missing credential is
always an error message, never a hang.

## Commands

| Command | Takes | What it does |
| --- | --- | --- |
| `doctor` | — | Checks both credentials and their scopes |
| `list` | — | Lists the Gitea repositories it can see, and how each is classified |
| `migrate` | — | Mirrors repositories to GitHub |
| `relink` | a **directory** | Repoints the local clones under it away from Gitea |

**`migrate` and `relink` never change anything without showing the plan and
asking.** Run either with no flags and it asks what you want, prints exactly what
it is about to do, and waits for a yes.

Prompts are skipped when stdin is not a terminal, so scripts and CI never hang.
There, a run that would change something refuses and names the flag you want:

| You want | Use |
| --- | --- |
| A preview, changing nothing | `--dry-run` |
| To go ahead unattended | `--yes` |

Every command takes `--gitea-url` (default `https://platform.zone01.gr/git`).

## Examples

```sh
gitea2github doctor       # do my credentials work, and do they have the right scopes?
gitea2github migrate      # asks, shows the plan, then asks again before doing it
```

A session:

```
Include 3 repositories owned by other people (group projects)? [y/N] n
Include 2 forks? [y/N] n
Replace email addresses in the commit history? [y/N] y
  Your own address, to keep linked to GitHub (blank for none): [me@example.com]
Create the GitHub repositories public? [y/N] n

Working out what would change...

STATUS   REPOSITORY                DETAIL
planned  ivogiake/linear-stats     would clone, redact emails, create and push
exists   ivogiake/go-reloaded      already on GitHub, left untouched
skipped  ppetraki/ascii-art-color  owned by ppetraki (use --collaborations to include)

Migrate 1 repository to github.com/JohnStarlight? [y/N]
```

Questions about exclusions appear only when the account actually contains
something to exclude — no "include forks?" if you have none. Any flag you pass
answers its question in advance, so `migrate --public` asks about everything
except visibility.

```sh
gitea2github list                                    # what can it see?
gitea2github migrate --dry-run                       # plan only, no questions
gitea2github migrate --only linear-stats             # one repository
gitea2github migrate --only linear-stats,go-reloaded  # or several
gitea2github migrate --public --yes                  # unattended, keep Gitea visibility
gitea2github migrate --jobs 1                        # slower, kinder to rate limits
gitea2github migrate --gitea-url https://gitea.example.com
```

The combination most Zone01 students want — group projects included, without
publishing anyone's address:

```sh
gitea2github migrate --collaborations --redact-emails --keep-email you@example.com
```

Then repoint the local clones. `relink` asks where they should push:

```sh
gitea2github relink ~/Git                    # asks: github, both, or gitea
gitea2github relink --push-to=both ~/Git     # answer it in advance
gitea2github relink --dry-run ~/Git          # plan only
```

## What gets skipped, and why

By default the migrator leaves alone anything where "copy it to my account" is
not obviously right:

| Skipped | Include it with |
| --- | --- |
| Repositories owned by another Gitea user | `--collaborations` |
| Forks | `--forks` |
| Archived repositories | `--archived` |
| Empty repositories | never — there is nothing to push |

The collaboration default is the important one: Zone01 group projects live under
one teammate's account, and republishing theirs under your own name should be a
deliberate act.

## Redacting email addresses

A group project carries the personal address of everyone who ever committed to
it, and publishing the repository publishes all of them.

```sh
gitea2github migrate --redact-emails --keep-email you@example.com
```

Every address becomes a stable stand-in such as `4f2a91c0de@redacted.invalid`,
in **both** places addresses hide: the author and committer headers, and the
commit message body, where `Co-authored-by:` trailers are just as public.

`.invalid` is reserved by RFC 2606 and can never resolve, so a redacted address
can never become someone else's real mailbox. The replacement is a hash of the
original, so one person maps to the same stand-in in every repository you
migrate — `git shortlog` still separates contributors — while nothing of the
original survives. `--keep-email` (repeatable) exempts your own address so your
commits stay linked to your GitHub profile.

**This rewrites history.** Every commit hash changes, because author and
committer identities are part of what a commit hashes, and commit signatures are
dropped. Without the flag, history transfers byte for byte.

## Flags

**migrate**

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dry-run` | `false` | Print the plan and stop, asking nothing |
| `--yes` | `false` | Skip the questions and the confirmation |
| `--only` | all | Comma-separated repository names |
| `--collaborations` | `false` | Also migrate repositories owned by other Gitea users |
| `--forks` | `false` | Also migrate forks |
| `--archived` | `false` | Also migrate archived repositories |
| `--public` | `false` | Carry Gitea visibility across; without it everything is private |
| `--jobs` | `4` | Repositories transferred at once |
| `--redact-emails` | `false` | Replace every email address in the history |
| `--keep-email` | none | Address to leave untouched (repeatable) |
| `--redact-domain` | `redacted.invalid` | Domain for redacted addresses |

**relink**

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dry-run` | `false` | Print the plan and stop, asking nothing |
| `--yes` | `false` | Skip the questions and the confirmation |
| `--push-to` | `github` | Where clones push — see below |
| `--keep-as` | `gitea` | New name for the old remote (`--push-to=github` only) |
| `--verify` | `true` | Confirm the GitHub repo exists first |

| `--push-to` | `origin` fetches | `git push` goes to | Extra remotes |
| --- | --- | --- | --- |
| `github` | GitHub | GitHub | `gitea` (the old one, renamed) |
| `both` | Gitea | **both servers** | `gitea`, `github` |
| `gitea` | Gitea | Gitea | `github` |

`doctor` and `list` take no flags of their own. Every command takes
`--gitea-url`.

## Safety properties

- **Nothing changes without a confirmation.** The plan comes from running the
  real pipeline in dry-run mode, not a separate code path, so it cannot drift out
  of step with what happens.
- **No prompt is mandatory.** Without a terminal, questions return defaults and a
  run that would change something stops rather than proceeding unasked.
- **Idempotent.** A repository already on GitHub is reported as `exists`, so an
  interrupted run is simply re-run.
- **Private by default.** Repositories are created private unless you pass
  `--public`, and even then a repository that was private on Gitea stays private:
  carrying visibility across never widens it.
- **Nothing is deleted.** `relink` renames the Gitea remote rather than removing
  it, so `git push gitea` still works.
- **Secrets never reach the logs.** Tokens are injected into clone URLs at exec
  time and redacted from all command output.
- **Ctrl-C is clean.** Interrupting stops new work and still prints the summary.

## Known limitations

- **Git LFS objects are not carried across** by `--mirror`. Those repositories
  need `git lfs fetch --all` / `git lfs push --all` as well.
- **Issues, pull requests and wikis stay on Gitea.** This moves Git data, not
  collaboration metadata.
- Destination repositories are created under your own account, not organisations.
- Pull-request refs (`refs/pull/*`) are dropped; GitHub rejects writes there.
- `--redact-emails` changes every commit hash and drops commit signatures.

## License

MIT — see [LICENSE](LICENSE).
