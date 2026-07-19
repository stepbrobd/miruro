# miruro

A command line client for watching anime from [miruro.tv](https://www.miruro.tv/),
in the spirit of [ani-cli](https://github.com/pystardust/ani-cli). Search a title,
pick an episode and a provider, and play it in mpv or vlc, or download it.

Binary Cache:

- Cache: <https://cache.ysun.co>
- Key: `cache.ysun.co-1:WxPYwT5g3kt9XhUhHPpNLZKI9HIOsVVAuqSHpok8Qt4=`

## How it works

miruro is an aggregator keyed by AniList id. Search resolves through the public
AniList GraphQL API. Episode and source resolution go through miruro's secure pipe
over native HTTP/2, so the only external programs are the player and, for HLS
downloads, ffmpeg. Providers vary in availability by network and region, so miruro
lets you pick a provider and falls back to the next one when an upstream is
unreachable.

## Usage

```
miruro [query]                search and play
miruro -c                     resume from history
miruro frieren -e 5           jump to an episode
miruro frieren -d -e 5        download instead of playing
miruro frieren --dub          dub instead of sub
miruro frieren --provider kiwi pin a provider
miruro resolve 154587 -e 1    print a stream url for scripting, no playback
```

Flags: `-e/--episode`, `-d/--download`, `-q/--quality`, `-v/--vlc`, `--dub`,
`-c/--continue`, `--provider`, `-D/--delete`.

Config lives at `$XDG_CONFIG_HOME/miruro/config.toml` with keys `player`, `quality`,
`provider`, `download_dir`, `dub`, and is overridable through `MIRURO_*` environment
variables. History is stored at `$XDG_STATE_HOME/miruro/history.json`.

## Install

```
nix profile install github:stepbrobd/miruro
```

Runtime needs a player (`mpv`, or `vlc`, or `iina` on macOS) and `ffmpeg` for HLS
downloads. Selection is in process, so no `fzf`. HTTP is native, so no `curl`.

## Develop

```
nix develop
go run ./cmd/miruro frieren
go test ./...                 unit tests
go test -tags live ./...      live tests against the real api
```
