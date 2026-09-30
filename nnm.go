package main

// Клиент NNMClub. Авторизации нет нигде: родные rss.php читает гость, info-hash
// раздачи виден в магнит-ссылке на странице темы. Лента отдаёт магниты с
// персональным announce юзера (bt02.nnm-club.cc:2710/<passkey>/announce) —
// клиент аннонсит его ключом, статистика на трекере считается ему.

import (
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

var nnmBase = env("NNM_BASE", "https://nnmclub.to")

const nnmUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36"

var nnmClient = &http.Client{Timeout: 20 * time.Second}

var omdbKey = env("OMDB_APIKEY", "") // бесплатный ключ omdbapi.com — для рейтинга IMDb (опционально)

// fetchNNM — GET с UA и (не)равным cookie; cp1251 → utf-8 (валидный utf-8 не трогаем)
func fetchNNM(u, cookie string) (string, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", nnmUA)
	req.Header.Set("Accept-Encoding", "identity")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	// трекер притормаживает быстрые серии запросов — до 3 попыток с паузой
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		resp, err = nnmClient.Do(req)
		if err == nil && resp.StatusCode != 200 {
			resp.Body.Close()
			err = fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
		}
		if err == nil {
			break
		}
		if attempt >= 2 {
			return "", err
		}
		time.Sleep(time.Duration(700*(attempt+1)) * time.Millisecond)
		req, err = http.NewRequest("GET", u, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("User-Agent", nnmUA)
		req.Header.Set("Accept-Encoding", "identity")
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return decodeBody(raw), nil
}

func decodeBody(raw []byte) string {
	if utf8.Valid(raw) {
		return string(raw)
	}
	decoded, err := charmap.Windows1251.NewDecoder().Bytes(raw)
	if err != nil {
		return string(raw)
	}
	return string(decoded)
}

// hashPoster — обложка разовой (hash) раздачи: темы у неё нет, ищем на
// трекере по названию («Курьер / Runner / 2026 / ДБ / WEB-DL (1080p)» →
// «Курьер 2026»), найденную тему резолвим и берём постер. Промах/нет сети —
// пусто, строка останется без картинки
var hashNameYearRe = regexp.MustCompile(`^(.*?)/.*?[( ]((19|20)\d{2})`)

func hashPoster(title string) string {
	if id := searchTopicID(title); id > 0 {
		if rel, err := resolveRelease(id); err == nil {
			return rel.Poster
		}
	}
	return ""
}

// searchTopicID — тема разовой раздачи по названию из dn плагина:
// «Курьер / Runner / 2026 / ДБ / WEB-DL (1080p)» → id темы на трекере
// (0 = не нашли). Два прохода: русское имя, затем оригинал
func searchTopicID(title string) int {
	m := hashNameYearRe.FindStringSubmatch(title)
	if m == nil {
		m = []string{"", title, "", ""}
		if y := regexp.MustCompile(`((19|20)\d{2})`).FindStringSubmatch(title); y != nil {
			m[2] = y[1]
		}
	}
	// имя и оригинал («Курьер / Runner / 2026 / …»): оригинал спасает,
	// когда форма русского у трекера и в dn плагина разошлась
	// («Бегушая» у Lampa vs «Бегущая» на NNM)
	name := strings.TrimSpace(strings.Split(m[1], "/")[0])
	// оригинал — второй сегмент полного названия (m[1] обрезан по первой «/»)
	orig := ""
	if segs := strings.Split(title, "/"); len(segs) > 1 {
		if o := strings.TrimSpace(segs[1]); !strings.ContainsAny(o[:1], "0123456789") {
			orig = o
		}
	}
	year := m[2]
	if year == "" || len(name) < 2 {
		return 0
	}
	if id := topicByQuery(name+" "+year, name, orig, year); id > 0 {
		return id
	}
	if len(orig) >= 3 {
		return topicByQuery(orig+" "+year, name, orig, year)
	}
	return 0
}

func topicByQuery(query, name, orig, year string) int {
	if name == "" || year == "" || len(name) < 2 {
		return 0
	}
	// поиск трекера понимает nm= в utf-8; cp1251-байты он молча
	// отбрасывает и показывает дефолтную страницу
	u := nnmBase + "/forum/tracker.php?nm=" + url.QueryEscape(query)

	key := "nm:" + query
	var body string
	cacheMu.Lock()
	if e, ok := rssCache[key]; ok && time.Since(e.ts) < time.Duration(ttl)*time.Second {
		body = e.body
		cacheMu.Unlock()
	} else {
		cacheMu.Unlock()
		b, err := fetchNNM(u, "")
		if err != nil {
			log.Printf("hashPoster поиск «%s»: %v", query, err)
			return 0
		}
		body = b
		cacheMu.Lock()
		rssCache[key] = rssCacheEntry{ts: time.Now(), body: body}
		markCacheDirty()
		cacheMu.Unlock()
	}

	// страница поиска линкует темы через viewtopic.php?t= (топ — через
	// tracker.php?t=, parseTrackerTop тут не годится)
	nameL, origL, yearL := strings.ToLower(name), strings.ToLower(orig), year
	seenTopic := map[int]bool{}
	for _, m := range searchRowRe.FindAllStringSubmatch(body, -1) {
		id, _ := strconv.Atoi(m[1])
		if id == 0 || seenTopic[id] {
			continue
		}
		seenTopic[id] = true
		t := strings.ToLower(tagRe.ReplaceAllString(m[2], ""))
		nameHit := nameL != "" && strings.Contains(t, nameL)
		origHit := origL != "" && strings.Contains(t, origL)
		if (nameHit || origHit) && strings.Contains(t, yearL) {
			return id
		}
	}
	log.Printf("hashPoster «%s»: совпадений нет (строк %d)", query, len(seenTopic))
	return 0
}

var searchRowRe = regexp.MustCompile(`(?s)<a[^>]*href="viewtopic\.php\?t=(\d+)"[^>]*>(.*?)</a>`)

// ---------- разбор родного rss.php

type rssItem struct {
	Title string
	Kind  string // topic | post
	ID    int    // id темы или поста
	Date  time.Time
}

var (
	rssItemRe  = regexp.MustCompile(`(?s)<item[^>]*>(.*?)</item>`)
	rssTitleRe = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
	rssLinkRe  = regexp.MustCompile(`<link>\s*([^<]+?)\s*</link>`)
	rssDateRe  = regexp.MustCompile(`<pubDate>([^<]+)</pubDate>`)
	rssChanRe  = regexp.MustCompile(`(?s)<channel>.*?<title>(.*?)</title>`)

	linkTopicRe = regexp.MustCompile(`viewtopic\.php\?t=(\d+)`)
	linkPostRe  = regexp.MustCompile(`viewtopic\.php\?p=(\d+)`)
)

// parseRSSItems — items родной ленты; ссылки бывают на темы (t=, новые раздачи
// раздела) и на посты (p=, новые сообщения в теме)
func parseRSSItems(body string) []rssItem {
	var out []rssItem
	for _, m := range rssItemRe.FindAllStringSubmatch(body, -1) {
		seg := m[1]

		tm := rssTitleRe.FindStringSubmatch(seg)
		if tm == nil {
			continue
		}
		it := rssItem{Title: html.UnescapeString(tm[1])}

		lm := rssLinkRe.FindStringSubmatch(seg)
		if lm != nil {
			if pm := linkPostRe.FindStringSubmatch(lm[1]); pm != nil {
				it.Kind, it.ID = "post", atoiDefault(pm[1])
			} else if tm2 := linkTopicRe.FindStringSubmatch(lm[1]); tm2 != nil {
				it.Kind, it.ID = "topic", atoiDefault(tm2[1])
			}
		}
		if it.ID == 0 {
			continue
		}

		if dm := rssDateRe.FindStringSubmatch(seg); dm != nil {
			it.Date, _ = time.Parse(time.RFC1123, strings.TrimSpace(dm[1]))
		}
		if it.Date.IsZero() {
			it.Date = time.Now()
		}
		out = append(out, it)
	}
	return out
}

// parseRSSChannel — название раздела из заголовка канала
func parseRSSChannel(body string) string {
	if m := rssChanRe.FindStringSubmatch(body); m != nil {
		return html.UnescapeString(strings.TrimSpace(m[1]))
	}
	return ""
}

// cleanTitle: «Раздел :: RE: Название» → «Название»
func cleanTitle(s string) string {
	if i := strings.LastIndex(s, "::"); i >= 0 {
		s = s[i+2:]
	}
	s = strings.TrimSpace(s)
	for strings.HasPrefix(s, "RE:") {
		s = strings.TrimSpace(strings.TrimPrefix(s, "RE:"))
	}
	return s
}

// ---------- страницы тем/постов → торрент

var (
	magnetRe   = regexp.MustCompile(`magnet:\?xt=urn:btih:([A-Fa-f0-9]{40})`)
	titleTagRe = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
	tagRe      = regexp.MustCompile(`<[^>]+>`)
	spaceRe    = regexp.MustCompile(`\s+`)

	// постер: var.postImg c url картинки; рейтинги КП/IMDb и служебные баннеры — мимо
	posterRe = regexp.MustCompile(`var class="postImg[^"]*" title="([^"]+)"`)
	// описание релиза: текст после «Описание:» до следующего тега
	descrRe = regexp.MustCompile(`(?s)Описание:\s*(?:</[a-z]+>|<[^>]+>)*\s*([^<]{40,1000})`)

	// техполя раздачи: «<span bold>Жанр:</span> значение<br»
	techRe = regexp.MustCompile(`(?s)<span[^>]*font-weight:\s*bold[^>]*>\s*([^<>]{2,30}):\s*</span>\s*(.*?)<br`)

	// сюжет — до следующего жирного поля (многоабзацный)
	plotRe = regexp.MustCompile(`(?s)Описание:\s*</span>\s*(.*?)<span[^>]*font-weight:\s*bold`)
	// карточка: рейтинг-картинка КП, ссылки на КП/IMDb, имя/размер торрента
	kpRatingRe = regexp.MustCompile(`(https?://(?:www\.)?kinopoisk\.ru/rating/\d+\.gif)`)
	kpLinkRe   = regexp.MustCompile(`(https?://(?:www\.)?kinopoisk\.ru/film/\d+/?)`)
	imdbLinkRe = regexp.MustCompile(`(https?://(?:www\.)?imdb\.com/title/(tt\d+)/?)`)
	malLinkRe  = regexp.MustCompile(`(https?://(?:www\.)?myanimelist\.net/anime/(\d+))`)
	torNameRe  = regexp.MustCompile(`<b>(\[NNM[-.]?Club[^<]*?\.torrent)</b>`)
	torSizeRe  = regexp.MustCompile(`(\d+(?:[.,]\d+)?(?:&nbsp;|\s)*(?:KB|MB|GB|КБ|МБ|ГБ))`)

	// своя тема на странице p=: ссылка в заголовке (maintitle) и пагинация
	// (t=…&start=). Частоту голых t= не считаем: виджет благодарностей под
	// каждым постом ведёт в служебную тему «Доска почета» (t=15061) и в
	// обсуждаемых раздачах побеждает по частоте саму тему
	postSelfTitle = regexp.MustCompile(`<a[^>]*class="maintitle"[^>]*>`)
	postSelfPage  = regexp.MustCompile(`viewtopic\.php\?t=(\d+)&amp;start=`)
)

// Release — всё, что достаём со страницы раздачи одним запросом.
// Гостю виден только магнит ГЛАВНОЙ раздачи темы (у постов-эпизодов магнитов
// нет), поэтому единица подписки — текущая раздача темы.
type Release struct {
	Hash   string
	Poster string
	Descr  string
	Tech   []TechField // «Производство/Жанр/Видео/Аудио…» со страницы раздачи
	// карточка страницы раздачи: рейтинг-картинка КП (через проки трекера)
	// и ссылки на Кинопоиск/IMDb/MyAnimeList из поста
	RatingImg  string
	KP         string
	IMDb       string
	IMDbRating string // через OMDb (если задан OMDB_APIKEY)
	IMDbVotes  string
	MAL        string // ссылка myanimelist.net/anime/<id> (аниме-карточки)
	MALRating  string // через Jikan (api.jikan.moe, без ключа)
	MALVotes   string
	TorName    string // имя torrent-файла (список файлов гостю скрыт)
	TorSize    string
}

// TechField — строка техданных раздачи («Жанр: комедия»)
type TechField struct {
	Name  string
	Value string
}

// TechString — значения полей одной строкой (для матчинга фильтров)
func (r Release) TechString() string {
	parts := make([]string, 0, len(r.Tech))
	for _, t := range r.Tech {
		parts = append(parts, t.Name+" "+t.Value)
	}
	return strings.Join(parts, " ")
}

// поля карточки — в порядке страницы; эти не показываем
var techSkip = map[string]bool{
	"Время раздачи": true, // служебное
}

// parseRelease — магнит, постер и описание со страницы темы
func parseRelease(body string) Release {
	var r Release
	if m := magnetRe.FindStringSubmatch(body); m != nil {
		r.Hash = m[1]
	}
	for _, m := range posterRe.FindAllStringSubmatch(body, 6) {
		u := m[1]
		bad := strings.Contains(u, "kinopoisk.ru/rating") || strings.Contains(u, "imdb") ||
			strings.Contains(u, "/channel/") || strings.HasSuffix(strings.ToLower(u), ".gif")
		if !bad && len(u) > 20 {
			r.Poster = u
			break
		}
	}
	// сюжет: полный, многоабзацный, абзацы сохраняем маркером ¶
	plot := ""
	if m := plotRe.FindStringSubmatch(body); m != nil {
		plot = m[1]
	} else if m := descrRe.FindStringSubmatch(body); m != nil {
		plot = m[1]
	}
	if plot != "" {
		plot = strings.ReplaceAll(plot, "<br", "¶<br") // абзацы переживают зачистку тегов
		plot = strings.TrimSpace(spaceRe.ReplaceAllString(tagRe.ReplaceAllString(html.UnescapeString(plot), " "), " "))
		plot = strings.ReplaceAll(plot, "¶ ", "¶")
		if len([]rune(plot)) > 2000 {
			plot = string([]rune(plot)[:2000]) + "…"
		}
		r.Descr = plot
	}

	// техполя — в порядке страницы; сюжет встаёт на своё место среди полей
	seen := map[string]bool{}
	for _, m := range techRe.FindAllStringSubmatch(body, 24) {
		name := strings.TrimSpace(m[1])
		if techSkip[name] || seen[name] {
			continue
		}
		val := strings.TrimSpace(spaceRe.ReplaceAllString(tagRe.ReplaceAllString(html.UnescapeString(m[2]), " "), " "))
		if val == "" && name != "Описание" {
			continue
		}
		if name == "Описание" {
			val = r.Descr // уже полный, с маркерами абзацев
		} else if len([]rune(val)) > 800 {
			val = string([]rune(val)[:800]) + "…"
		}
		seen[name] = true
		r.Tech = append(r.Tech, TechField{Name: name, Value: val})
	}

	// карточка: рейтинг КП (картинкой через проки трекера), ссылки, торрент
	if m := kpRatingRe.FindStringSubmatch(body); m != nil {
		r.RatingImg = "https://nnmstatic.win/forum/image.php?link=" + url.QueryEscape(m[1])
	}
	if m := kpLinkRe.FindStringSubmatch(body); m != nil {
		r.KP = m[1]
	}
	if m := imdbLinkRe.FindStringSubmatch(body); m != nil {
		r.IMDb = m[1]
	}
	if m := malLinkRe.FindStringSubmatch(body); m != nil {
		r.MAL = m[1]
	}
	if m := torNameRe.FindStringSubmatch(body); m != nil {
		r.TorName = m[1]
	}
	if m := torSizeRe.FindStringSubmatch(body); m != nil {
		r.TorSize = strings.ReplaceAll(strings.TrimSpace(m[1]), " ", " ")
	}
	return r
}

// resolveRelease — текущая раздача темы (гость). Кэшируется на TTL.
func resolveRelease(topicID int) (Release, error) {
	cacheKey := fmt.Sprintf("topic:%d", topicID)
	cacheMu.Lock()
	if e, ok := resolveCache[cacheKey]; ok && time.Since(e.ts) < time.Duration(ttl)*time.Second {
		cacheMu.Unlock()
		return e.rel, nil
	}
	cacheMu.Unlock()

	body, err := fetchNNM(fmt.Sprintf("%s/forum/viewtopic.php?t=%d", nnmBase, topicID), "")
	if err != nil {
		return Release{}, err
	}

	rel := parseRelease(body)

	cacheMu.Lock()
	resolveCache[cacheKey] = resolveEntry{ts: time.Now(), rel: rel}
	markCacheDirty()
	cacheMu.Unlock()
	return rel, nil
}

// staleResolve — последний известный резолв темы БЕЗ учёта TTL (персистентный
// кэш живёт 72 ч): когда nnm троттлит (HTTP 503), отдаём прошлую версию раздачи
// вместо выбрасывания item — клиент не теряет раздачу, апдейт доедет новым
// guid'ом, когда трекер снова ответит
func staleResolve(topicID int) (Release, bool) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	e, ok := resolveCache[fmt.Sprintf("topic:%d", topicID)]
	return e.rel, ok && e.rel.Hash != ""
}

var (
	omdbRateRe  = regexp.MustCompile(`"imdbRating":"([0-9.]+?)"`)
	omdbVotesRe = regexp.MustCompile(`"imdbVotes":"([0-9,]+?)"`)
)

// imdbRating — рейтинг IMDb через OMDb (ключ OMDB_APIKEY, бесплатный);
// пустой ключ → пустой результат; кэш на TTL
func imdbRating(tt string) (val, votes string) {
	if omdbKey == "" {
		return "", ""
	}
	cacheKey := "imdb:" + tt
	cacheMu.Lock()
	if e, ok := resolveCache[cacheKey]; ok && time.Since(e.ts) < time.Duration(ttl)*time.Second {
		cacheMu.Unlock()
		return e.rel.Hash, e.rel.Descr // рейтинг спрятан в Hash, голоса — в Descr
	}
	cacheMu.Unlock()

	req, _ := http.NewRequest("GET", "https://www.omdbapi.com/?i="+tt+"&apikey="+omdbKey, nil)
	resp, err := nnmClient.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	if m := omdbRateRe.FindStringSubmatch(body); m != nil {
		val = m[1]
	}
	if m := omdbVotesRe.FindStringSubmatch(body); m != nil {
		votes = m[1]
	}

	cacheMu.Lock()
	resolveCache[cacheKey] = resolveEntry{ts: time.Now(), rel: Release{Hash: val, Descr: votes}}
	markCacheDirty()
	cacheMu.Unlock()
	return val, votes
}

var (
	malIDRe    = regexp.MustCompile(`myanimelist\.net/anime/(\d+)`)
	jikanScore = regexp.MustCompile(`"score":([0-9]+(?:\.[0-9]+)?)`)
	jikanVotes = regexp.MustCompile(`"scored_by":(\d+)`)
)

// malRating — рейтинг MyAnimeList через Jikan (api.jikan.moe, ключ не нужен;
// лимит ~3 req/s — нам хватает, кэш на TTL). link — ссылка MAL со страницы
func malRating(link string) (val, votes string) {
	m := malIDRe.FindStringSubmatch(link)
	if m == nil {
		return "", ""
	}
	id := m[1]
	cacheKey := "mal:" + id
	cacheMu.Lock()
	if e, ok := resolveCache[cacheKey]; ok && time.Since(e.ts) < time.Duration(ttl)*time.Second {
		cacheMu.Unlock()
		return e.rel.Hash, e.rel.Descr
	}
	cacheMu.Unlock()

	req, _ := http.NewRequest("GET", "https://api.jikan.moe/v4/anime/"+id, nil)
	resp, err := nnmClient.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	if m := jikanScore.FindStringSubmatch(body); m != nil {
		val = m[1]
	}
	if m := jikanVotes.FindStringSubmatch(body); m != nil {
		votes = m[1]
	}

	cacheMu.Lock()
	resolveCache[cacheKey] = resolveEntry{ts: time.Now(), rel: Release{Hash: val, Descr: votes}}
	markCacheDirty()
	cacheMu.Unlock()
	return val, votes
}

var (
	// ключ клиента Lampa — публичный (как в tracker-top), свой — через env
	tmdbKey = env("TMDB_APIKEY", "4ef0d7355d9ffb5151e987764708ce96")
	tmdbAPI = "https://api.themoviedb.org/3/"
)

type tmdbHit struct {
	VoteAverage float64 `json:"vote_average"`
	VoteCount   int     `json:"vote_count"`
}

// tmdbRating — рейтинг TMDB для карточки разовой раздачи: по IMDb-id из
// страницы темы (/find — однозначный матч), иначе поиск по названию и году
// (фильм, затем сериал; русское имя, затем оригинал — как в поиске раздач).
// Кэш на TTL (вместе с «не нашли»), как у IMDb/MAL
func tmdbRating(imdbID, title string) (val, votes string) {
	cacheKey := "tmdb:" + imdbID + "|" + title
	cacheMu.Lock()
	if e, ok := resolveCache[cacheKey]; ok && time.Since(e.ts) < time.Duration(ttl)*time.Second {
		cacheMu.Unlock()
		return e.rel.Hash, e.rel.Descr
	}
	cacheMu.Unlock()

	if hit, ok := tmdbLookup(imdbID, title); ok && hit.VoteCount > 0 {
		val = strconv.FormatFloat(hit.VoteAverage, 'f', 1, 64)
		votes = groupDigits(hit.VoteCount)
	}

	cacheMu.Lock()
	resolveCache[cacheKey] = resolveEntry{ts: time.Now(), rel: Release{Hash: val, Descr: votes}}
	markCacheDirty()
	cacheMu.Unlock()
	return val, votes
}

func tmdbLookup(imdbID, title string) (tmdbHit, bool) {
	if imdbID != "" {
		if h, ok := tmdbFetch("find/"+imdbID, url.Values{
			"external_source": {"imdb_id"},
		}); ok {
			return h, true
		}
	}
	name, orig, year := nameOrigYear(title)
	for _, q := range []string{name, orig} {
		if len(q) < 2 {
			continue
		}
		for _, kind := range []string{"movie", "tv"} {
			v := url.Values{"query": {q}}
			if year != "" {
				// год у фильма и сериала — разные параметры
				if kind == "movie" {
					v.Set("year", year)
				} else {
					v.Set("first_air_date_year", year)
				}
			}
			if h, ok := tmdbFetch("search/"+kind, v); ok {
				return h, true
			}
		}
	}
	return tmdbHit{}, false
}

// tmdbFetch — GET к TMDB (language=ru): /find отдаёт movie_result/tv_result,
// /search/* — results; из списка берём первый результат с голосами
func tmdbFetch(endpoint string, q url.Values) (tmdbHit, bool) {
	q.Set("api_key", tmdbKey)
	q.Set("language", "ru")
	req, err := http.NewRequest("GET", tmdbAPI+endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return tmdbHit{}, false
	}
	req.Header.Set("User-Agent", nnmUA)
	resp, err := nnmClient.Do(req)
	if err != nil {
		return tmdbHit{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return tmdbHit{}, false
	}
	var body struct {
		Results      []tmdbHit `json:"results"`       // /search/*
		MovieResults []tmdbHit `json:"movie_results"` // /find — во множественном
		TvResults    []tmdbHit `json:"tv_results"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil {
		return tmdbHit{}, false
	}
	for _, list := range [][]tmdbHit{body.MovieResults, body.TvResults, body.Results} {
		if h, ok := tmdbPick(list); ok {
			return h, true
		}
	}
	return tmdbHit{}, false
}

// tmdbPick — первый результат с голосами: единичный голос даёт мусорные
// 10.0, поэтому сперва ищем ≥10 голосов, потом любой >0
func tmdbPick(list []tmdbHit) (tmdbHit, bool) {
	for _, h := range list {
		if h.VoteCount >= 10 {
			return h, true
		}
	}
	for _, h := range list {
		if h.VoteCount > 0 {
			return h, true
		}
	}
	return tmdbHit{}, false
}

var yearAnyRe = regexp.MustCompile(`\b((19|20)\d{2})\b`)

// nameOrigYear — «Курьер / Runner / 2026 / ДБ / WEB-DL (1080p)» → «Курьер»,
// «Runner», «2026» (поиск TMDB, как parseTitle в tracker-top)
func nameOrigYear(title string) (name, orig, year string) {
	if m := hashNameYearRe.FindStringSubmatch(title); m != nil {
		name, year = m[1], m[2]
	} else {
		// года нет — ищем просто по названию
		name = title
		if y := yearAnyRe.FindStringSubmatch(title); y != nil {
			year = y[1]
		}
	}
	// запасной сценарий отдаёт имя целиком: режем на год/скобки/слэши
	name = strings.Split(name, "(")[0]
	name = strings.TrimSpace(yearAnyRe.ReplaceAllString(name, ""))
	name = strings.TrimSpace(spaceRe.ReplaceAllString(name, " "))
	if segs := strings.Split(title, "/"); len(segs) > 1 {
		// оригинал — второй сегмент; год-сегмент (« / 2026 / ») и техданные мимо
		o := strings.TrimSpace(strings.Split(segs[1], "(")[0])
		if len(o) >= 2 && !strings.ContainsAny(o[:1], "0123456789") {
			orig = o
		}
	}
	if len([]rune(name)) < 2 {
		name = ""
	}
	return name, orig, year
}

// groupDigits — 12345 → «12 345» (голоса на карточке)
func groupDigits(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return s
}

// topicInfo — название, постер и id темы (для страницы подписок).
// Если передан пост (kind=post): достаём тему из заголовка страницы
func topicInfo(kind string, id int) (topicID int, title, poster string, err error) {
	if kind == "post" {
		body, ferr := fetchNNM(fmt.Sprintf("%s/forum/viewtopic.php?p=%d", nnmBase, id), "")
		if ferr != nil {
			return 0, "", "", ferr
		}
		if strings.TrimSpace(body) == "" {
			return 0, "", "", errDeepPost
		}
		if self := postTopicSelf(body); self != 0 {
			id = self
		} else {
			return 0, "", "", errDeepPost
		}
	}

	body, ferr := fetchNNM(fmt.Sprintf("%s/forum/viewtopic.php?t=%d", nnmBase, id), "")
	if ferr != nil {
		return 0, "", "", ferr
	}
	if m := titleTagRe.FindStringSubmatch(body); m != nil {
		t := html.UnescapeString(m[1])
		if i := strings.LastIndex(t, "::"); i >= 0 {
			t = t[:i]
		}
		title = strings.TrimSpace(t)
	}
	poster = parseRelease(body).Poster
	return id, title, poster, nil
}

var errDeepPost = errors.New("пост не открывается без раздела: откройте тему целиком и скопируйте ссылку с t=…")

// postTopicSelf — id темы, которой принадлежит пост, со страницы viewtopic?p=:
// сначала ссылка в заголовке (maintitle), затем пагинация (t=…&start=)
func postTopicSelf(body string) int {
	if tag := postSelfTitle.FindString(body); tag != "" {
		if m := linkTopicRe.FindStringSubmatch(tag); m != nil {
			return atoiDefault(m[1])
		}
	}
	if m := postSelfPage.FindStringSubmatch(body); m != nil {
		return atoiDefault(m[1])
	}
	return 0
}

// magnetLink — магнит ленты: только btih, без announce. На nnm ключи минтятся
// сайтом на каждое скачивание (в магнит/.torrent залогиненного), константный
// ключ ленты трекер не принимает — пиры клиент находит через DHT/PEX
func magnetLink(hash string) string {
	return "magnet:?xt=urn:btih:" + hash
}

// ---------- топ трекера (популярное за N дней)

// корневые разделы «видео» на NNMClub (как в tracker-top: мусор отсекаем)
var nnmVideoRoots = map[int]bool{
	216: true, 318: true, 220: true, 224: true, 1311: true, 256: true, 264: true, // кино
	1219: true, 768: true, 769: true, 713: true, // сериалы
	576: true,                       // документалистика
	724: true,                       // детское видео/мультфильмы
	620: true, 624: true, 628: true, // аниме
}

var (
	trTdRe     = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
	trForumRe  = regexp.MustCompile(`href="tracker\.php\?f=(\d+)"[^>]*>([^<]+)`)
	trTitleBRe = regexp.MustCompile(`(?s)href="viewtopic\.php\?t=\d+"[^>]*><b>(.*?)</b>`)
	trAddedRe  = regexp.MustCompile(`(\d{9,10})`)
	trOptionRe = regexp.MustCompile(`<option[^>]+value="(\d+)"[^>]*>([^<]*)</option>`)
)

type topRow struct {
	TopicID int
	Title   string
	ForumID int
	Added   int64 // unix
}

// parseTrackerTop — строки топа трекера (как parseNNM в tracker-top, только нужное)
func parseTrackerTop(body string) []topRow {
	var out []topRow
	for _, seg := range strings.Split(body, "</tr>") {
		if !strings.Contains(seg, "viewtopic.php?t=") || !trTitleBRe.MatchString(seg) {
			continue
		}
		cells := trTdRe.FindAllStringSubmatch(seg, -1)
		if len(cells) < 9 {
			continue
		}

		idm := linkTopicRe.FindStringSubmatch(seg)
		if idm == nil {
			continue
		}
		row := topRow{TopicID: atoiDefault(idm[1])}
		if fm := trForumRe.FindStringSubmatch(cells[1][1]); fm != nil {
			row.ForumID = atoiDefault(fm[1])
		}
		// последняя ячейка: завершённость + время добавления
		last := cells[len(cells)-1][1]
		if am := trAddedRe.FindStringSubmatch(last); am != nil {
			row.Added = int64(atoiDefault(am[1]))
		}
		if tm := trTitleBRe.FindStringSubmatch(cells[2][1]); tm != nil {
			row.Title = html.UnescapeString(stripTags(tm[1]))
		}
		if row.TopicID > 0 && row.Title != "" {
			out = append(out, row)
		}
	}
	return out
}

// nnmVideoSubtree — поддерево видео-разделов по списку категорий со страницы
// (топ-уровень без "|-", подфорумы с ним; берём всё под видео-корнями)
func nnmVideoSubtree(body string) map[int]bool {
	return subtreeFor(body, nnmVideoRoots)
}

// Category — верхний уровень разделов трекера (для галок настройки топа)
type Category struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Primary bool   `json:"primary"`
}

// categorySelect — содержимое селекта разделов трекера (<select name="f[]">);
// на странице есть и другие селекты (сортировка, «золотые раздачи», колонки) —
// их опции мусорные и к разделам не относятся
var catSelRe = regexp.MustCompile(`(?s)<select[^>]*name="f\[\]"[^>]*>(.*?)</select>`)

func categorySelect(body string) string {
	return catSelRe.FindStringSubmatch(body)[1]
}

// parseTrackerCategories — верхний уровень: опции без "|-";
// Primary = видео и «горячие новинки» — в UI показываются первыми
func parseTrackerCategories(body string) []Category {
	var out []Category
	for _, m := range trOptionRe.FindAllStringSubmatch(categorySelect(body), -1) {
		id := atoiDefault(m[1])
		text := html.UnescapeString(m[2])
		if !strings.Contains(text, "|-") {
			name := strings.TrimSpace(strings.ReplaceAll(text, " ", " "))
			out = append(out, Category{
				ID:      id,
				Name:    name,
				Primary: nnmVideoRoots[id] || strings.Contains(name, "Горячие новинки"),
			})
		}
	}
	return out
}

// leafRoots — каждый подфорум → его корневой раздел; корневая лента rss.php
// пуста, «Лента разделов» подписывается листьями, группируя их по корню
func leafRoots(body string) map[int]Category {
	out := map[int]Category{}
	var cur Category
	for _, m := range trOptionRe.FindAllStringSubmatch(categorySelect(body), -1) {
		id := atoiDefault(m[1])
		text := strings.TrimSpace(strings.ReplaceAll(html.UnescapeString(m[2]), " ", " "))
		if !strings.Contains(text, "|-") {
			cur = Category{ID: id, Name: text}
			continue
		}
		if cur.ID != 0 && id != cur.ID {
			out[id] = cur
		}
	}
	return out
}

// leafNames — id → имя всех разделов селекта (для названий подписок-листьев)
func leafNames(body string) map[int]string {
	out := map[int]string{}
	for _, m := range trOptionRe.FindAllStringSubmatch(categorySelect(body), -1) {
		out[atoiDefault(m[1])] = strings.TrimSpace(strings.ReplaceAll(html.UnescapeString(m[2]), " ", " "))
	}
	return out
}

// subtreeFor — все разделы под выбранными корнями (корень + его подфорумы
// с "|-" до следующего корня)
func subtreeFor(body string, roots map[int]bool) map[int]bool {
	ids := map[int]bool{}
	inRoot := false
	for _, m := range trOptionRe.FindAllStringSubmatch(categorySelect(body), -1) {
		id := atoiDefault(m[1])
		text := html.UnescapeString(m[2])
		if !strings.Contains(text, "|-") {
			inRoot = roots[id]
		}
		if inRoot {
			ids[id] = true
		}
	}
	return ids
}

// ---------- дедуп топа по фильму

var (
	filmYearRe  = regexp.MustCompile(`\((\d{4})`)
	filmBaseRe  = regexp.MustCompile(`[(\[]`)
	filmQ2160Re = regexp.MustCompile(`(?i)2160p?|4k|uhd`)
	filmQ1080Re = regexp.MustCompile(`(?i)1080p?i?`)
	filmQ720Re  = regexp.MustCompile(`(?i)720p?i?`)
)

// filmKey — «Название / Original (Год) …» → ключ фильма: разные темы одной
// раздачи (720p/1080p/4K, варианты перевода названия) схлопываются
func filmKey(title string) string {
	base := filmBaseRe.Split(title, 2)[0]

	var parts []string
	for _, p := range strings.Split(base, "/") {
		if p = strings.TrimSpace(strings.Trim(p, " -")); p != "" {
			parts = append(parts, p)
		}
	}

	ru, orig := "", ""
	if len(parts) > 0 {
		ru = parts[0]
	}
	if len(parts) > 1 {
		orig = parts[len(parts)-1]
	}

	year := ""
	if m := filmYearRe.FindStringSubmatch(title); m != nil {
		year = m[1]
	}

	return strings.ToLower(ru+"|"+orig) + "|" + year
}

// filmQualityRank — sd=0 < 720 < 1080 < 2160
func filmQualityRank(title string) int {
	switch {
	case filmQ2160Re.MatchString(title):
		return 3
	case filmQ1080Re.MatchString(title):
		return 2
	case filmQ720Re.MatchString(title):
		return 1
	}
	return 0
}

// dedupeTopRows — один фильм = одна тема: лучшая по качеству,
// при равенстве — самая свежая («рядом или через одну» больше не будет)
func dedupeTopRows(rows []topRow) []topRow {
	type slot struct {
		row  topRow
		rank int
	}
	best := map[string]*slot{}
	for _, r := range rows {
		key := filmKey(r.Title)
		rank := filmQualityRank(r.Title)
		if s, ok := best[key]; !ok || rank > s.rank || (rank == s.rank && r.Added > s.row.Added) {
			if !ok {
				best[key] = &slot{}
			}
			best[key].row = r
			best[key].rank = rank
		}
	}
	out := make([]topRow, 0, len(best))
	for _, s := range best {
		out = append(out, s.row)
	}
	return out
}

// hasCyrillic — есть ли в строке русские буквы
func hasCyrillic(s string) bool {
	for _, r := range s {
		if (r >= 'А' && r <= 'я') || r == 'Ё' || r == 'ё' {
			return true
		}
	}
	return false
}

func stripTags(s string) string {
	return strings.TrimSpace(spaceRe.ReplaceAllString(tagRe.ReplaceAllString(s, " "), " "))
}

// OptVal — значение опции-фильтра трекера
type OptVal struct {
	Value int    `json:"value"`
	Name  string `json:"name"`
}

var selByNameRe = regexp.MustCompile(`(?s)<select[^>]*name="([a-z\[\]]+)"[^>]*>(.*?)</select>`)

// trackerOptions — опции селектов страницы трекера по имени; полезны как
// фильтры (sds = тип раздачи: золотые/серебряные…, tm = окно времени)
func trackerOptions(body, name string) []OptVal {
	var out []OptVal
	for _, m := range selByNameRe.FindAllStringSubmatch(body, -1) {
		if m[1] != name {
			continue
		}
		for _, o := range trOptionRe.FindAllStringSubmatch(m[2], -1) {
			v := atoiDefault(strings.ReplaceAll(o[1], "[]", ""))
			if v < 0 {
				continue // «не учитывать» и прочие отключенные значения
			}
			name := strings.ReplaceAll(html.UnescapeString(o[2]), " ", " ")
			out = append(out, OptVal{Value: v, Name: stripTags(name)})
		}
	}
	return out
}

// ---------- подписки: URL → вид + id

var (
	urlTopicRe = regexp.MustCompile(`viewtopic\.php\?[^#]*\bt=(\d+)`)
	urlPostRe  = regexp.MustCompile(`viewtopic\.php\?[^#]*\bp=(\d+)`)
	urlForumRe = regexp.MustCompile(`(?:tracker|viewforum)\.php\?[^#]*\bf=(\d+)`)
)

// parseNNMURL — из вставленной человеком ссылки (тема, пост или раздел трекера)
func parseNNMURL(u string) (kind string, id int) {
	if m := urlPostRe.FindStringSubmatch(u); m != nil {
		return "post", atoiDefault(m[1]) // пост сконвертируем в тему при добавлении
	}
	if m := urlTopicRe.FindStringSubmatch(u); m != nil {
		return "topic", atoiDefault(m[1])
	}
	if m := urlForumRe.FindStringSubmatch(u); m != nil {
		return "forum", atoiDefault(m[1])
	}
	return "", 0
}

// parseMagnet — btih из магнета (hex или base32 — нормализуем в hex40) и
// название из dn=. Разовая раздача из плагина Lampa приходит именно магнетом.
func parseMagnet(u string) (hash, name string) {
	if !strings.HasPrefix(strings.ToLower(u), "magnet:") {
		return "", ""
	}
	i := strings.IndexByte(u, '?')
	if i < 0 {
		return "", ""
	}
	q, err := url.ParseQuery(u[i+1:])
	if err != nil {
		return "", ""
	}
	for _, xt := range q["xt"] {
		v := strings.ToLower(strings.TrimSpace(xt))
		if !strings.HasPrefix(v, "urn:btih:") {
			continue
		}
		btih := v[len("urn:btih:"):]
		if len(btih) == 40 && strings.Trim(btih, "0123456789abcdef") == "" {
			return btih, q.Get("dn")
		}
		if len(btih) == 32 && strings.Trim(btih, "abcdefghijklmnopqrstuvwxyz234567") == "" {
			if raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(btih)); err == nil && len(raw) == 20 {
				return hex.EncodeToString(raw), q.Get("dn")
			}
		}
	}
	return "", ""
}

// magnetPageURL — страница раздачи на трекере из магнета парсера: nnm-магниты
// несут её в tr=retracker…&comment=viewtopic.php%3Fp%3D… — кодирование
// вложенное, декодируем в несколько проходов и ищем ссылку на тему.
// Есть не всегда — раздача может быть не с NNM.
var magnetPageRe = regexp.MustCompile(`https?://[^\s"'<>]+viewtopic\.php\?[^\s"'<>]*\b[pt]=\d+`)

func magnetPageURL(u string) string {
	if !strings.HasPrefix(strings.ToLower(u), "magnet:") {
		return ""
	}
	dec := u
	for i := 0; i < 3; i++ {
		n, err := url.QueryUnescape(dec)
		if err != nil || n == dec {
			break
		}
		dec = n
	}
	return magnetPageRe.FindString(dec)
}

// subRSSURL — адрес родной ленты подписки
func subRSSURL(s *Sub) string {
	if s.Kind == "forum" {
		return fmt.Sprintf("%s/forum/rss.php?f=%d&t=1&c=50", nnmBase, s.NNMID) // новые раздачи раздела
	}
	return fmt.Sprintf("%s/forum/rss.php?topic=%d&c=50", nnmBase, s.NNMID) // новые посты темы
}

func atoiDefault(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
