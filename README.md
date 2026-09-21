# gitea2github

Move your repositories from a Gitea instance to GitHub — **with every branch and
tag intact** — and repoint your local clones at the new home.

Built for [Zone01](https://platform.zone01.gr) students putting their coursework
on GitHub, but it works with any Gitea instance.

[Install](#install) ·
[Credentials](#credentials) ·
[Your token](#what-this-does-with-your-token) ·
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
- **Visibility is never widened by accident.** Repositories are created exactly
  as they are on Gitea unless you say otherwise, one by one. Descriptions come
  along too, instead of being retyped.
- **Your clones get repointed** — including [pushing to both
  servers](#flags) if you are not done with Gitea.
- **Thirty repositories are one command**, run in parallel, with a summary.

## Install

```sh
go install github.com/JohnStarlight/gitea2github@latest
```

Or from a checkout: `go build -o gitea2github .`

Runs on macOS, Linux, Windows and BSD — anywhere Go and git run. Needs **Go
1.21+** to build and **git** on `PATH` to run; the `gh` CLI is optional.

Everything is the Go standard library except `golang.org/x/term`, which is used
only to put the terminal into raw mode for the selection screen and to tell a
terminal from a pipe. That is roughly 165 lines of linked code from the Go
team, and it is the whole dependency tree — see [what this does with your
token](#what-this-does-with-your-token).

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

## What this does with your token

This tool asks for the keys to two accounts, so it should not ask you to take
its word for anything. Every claim below is one command away from being
checked, on the copy of the source you are about to build.

**It never asks you to paste a token.** It reads the credentials you already
use from the shell — `gh auth token`, the git credential helper, your keychain,
or an environment variable you set. There is no config file to write a secret
into, and nothing to mistype.

**Your tokens only ever go to two hosts.** The GitHub address is a constant in
the source, not a setting; the Gitea address is the one you type. Everything
that opens a network connection lives in two files:

```sh
grep -rn 'http\.\|Dial(' --include='*.go' internal main.go | grep -v _test
```

**Your tokens never reach the disk, the process list, or the logs.** The tool
writes no config file and keeps no credential of its own. Tokens live in memory
for the length of the run and are stripped from every line of git output before
it is printed.

If you pass credentials in `--gitea-url` yourself, they are stripped before
the address is printed, so a token cannot end up in your scrollback, in a
screenshot or in a pasted bug report.

Crucially, a token is never spliced into a URL. Doing that is the usual way to
authenticate git from a program, and it leaks twice: the URL shows up in the
argument list `ps` publishes to **every** user on the machine, and `git clone`
records the URL it cloned from, so the token is also written into the mirror's
config — where an interrupted run would leave it. Instead git is handed a
plain URL and asks for the credential through `GIT_ASKPASS`, which re-runs this
binary and reads the token from its environment. Environment variables are
readable only by you and root, and are never written anywhere:

```sh
grep -rn 'GIT_ASKPASS\|askpassEnv' --include='*.go' internal
```

That property is enforced rather than documented: `runGitAs` refuses to run a
git command that carries the credential in its arguments, and a test fails the
build if anything in the package builds a URL with credentials in it.

**The dependency tree is one package deep.** Two modules, both from the Go
team, neither of which can open a network connection:

```sh
go list -m all          # golang.org/x/term, golang.org/x/sys
```

`go.sum` pins both by hash, and Go verifies them against the public
[transparency log](https://sum.golang.org) on every build, so the code you
audit is the code that runs.

**You build it yourself.** `go install` compiles from source on your machine.
There is no prebuilt binary to trust, and no install script piped into a shell.

## Commands

| Command | Takes | What it does |
| --- | --- | --- |
| `doctor` | — | Checks both credentials and their scopes |
| `list` | — | Lists the Gitea repositories it can see, and how each is classified |
| `migrate` | — | Mirrors repositories to GitHub |
| `relink` | a **directory** | Repoints the local clones under it away from Gitea, one destination per clone |

**`migrate` and `relink` never change anything without showing the plan and
asking.** Run either with no flags and it asks what you want, prints exactly what
it is about to do, and waits for a yes.

On a terminal, `migrate` opens a selection screen instead of a run of questions.
Nothing is decided until you press Enter, so changing your mind about the forks
after reading the plan costs a keystroke rather than a restart:

```
 gitea.zone01.gr  ->  github.com/ivogiake                        40 repositories
   1 [ ] group projects (1)   2 [ ] forks (1)   3 [ ] archived (1)
   e REDACTING 2 of 4   E all   m keep: you@example.com
   REWRITING HISTORY CANNOT BE UNDONE -- ONLY FOR FINISHED PROJECTS

 > *  ivogiake/ascii-art       private   create, emails redacted      ← amber
   *  ivogiake/go-reloaded     private   create, emails redacted      ← amber
   *  ivogiake/lem-in          public    create, now public, …        ← amber
   -  ivogiake/net-cat                   already on GitHub            ← grey
   +  ivogiake/old-mirror                a fork (press 2)             ← cyan
   +  zone01/groupie-tracker             a group project (press 1)    ← cyan

   30 to migrate -> 20 unchanged + 4 visibility + 5 redacted + 1 visibility & redacted
   space select   v visibility   a all   n none   / search   enter migrate   q quit
```

Each row is drawn whole in the colour of what will happen to it, so the state
of the list can be read at a glance rather than one row at a time:

| Colour | Symbol | Means |
| --- | --- | --- |
| Green | `*` | Will be copied to GitHub exactly as it is on Gitea |
| Orange | `*` | Visibility flipped away from the source |
| Purple | `*` | History rewritten to redact addresses |
| Pink | `*` | Both of those at once |
| Cyan | `+` | Held back only by a closed gate: one keystroke away |
| Grey | `-` | Nothing will happen to it — already on GitHub, empty, or unchecked |

Both changes are chosen one repository at a time. Redaction used to be a single
switch over the whole run, which meant that opening a gate or checking one more
box silently rewrote the history of whatever it brought in — a side effect on
the one operation that cannot be undone by unchecking a box afterwards. `E`
still applies it to everything at once when that is what you want, and undoes
itself when pressed again.

The counts along the bottom read as arithmetic rather than as a row of
independent figures — `20 + 10 = 30` can be checked at a glance — and are drawn
in the same colours, so the footer is the key to the list above it. When there
is nothing to split they collapse to `30 to migrate, all unchanged`, and a
category with nothing in it is left out rather than shown as a zero.

As the terminal narrows the line gives up the tail first and the breakdown
last: `could add` and `not moving` only restate what the cyan and grey rows
already say, while the breakdown says something only this line can. Before
summing the three kinds into one it shortens the combined label to `both
changes`, which is unambiguous with the other two named immediately before it.

The fuller shades need a 256-colour terminal. Where `TERM` does not claim one,
the screen falls back to the sixteen every terminal has, choosing hues that are
further apart rather than closer so the distinction survives the downgrade.

Redaction's own control is red rather than the colour of the rows it makes, and
a warning sits under it whenever any repository is being rewritten. It is the
only choice on either screen that cannot be taken back: the rewritten commits
are new objects, the originals never reach GitHub, and a clone of the result
can no longer push to the Gitea repository it came from. That is right for work
that is finished and wrong for work that is not.

The toggles along the top are drawn in the same colours, one shade brighter, so
what a gate touches needs no explaining: cyan while its repositories wait
behind it, green once they are coming along, amber for the one that changes
what lands on GitHub. A gate whose repositories are every one of them already
there is greyed out rather than left advertising a count it cannot act on.

The symbols carry the same distinction as the colours, so the screen still
reads in a monochrome terminal or to someone who cannot separate the hues.

| Key | Does |
| --- | --- |
| `↑` `↓` / `k` `j` | Move |
| `space` | Include or exclude the row |
| `v` | Flip that repository between public and private |
| `a` / `n` | Include or exclude everything on screen |
| `1` `2` `3` | Include group projects / forks / archived |
| `e` / `E` | Redact this repository's history / all of them |
| `r` | Change the name it will take on GitHub |
| `m` | Choose the address to keep linked to your GitHub account |
| `/` | Search by name — filters the view, never the selection |
| `enter` | Go on to the final confirmation |
| `q` / `esc` | Quit, changing nothing |

Pass `--no-tui` for the numbered prompts instead. That is also what runs
automatically when there is no terminal.

### Repointing your clones

Moving the repositories is only half the job: the working copies on your
machine still push to Gitea. When a migration finishes, `migrate` offers to
repoint them and opens a second screen for the clones it finds:

```
 github.com/ivogiake                                                5 clones
   1 [x] github   2 [ ] both   3 [ ] gitea   A all
   d directory: ~/Git

 > *  ~/Git/ascii-art     github  push and pull use GitHub; Gitea kept as the "gitea" remote
   *  ~/Git/lem-in        both    push reaches both servers; pull still comes from Gitea
   *  ~/Git/go-reloaded   gitea   push and pull stay on Gitea; GitHub added as the "github" remote
   -  ~/Git/notes                 origin is not on platform.zone01.gr
   -  ~/Git/quad                  no matching repository on GitHub yet

   3 to repoint   1 github   1 both   1 gitea   2 left alone
   space select   1/2/3 destination   A all   d directory   enter repoint   q quit
```

`d` points the screen at another directory and rescans without leaving it, so
opening it on the wrong folder costs a keystroke rather than a restart. The
scan walks the disk and asks GitHub about every clone it finds, so the screen
says what it is doing while it waits, and a path that cannot be read leaves the
selection you had built up alone.

Each row says what the two commands you will actually type do afterwards,
rather than which remote gets moved where — the mechanism is not the question
somebody is deciding on. Long descriptions wrap onto a second line rather than
being cut off.

The destination is chosen per clone rather than per run: a folder of coursework
rarely wants one answer for all of it — the group project you still push to
Gitea for audits is not the one you are done with. `A` gives every clone on
screen the destination of the one under the cursor, and `--push-to` still sets
them all from the command line.

### Repositories whose history was redacted

Redaction leaves GitHub holding commits that share no ancestor with the clone
on your machine. That clone cannot push there — git refuses it — and the hint
git prints in refusing points straight at `--force`, which would republish
every address the redaction removed.

`relink` recognises those repositories and offers one thing for them: the clone
takes on GitHub's rewritten history and stops being a clone of the Gitea
repository. `both` and `gitea` are greyed out, because neither is possible.

```
 > *  ~/Git/ascii-art   adopt   takes on GitHub's rewritten history; Gitea remote removed
   -  ~/Git/go-reloaded         2 commits never pushed to Gitea; push them first
```

The history is fetched from GitHub rather than reproduced locally, so the
result matches by construction rather than by getting a rewrite exactly right.
Your working tree is untouched — redaction changes who made a commit, not what
it contains — and a clone with uncommitted changes, or with commits that never
reached Gitea, is refused until that is dealt with: once it belongs to GitHub
it can never push to Gitea again.

This is the one operation either screen offers that rewrites what is on your
machine, and it cannot be undone. It is right for work that is finished and
wrong for work that is not.

`relink` opens the same screen on its own, for clones migrated by hand or on
another machine. With no directory it scans the one you are standing in; give
it a path to scan somewhere else. The scan changes nothing, and both the screen
and the plan name the directory they worked on, so a wrong guess is visible
before anything acts on it. The offer after a migration never runs unasked:
`--yes` is consent to the migration that was described, not to rewriting
remotes in directories it never touched, so an unattended run prints the
command to use instead.

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

Working out what would change...

  #  STATUS   REPOSITORY                VISIBILITY  DETAIL
  1  planned  ivogiake/linear-stats     private     would clone, redact emails, create and push
  2  planned  ivogiake/math-skills      private     would clone, redact emails, create and push
     exists   ivogiake/go-reloaded                  already on GitHub, left untouched
     skipped  ppetraki/ascii-art-color              owned by ppetraki (use --collaborations to include)

The repositories above will be created with the visibility shown.
To flip any, enter its number(s) separated by spaces [Enter to keep them as they are]: 2
  ivogiake/math-skills  private -> public

Migrate 2 repositories to github.com/JohnStarlight? [y/N]
```

Questions about exclusions appear only when the account actually contains
something to exclude — no "include forks?" if you have none. Any flag you pass
answers its question in advance.

**Visibility mirrors Gitea unless you change it.** Each repository being created
is numbered, with the visibility it will get; typing its number flips it. Doing
nothing changes nothing, in either direction.

```sh
gitea2github list                                    # what can it see?
gitea2github migrate --dry-run                       # plan only, no questions
gitea2github migrate --only linear-stats             # one repository
gitea2github migrate --only linear-stats,go-reloaded  # or several
gitea2github migrate --visibility=private --yes       # unattended, force all private
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

## Two repositories, one name

Gitea namespaces repositories by owner and GitHub does not, so your own
implementation of an exercise and the group's both want to land as
`you/quadchecker`. Left alone that is not merely untidy but quietly lossy: the
first one across creates the repository, the second is reported as already
present, and one of the two never moves while the summary says everything was
accounted for. Which one won depended on whichever worker finished first.

The names are worked out from the whole list before anything runs. Yours keeps
the plain name, since that is what you will look for; the others are told apart
by whose they are, and the run says so:

```
2 repositories are called "quadchecker"; renaming to keep both:
  akasapid/quadchecker -> quadchecker-akasapid
  ivogiake/quadchecker -> quadchecker
```

`quadchecker-akasapid` is correct and impersonal, and the person who owns the
repositories usually has a better word for which one it is — so `r` on the
selection screen replaces it with anything you like:

```
 > *  akasapid/quadchecker   private   create as quadchecker-team
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

`--keep-email` names an address of **yours**. It is rewritten to your GitHub
no-reply — `<id>+<login>@users.noreply.github.com`, worked out from the account
the token belongs to — rather than left as it was.

That distinction matters more than it looks. GitHub attributes a commit to an
account only when its address is one that account has verified, or its
no-reply. An address merely left alone — a Gitea no-reply, say — is hidden, but
shows as nobody: no avatar, no link, no contribution graph. Leaving the address
alone therefore bought attribution only by publishing the real one it was
supposed to hide.

Rewriting gives both. Several addresses of yours collapse into the one author,
so a history where you committed from two machines does not arrive as two
strangers:

```
john.vogiakelis@gmail.com            -> 259051186+JohnStarlight@users.noreply.github.com
ivogiake@noreply.platform.zone01.gr  -> 259051186+JohnStarlight@users.noreply.github.com
basilisalevizos@yahoo.gr             -> ec53e453c0@redacted.invalid
p.petrakis@hotmail.gr                -> 50e51655ba@redacted.invalid
```

Every replaced address takes the same shape: ten hexadecimal characters, then
`@redacted.invalid`. The domain is not configurable, for two reasons.

`.invalid` is reserved by RFC 2606 and can never resolve, so a redacted address
can never turn out to be a real mailbox belonging to somebody else. A domain
chosen by whoever ran the migration cannot promise that.

The shape also has to be recognisable later. A repository on GitHub is the only
record of how it was redacted, and reading that back — which addresses were
deliberately left alone, and so which ones anything working on that repository
afterwards has to leave alone too — means telling a redacted address from a
real one by looking at it. A shape that varied from run to run could not be
recognised at all.

## Flags

**migrate**

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dry-run` | `false` | Print the plan and stop, asking nothing |
| `--yes` | `false` | Skip the questions and the confirmation |
| `--no-tui` | `false` | Use numbered prompts instead of the selection screen |
| `--only` | all | Comma-separated repository names |
| `--collaborations` | `false` | Also migrate repositories owned by other Gitea users |
| `--forks` | `false` | Also migrate forks |
| `--archived` | `false` | Also migrate archived repositories |
| `--visibility` | `mirror` | `mirror` the Gitea setting, or force `private` / `public` |
| `--jobs` | `4` | Repositories transferred at once |
| `--redact-emails` | `false` | Replace every email address in the history, in every repository |
| `--keep-email` | none | An address of yours, rewritten to your GitHub no-reply (repeatable) |

**relink**

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dry-run` | `false` | Print the plan and stop, asking nothing |
| `--yes` | `false` | Skip the questions and the confirmation |
| `--push-to` | `github` | Where clones push, for all of them — see below |
| `--no-tui` | `false` | Use numbered prompts instead of the selection screen |
| `--keep-as` | `gitea` | New name for the old remote (`--push-to=github` only) |
| `--verify` | `true` | Confirm the GitHub repo exists first |

| `--push-to` | `origin` fetches | `git push` goes to | Extra remotes |
| --- | --- | --- | --- |
| `github` | GitHub | GitHub | `gitea` (the old one, renamed) |
| `both` | Gitea | **both servers** | `gitea`, `github` |
| `gitea` | Gitea | Gitea | `github` |

`both` needs the GitHub repository to be **private**, and is refused otherwise.
It carries whatever you commit to GitHub on every push, addresses and all —
contained while the destination is private, and a continuous publication while
it is not. It is also refused for a repository whose history was redacted,
where the only coherent outcome is for the clone to take that history on.

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
- **Visibility is mirrored, not guessed.** A repository is created exactly as
  private or public as it is on Gitea unless you change it deliberately, per
  repository, on the row in front of you.
- **The selection screen decides nothing on its own.** Quitting it with `q`,
  `esc` or Ctrl-C changes nothing, and what it hands the migrator is exactly
  what the tally at the bottom said.
- **Nothing is deleted.** `relink` renames the Gitea remote rather than removing
  it, so `git push gitea` still works.
- **Secrets never reach the logs.** Tokens are injected into clone URLs at exec
  time and redacted from all command output.
- **Ctrl-C is clean.** Interrupting stops new work and still prints the summary.
  Interrupting the selection screen restores the terminal first, so you are
  never left in a shell that has stopped echoing what you type.

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
