# gitea2github

Move your repositories from a Gitea instance to GitHub — **with every branch and
tag intact** — and repoint your local clones at the new home.

Built for [Zone01](https://platform.zone01.gr) students putting their coursework
on GitHub, but it works with any Gitea instance.

[Install](#install) ·
[Getting started](#getting-started) ·
[Your token](#what-this-does-with-your-token) ·
[The screen](#the-selection-screen) ·
[One-way choices](#choices-you-cannot-take-back) ·
[Without the interface](#without-the-interface) ·
[Safety](#safety-properties)

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
  as `exists` and left untouched; a repository the interruption left empty is
  finished instead.
- **Visibility is never widened by accident.** Repositories are created exactly
  as they are on Gitea unless you say otherwise, one by one. Descriptions,
  websites, topics and the default branch come along too, instead of being
  set again by hand.
- **Your clones get repointed** — including [pushing to both
  servers](#repointing-your-clones) if you are not done with Gitea.
- **Thirty repositories are one command**, run in parallel, with a summary.

## Install

```sh
go install github.com/JohnStarlight/gitea2github@latest
```

Or from a checkout: `go build -o gitea2github .`

Runs on macOS, Linux, Windows and BSD — anywhere Go and git run. Needs **Go
1.21 or newer** installed to build, and **git** on `PATH` to run; the `gh` CLI
is optional. It is built with Go 1.27, which Go fetches by itself if yours is
older — so it runs where Go 1.27 does: **macOS 13 Ventura** or newer (Macs
from 2017–2018 on, and every Apple silicon Mac), Windows 10 or newer, and
Linux with kernel 3.2 or newer.

Everything is the Go standard library except `golang.org/x/term`, which is used
only to put the terminal into raw mode for the selection screen and to tell a
terminal from a pipe. That is roughly 165 lines of linked code from the Go
team, and it is the whole dependency tree — see [what this does with your
token](#what-this-does-with-your-token).

## Getting started

Three commands, in this order. None of them changes anything until you have
seen what it is about to do and said yes.

```sh
gitea2github doctor
```

Checks that both credentials work and have the scopes the rest will need. Run
it first; a migration that dies halfway through because a token was too narrow
is a worse way to find out.

```sh
gitea2github migrate
```

Opens a screen listing everything on Gitea, with what would happen to each
repository. Move with the arrows, include or exclude with `space`, press
`enter` when the tally at the bottom says what you meant. Then it shows the
plan and asks once more.

```sh
gitea2github relink
```

Your clones still push to Gitea after a migration. This scans the directory you
are standing in and repoints them — one destination per clone, chosen on a
second screen. `migrate` offers to do this for you when it finishes, so most of
the time you will not type it.

That is the whole tool. Everything below is detail about what the screens
offer, and a [last section](#without-the-interface) for people who would rather
type flags than look at a screen.

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

## The selection screen

`migrate` and `relink` each open one. Nothing is decided until you press
`enter`, so changing your mind about the forks after reading the plan costs a
keystroke rather than a restart.

```
 gitea.zone01.gr  ->  github.com/JohnStarlight                        40 repositories
   1 [ ] group projects (1)   2 [ ] forks (1)   3 [ ] archived (1)
   e REDACTING 2 of 4   E all   m keep: you@example.com
   REWRITING HISTORY CANNOT BE UNDONE -- ONLY FOR FINISHED PROJECTS

 > *  JohnStarlight/ascii-art       private   create, emails redacted
   *  JohnStarlight/lem-in          public    create, now public
   -  JohnStarlight/net-cat                   already on GitHub
   +  JohnStarlight/old-mirror                a fork (press 2)
   +  zone01/groupie-tracker             a group project (press 1)

   30 to migrate -> 20 unchanged + 4 visibility + 5 redacted + 1 both changes
   space select   v visibility   e redact   r rename   / search   enter migrate   q quit
```

Each row is drawn whole in the colour of what will happen to it, so the list
can be read at a glance rather than one row at a time:

| Colour | Symbol | Means |
| --- | --- | --- |
| Green | `*` | Copied to GitHub exactly as it is on Gitea |
| Orange | `*` | Visibility flipped away from the source |
| Purple | `*` | History rewritten to hide addresses |
| Pink | `*` | Both of those at once |
| Cyan | `+` | Held back only by a closed gate: one keystroke away |
| Grey | `-` | Nothing will happen to it |

The toggles along the top take the same colours a shade brighter, so what a
gate touches needs no explaining. The counts along the bottom take them too,
which makes the footer the key to the list. The symbols carry the same
distinctions, so the screen still reads in a monochrome terminal or to someone
who cannot separate the hues; where `TERM` does not claim 256 colours it falls
back to the sixteen every terminal has, choosing hues further apart rather than
closer.

| Key | Does |
| --- | --- |
| `↑` `↓` / `k` `j` | Move |
| `space` | Include or exclude the row |
| `v` | Flip that repository between public and private |
| `e` / `E` | Redact this repository's history / all of them |
| `r` | Change the name it will take on GitHub |
| `m` | Choose the address to keep linked to your GitHub account |
| `1` `2` `3` | Include group projects / forks / archived |
| `a` / `n` | Include or exclude everything on screen |
| `/` | Search by name — filters the view, never the selection |
| `enter` | Go on to the final confirmation |
| `q` / `esc` | Quit, changing nothing |

### Repointing your clones

Moving the repositories is half the job: the working copies on your machine
still push to Gitea. When a migration finishes, `migrate` offers to repoint
them and opens a second screen for the clones it finds:

```
 github.com/JohnStarlight                                                5 clones
   1 [x] github   2 [ ] both   3 [ ] gitea   A all
   d directory: ~/Git

 > *  ~/Git/ascii-art     github  push and pull use GitHub; Gitea kept as the "gitea" remote
   *  ~/Git/lem-in        both    push reaches both servers; pull still comes from Gitea
   *  ~/Git/go-reloaded   gitea   push and pull stay on Gitea; GitHub added as the "github" remote
   -  ~/Git/notes                 origin is not on platform.zone01.gr

   3 to repoint   1 github   1 both   1 gitea   2 left alone
```

Each row says what the two commands you will actually type do afterwards,
rather than which remote gets moved where. The destination is chosen per clone:
a folder of coursework rarely wants one answer for all of it — the group
project you still push to Gitea for audits is not the one you are done with.
`A` gives every clone on screen the destination of the one under the cursor,
and `d` points the screen at another directory without leaving it. `r` gives a
clone the name its repository took on GitHub, when that is not its own name —
say, one you typed during the migration — and looks for it there.

**Who your next commits are made as.** A clone keeps the name and address
your git config gives it, and its next push to GitHub publishes them — after
a redacting migration, that may be the very address the rewrite hid. For the
clones whose pushes will go to GitHub, `relink` offers to make new commits as
your GitHub login and no-reply address instead, set in those clones only and
never in your global config, which your Gitea work uses too:

```
New commits in these 3 clones would still carry you@example.com,
and the next push would publish it on GitHub.
Make them as JohnStarlight <259051186+JohnStarlight@users.noreply.github.com>, in these 3 clones only? [Y/n]
```

After a redacting migration the answer defaults to yes; otherwise to no.

**`both` needs the GitHub repository to be private.** It carries whatever you
commit to GitHub on every push, addresses and all — contained while the
destination is private, a continuous publication while it is not.

## Choices you cannot take back

Most of what the screens offer is reversible. Including a fork, flipping a
repository to public, renaming it on GitHub — all of that can be changed
afterwards by running the tool again, or on GitHub itself.

Two things cannot, and both are drawn in red with the reason spelled out in
capitals above the list.

### Rewriting history to hide addresses

`e` and `E` replace every email address in a repository's history. The
rewritten commits are new objects with new hashes; the originals never reach
GitHub, and commit signatures are dropped because a signature covers the object
it signed.

It is right for work that is finished and wrong for work that is not. A clone
of the result can no longer push to the Gitea repository it came from — the
histories have no ancestor in common — so a repository you are still handing in
should keep its addresses until you are done with it.

### Taking on a rewritten history

The other side of the same coin. After a redacting migration the clones on your
machine still hold the original commits, so they can no longer push to what was
just created on GitHub: the copies there are different objects. Git rejects the
push, and the hint it prints in rejecting it points at `--force`, which would
republish every address the rewrite removed.

`relink` recognises those repositories by asking GitHub for the commits the
clone remembers from Gitea: a copy carries them across under the same hashes,
and a rewrite carries none of them, whatever addresses it left behind. For those
it offers one thing: the clone takes GitHub's rewritten history on and stops
being a clone of the Gitea repository. `both` and `gitea` are greyed out, because neither is possible.

```
 > *  ~/Git/ascii-art   adopt   takes on GitHub's rewritten history; Gitea remote removed
   -  ~/Git/go-reloaded         main has 2 commits that GitHub does not have
```

The history is fetched from GitHub rather than reproduced locally, so the
result matches by construction rather than by getting a rewrite exactly right.
Every branch and every tag moves, not only the one checked out: one left on
the original history would publish it again on its next push. Each moves onto
its twin on GitHub, which is harmless exactly when the twin has the same files —
redaction changes who made a commit, not what it contains — so that is what is
checked, for all of them, before anything moves. Your working tree is
untouched.

A clone is refused, with nothing changed, when it has something GitHub's copy
does not: uncommitted changes, a commit made after the migration, a branch or
tag that exists only here. The message says which, and what can be done. Work
that GitHub lacks cannot be added to a redacted copy afterwards, which is why
`migrate` takes [the work on your computer](#work-that-is-only-on-your-computer)
along in the first place.

A migration that redacted anything says so before offering, and offers with the
answer already yes:

```
2 histories were rewritten. The clones on this machine still hold the original,
so they can no longer push to GitHub: what is there now is different commits.
Have the clones under ~/Git take on the rewritten history? [Y/n]
```

Declining is fine and says what it costs. A migration that rewrote nothing asks
the tidier question it always did, with the answer still no.

## Redacting email addresses

A group project carries the personal address of everyone who ever committed to
it, and publishing the repository publishes all of them.

Every address becomes a stand-in such as `4f2a91c0de@redacted.invalid`, in both
places addresses hide: the author and committer headers, and the commit message
body, where `Co-authored-by:` trailers are just as public.

Within a repository, one person keeps one stand-in, so `git shortlog` still
separates contributors. Nothing of the original survives, and it cannot be
worked back out: each stand-in is computed under a random secret that exists
only while that repository is being rewritten. Without one, anybody with a
list of likely addresses could test them — and at a school, where every login
is public and every Gitea no-reply address follows from one, that list is easy
to make. Each repository gets its own secret, so a classmate is not
recognisable as the same person across your repositories either.

Repositories redacted by versions of this tool before the secret was added
used a plain hash, which can be tested that way. To protect one, delete it on
GitHub and migrate it again.

**`--keep-email` names an address of yours**, and rewrites it to your GitHub
no-reply — `<id>+<login>@users.noreply.github.com`, worked out from the account
the token belongs to — rather than leaving it as it was.

That distinction matters more than it looks. GitHub attributes a commit to an
account only when its address is one that account has verified, or its
no-reply. An address merely left alone — a Gitea no-reply, say — is hidden, but
shows as nobody: no avatar, no link, no contribution graph. Leaving it alone
therefore bought attribution only by publishing the real address it was
supposed to hide.

Rewriting gives both, and several addresses of yours collapse into one author,
so a history written from two machines does not arrive as two strangers:

```
you@example.com                 -> 259051186+JohnStarlight@users.noreply.github.com
you@noreply.platform.zone01.gr  -> 259051186+JohnStarlight@users.noreply.github.com
teammate@example.net            -> 7fe87d38c9@redacted.invalid
classmate@example.org           -> ff2018ab2f@redacted.invalid
```

Every replaced address takes the same shape: ten hexadecimal characters, then
`@redacted.invalid`. The domain is not configurable, for two reasons.

`.invalid` is reserved by RFC 2606 and can never resolve, so a redacted address
can never turn out to be a real mailbox belonging to somebody else. A domain
chosen by whoever ran the migration cannot promise that.

The shape also stays recognisable. A stand-in can be told from a real address
by looking at it, by a person or by a tool, and `relink` falls back on exactly
that when it cannot compare commits with GitHub. A shape that varied from run
to run could not be recognised at all.

### Addresses inside files

Redaction rewrites commits: who made them, and what their messages say. It
does not touch what the files contain — rewriting file contents is how files
get corrupted — so an address written into a `package.json`, a README or a
script is published with the repository however carefully the history was
redacted. It is also in every older version of that file, deleted or not.

So a redacted repository is looked through, every version of every file,
before anything is created. One that would be public and has addresses in its
files is held back — not created at all — and the run says where they are and
what the choices are:

```
ascii-art was NOT migrated. Its files contain email addresses,
and it would be public on GitHub:
  maria@mail.example.gr          in package.json
  kostas@uni.example.gr          in older versions of README.md

Redaction changes commits, NOT the files inside them. Your choices:
  1  Migrate it as private instead:
       press v on its row, or: gitea2github migrate --only ascii-art --visibility=private
  2  Publish it anyway:
       gitea2github migrate --only ascii-art --allow-emails-in-files
  3  Remove the addresses from the files on Gitea first. Older versions
     keep them too, so this needs the history rewritten (git filter-repo).
```

A private one goes ahead, with a warning that making it public later would
publish them. Addresses that belong to nobody are not reported: `example.com`
and `.invalid`, no-reply addresses, and the `git@github.com` of a clone URL.

## Work that is only on your computer

The migration copies from Gitea, so a commit you made after your last push —
the last touches to a finished project — would not reach GitHub. Before it
asks you to confirm, `migrate` looks through your copies of the repositories
(in the current directory, or wherever `--clones` points) and says what they
have that Gitea does not:

```
Checked your copies under ~/Git. This work is on this computer but NOT on Gitea:
  JohnStarlight/ascii-art  (~/Git/ascii-art)
      main: 1 commit; experiment: new branch, 2 commits; tag v2

Put this work on GitHub too? [Y/n]
Also send it to Gitea? [y/N]
```

Taken, it goes through the same redaction as everything else. Your copy is
only read, never changed. Two things are left out and say so: a branch where
Gitea also has commits your copy lacks, since taking one side would lose the
other; and a repository with work in more than one copy, since which one is
meant is yours to say.

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
  teammate/quadchecker -> quadchecker-teammate
  JohnStarlight/quadchecker -> quadchecker
```

`quadchecker-teammate` is correct and impersonal, and the person who owns the
repositories usually has a better word for which one it is — so `r` on the
selection screen replaces it with anything you like:

```
 > *  teammate/quadchecker   private   create as quadchecker-team
```

`relink` follows both. It matches a clone by the whole of its Gitea name,
`teammate/quadchecker`, not only by the last part, so the group's clone is
pointed at `quadchecker-teammate` and yours at `quadchecker`. The repointing
offered at the end of a migration knows every name, including the ones you
typed; `relink` run later works out the automatic ones again from Gitea, but
cannot know a name you typed: give it with `r` on its screen, or with
`--name teammate/quadchecker=quadchecker-team`. Whatever name it arrives at, a
GitHub repository whose commits and files both differ from the clone's is left
alone rather than repointed at.

## Without the interface

Everything above assumes you want to look at a screen. If you would rather type
a command and have it run, every choice the screens make has a flag, and
`--yes` skips the screen and the confirmation entirely.

This is also what happens automatically when there is no terminal — in a script,
a container, or a CI job — so the same invocation works in both places. Pass
`--no-tui` to get the numbered prompts on a terminal too.

A run that would change something and has nobody to ask refuses rather than
guessing:

```
refusing to change anything without a terminal to confirm on;
pass --yes to proceed or --dry-run to preview (12 repositories would be migrated)
```

### Common invocations

```sh
gitea2github list                                     # what can it see?
gitea2github migrate --dry-run                        # plan only, changes nothing
gitea2github migrate --only linear-stats              # one repository
gitea2github migrate --only linear-stats,go-reloaded  # or several
gitea2github migrate --visibility=private --yes       # unattended, force all private
gitea2github migrate --jobs 1                         # slower, kinder to rate limits
gitea2github migrate --gitea-url https://gitea.example.com
```

The combination most Zone01 students want — group projects included, without
publishing anyone's address, your own commits still linked to your profile:

```sh
gitea2github migrate --collaborations --redact-emails \
  --keep-email you@example.com --yes
```

Then the clones:

```sh
gitea2github relink                       # the directory you are standing in
gitea2github relink ~/Git                 # or another one
gitea2github relink --push-to=both --yes  # answer the destination in advance
gitea2github relink --dry-run ~/Git       # plan only
```

### migrate

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dry-run` | `false` | Print the plan and stop, asking nothing |
| `--yes` | `false` | Skip the screen and the confirmation |
| `--no-tui` | `false` | Numbered prompts instead of the screen |
| `--only` | all | Comma-separated repository names |
| `--collaborations` | `false` | Also migrate repositories owned by other Gitea users |
| `--forks` | `false` | Also migrate forks |
| `--archived` | `false` | Also migrate archived repositories |
| `--visibility` | `mirror` | `mirror` the Gitea setting, or force `private` / `public` |
| `--jobs` | `4` | Repositories transferred at once |
| `--redact-emails` | `false` | Rewrite every history to hide addresses |
| `--keep-email` | none | An address of yours, rewritten to your GitHub no-reply (repeatable) |
| `--clones` | current directory | Where your copies of the repositories are |
| `--local-work` | `include` | Work in those copies that Gitea lacks: `include` it on GitHub, or `skip` it |
| `--push-local-work` | `false` | Also send that work to Gitea |
| `--allow-emails-in-files` | `false` | Publish a redacted repository although its files contain addresses |

### relink

| Flag | Default | Meaning |
| --- | --- | --- |
| `--dry-run` | `false` | Print the plan and stop, asking nothing |
| `--yes` | `false` | Skip the screen and the confirmation |
| `--no-tui` | `false` | Numbered prompts instead of the screen |
| `--push-to` | `github` | Where clones push, for all of them |
| `--keep-as` | `gitea` | New name for the old remote (`--push-to=github` only) |
| `--verify` | `true` | Confirm the GitHub repository exists first |
| `--name` | none | The name a repository took on GitHub, as `owner/repo=name` (repeatable) |
| `--commit-as` | asked | `github`: new commits in clones that push to GitHub use your GitHub name and no-reply address; `keep`: leave them |

| `--push-to` | `origin` fetches | `git push` goes to | Extra remotes |
| --- | --- | --- | --- |
| `github` | GitHub | GitHub | `gitea` (the old one, renamed) |
| `both` | Gitea | **both servers** | `gitea`, `github` |
| `gitea` | Gitea | Gitea | `github` |

`both` is refused unless the GitHub repository is private, and refused outright
for a repository whose history was redacted. `doctor` and `list` take no flags
of their own. Every command takes `--gitea-url`.

## Safety properties

- **Nothing changes without a confirmation.** The plan comes from running the
  real pipeline in dry-run mode, not a separate code path, so it cannot drift
  out of step with what happens.
- **No prompt is mandatory.** Without a terminal, questions return defaults and
  a run that would change something stops rather than proceeding unasked.
- **Idempotent.** A repository already on GitHub is reported as `exists`, so an
  interrupted run is simply re-run. One that was created but never received
  its push is empty, and the next run pushes into it rather than calling it
  done — made private first if Gitea and GitHub disagree about it, unless you
  chose otherwise. The push is atomic, so a repository is either empty or
  complete, never half-filled.
- **Visibility is mirrored, not guessed.** A repository is created exactly as
  private or public as it is on Gitea unless you change it deliberately, on the
  row in front of you.
- **The screens decide nothing on their own.** Quitting with `q`, `esc` or
  Ctrl-C changes nothing, and what they hand over is exactly what the tally at
  the bottom said.
- **Tokens never reach an argument, a URL or a file.** Git is handed a plain
  address and asks for the credential through `GIT_ASKPASS`, which re-runs this
  binary and reads it from the environment — so nothing appears in the argument
  list `ps` publishes, or in the config a clone writes.
- **The Gitea remote survives unless it cannot.** `relink` renames it rather
  than deleting it, so `git push gitea` still works. The one exception is a
  clone taking on a rewritten history, where the old remote is removed because
  it can no longer be pushed to.
- **Ctrl-C is clean.** Interrupting stops new work and still prints the summary.
  Interrupting a screen restores the terminal first, so you are never left in a
  shell that has stopped echoing what you type.

## Known limitations

- **Git LFS files are not carried across** — only their pointers are. A
  repository that uses LFS is named at the end of the run, with the commands
  that copy its files from Gitea to GitHub.
- **Issues, pull requests and wikis stay on Gitea.** This moves Git data, not
  collaboration metadata.
- Destination repositories are created under your own account, not organisations.
- Pull-request refs (`refs/pull/*`) are dropped; GitHub rejects writes there.
- `--redact-emails` changes every commit hash and drops commit signatures.

## License

MIT — see [LICENSE](LICENSE).
