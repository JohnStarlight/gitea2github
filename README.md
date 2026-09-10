# gitea2github

Move your repositories from a Gitea instance to GitHub — **with every branch and
tag intact** — and repoint your local clones at the new home.

Built for [Zone01](https://platform.zone01.gr) students who want their coursework
on GitHub for a portfolio, but it works with any Gitea instance.

## Why not just do it by hand?

The manual route — create repo, copy URL, `git remote add`, `git push` — carries
over only the branch you happen to have checked out. Every other branch and every
tag stays behind on Gitea. This tool uses `git clone --mirror` / `git push --mirror`,
which copies **all** refs.

## Install

```sh
go install github.com/JohnStarlight/gitea2github@latest
```

Or from a checkout: `go build -o gitea2github .`

## Credentials

Nothing to configure if you already use Gitea and GitHub from the shell:

| Service | Looked up in order |
| --- | --- |
| Gitea | `GITEA_TOKEN` env var → your git credential helper (macOS keychain) |
| GitHub | `GITHUB_TOKEN` env var → the `gh` CLI |

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
| `--gitea-url` | `https://platform.zone01.gr/git` | Source instance |

**relink**

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dry-run` | `false` | Report without changing |
| `--keep-as` | `gitea` | New name for the old remote |
| `--verify` | `true` | Confirm the GitHub repo exists first |

## Known limitations

- **Git LFS objects are not carried across** by `--mirror`. Repositories using
  LFS need `git lfs fetch --all` / `git lfs push --all` in addition.
- **Issues, pull requests and wikis stay on Gitea.** This tool moves Git data,
  not the Gitea-side collaboration metadata.
- Destination repositories are created under the authenticated user's account,
  not under organisations.
