<div align="center">

# 📡 nnm-rss

**Personal RSS feeds for NNM Club torrents**

[![License: MIT](https://img.shields.io/badge/License-MIT-0x3654.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/image-ghcr.io%2F0x3654%2Fnnm--rss-2496ED.svg)](https://github.com/0x3654/nnm-rss/pkgs/container/nnm-rss)
[![Go](https://img.shields.io/badge/Go-1.27-00ADD8.svg)](https://go.dev)

[What is this](#what-is-this) · [Feeds](#feeds) · [Deploy](#deploy) · [API](#api) · [Plugin integration](#plugin-integration) · [О русском](#по-русски)

</div>

## What is this

A self-hosted RSS service for [NNM Club](https://nnm-club.cc) torrents — a personal
counterpart of the public Cub/Reactor trackers feed. You keep your subscriptions
in a web UI and consume them from any RSS client (transmission-rss, NASctl, …).

Design points:

- **Accounts are service-local** — login & password, not tracker credentials.
  NNM Club mints a unique passkey per download, there is no permanent one to reuse.
- **Magnets are bare btih** (DHT/PEX, no announce) — a `.torrent` file is assembled
  by the built-in DHT resolver and served at `/t/<hash>.torrent`.
- **A guest only sees the magnet of a topic's main release**, so a subscription
  tracks the *current* main release: a repack (new hash) becomes a new item and
  your client downloads the update.

| | |
|---|---|
| Section subscription | `rss.php?f=` — new releases of a forum |
| Topic subscription | `rss.php?topic=` — main release changes (repacks) |
| One-shot magnet | any info-hash, from a plugin or manual paste |
| Filters | per-subscription regex, on/off and auto toggles, poster thumbnails |
| State | single JSON on a volume (`/data`), no external DB |
| Resolve | polite parallel workers (8) with TTL caches |

## Feeds

Times respect `TZ_OFFSET` (default +4).

| Feed | URL | Contents |
|---|---|---|
| Auto | `/rss/<token>` | subscriptions with the "auto" toggle — watch a specific release |
| All | `/rss/<token>/all` | every subscription (sections act as a search variant) |
| Top 7 days | `/rss/<token>/top/7` | most seeded over the window |
| Top 14 days | `/rss/<token>/top/14` | same, wider window |

Item descriptions carry a poster and the film text from the release page, so RSS
clients render them as cards. Top-feed sections are picked with top-level
checkboxes (87 categories; video roots by default).

## Deploy

```bash
docker run -d --name nnm-rss \
  -p 8356:8356 -v nnm-rss-data:/data \
  -e PUBLIC_URL=https://nnm.example.com \
  -e OMDB_APIKEY=… \
  ghcr.io/0x3654/nnm-rss:latest
```

CI builds a scratch multi-arch image (deps → test → build, non-root, healthcheck).

| Env | Default | Purpose |
|---|---|---|
| `PORT` | `8356` | listen port |
| `DATA_DIR` | `/data` | state + resolver cache (back this volume up) |
| `NNM_BASE` | — | tracker base URL override |
| `TTL` | `600` | tracker page cache, seconds |
| `TZ_OFFSET` | `4` | feed pubDate timezone |
| `PUBLIC_URL` | — | base for `.torrent` links (omit → magnets) |
| `OMDB_APIKEY` | — | ratings for release cards |

Tests run in a container against live page snapshots in `tests/`:

```bash
docker run --rm -v "$PWD":/src -w /src -v nnm-gomod:/go/pkg/mod \
  golang:1.27-alpine go test ./...
```

## API

```
GET    /                          public home + "My subscription"
POST   /api/login, /api/register  login (cookie `s`, a year)
GET    /api/me                    profile: subs, histories, feed URLs
POST   /api/subs, PATCH/DELETE /api/subs/{id}
GET    /rss/{token}[/all|/top/{7|14}]
GET    /t/{hash}                  release page (title, files, magnet)
GET    /t/{hash}.torrent          DHT-assembled .torrent
GET    /healthz
```

## Plugin integration

`POST /api/login` with JSON `{login, password}` returns `{token}`; plugins then
send `Authorization: Bearer <token>` (CORS is open on `/api/*`, cookies not
needed). `POST /api/subs` also accepts a magnet — it becomes a one-shot `kind=hash`
subscription in the auto feed; `GET /api/card` renders a release card for
[Lampa](https://github.com/0x3654/lampa-plugins) plugins.

> [!NOTE]
> The DHT resolver gets up to 20 s for a fresh release, so the first poll
> receives an http `.torrent` (both NASctl and transmission-rss ignore magnets
> in `link`).

> [!IMPORTANT]
> Personal project, no affiliation with NNM Club. The tracker has no official
> API; parsing follows the guest-accessible pages and may break on redesigns.
> Inspired by [lostfilmfeed](https://lostfilmfeed.byalex.dev).

---

## По-русски

Сервис личных RSS-лент NNM-Club: главная публична, вход — **свой логин и
пароль** (не данные трекера; nnm минтит уникальный passkey на каждое
скачивание — постоянного нет). Ленты секретные: `/rss/<токен>` на профиль.

Подписка: раздел (новые раздачи), раздача (изменение главной раздачи) или
магнет (разовая — из плагина Lampa); вставка любой ссылки конвертируется
в тему с названием и постером. Фильтры-регэкспы, тумблеры «вкл» и «авто»,
постер в списке подписок. Истории две, как ленты (авто и общая).

Единица подписки = текущая **главная раздача темы**: батч обновился (новый
hash) → новый item, клиент скачает обновление. Магниты в ленте — btih без
announce (DHT/PEX), `.torrent` собирает встроенный DHT-резолвер
(`/t/<hash>.torrent`), кэш резолва живёт на томе `/data`.

Описание items — постер + текст фильма со страницы раздачи: RSS-клиент
показывает карточкой (NASctl, transmission-rss). Свежей раздаче лента даёт
DHT до 20 с, чтобы первый опрос получил http-`.torrent`.

Плагины Lampa ([Torrent Send](https://github.com/0x3654/lampa-plugins))
работают с сервисом через Bearer-API: кнопка «Добавить в Plex» пушит раздачу
в ленту «авто» → transmission-rss качает в библиотеку; карточка раздачи
рендерится `/api/card`.

## License

MIT © [0x3654](https://github.com/0x3654)
