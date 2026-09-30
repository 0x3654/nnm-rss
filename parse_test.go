package main

import (
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("tests/" + name)
	if err != nil {
		t.Fatal(err)
	}
	// фикстуры — сырые cp1251-страницы, гоняем через тот же декодер, что и вживую
	return decodeBody(b)
}

var fails int

func check(t *testing.T, name string, cond bool, extra ...any) {
	t.Helper()
	if !cond {
		fails++
		t.Errorf("FAIL %s %v", name, extra)
	}
}

// ---------- родной rss.php

func TestParseRSSForum(t *testing.T) {
	body := fixture(t, "nnm_rss_f270.xml")

	ch := parseRSSChannel(body)
	check(t, "канал — название раздела", ch != "" && ch != "NNM-Club", ch)

	items := parseRSSItems(body)
	check(t, "items >= 2", len(items) >= 2, len(items)) // в разделе на момент снимка было 2 новых раздачи

	var sawTopic bool
	for _, it := range items {
		check(t, "id", it.ID > 0, it)
		check(t, "kind", it.Kind == "topic" || it.Kind == "post", it.Kind)
		check(t, "дата", !it.Date.IsZero(), it.Title)
		if it.Kind == "topic" {
			sawTopic = true
		}
	}
	check(t, "есть темы-раздачи", sawTopic)

	if len(items) > 0 {
		title := cleanTitle(items[0].Title)
		check(t, "cleanTitle без «Раздел ::»", !strings.Contains(title, "::"), title)
	}
}

func TestCleanTitle(t *testing.T) {
	check(t, "раздел", cleanTitle("Отечественные Новинки (SD, DVD) :: Касса невест (2025) WEBRip") ==
		"Касса невест (2025) WEBRip")
	check(t, "RE", cleanTitle("Раздел :: RE: Сериал (2026) S01") == "Сериал (2026) S01")
}

// ---------- страницы тем → Release (магнит, постер, описание)

func TestGuestReleaseOnHiddenDownload(t *testing.T) {
	body := fixture(t, "nnm_topic.html") // «Скачать» гостю скрыт, магнит — виден
	m := titleTagRe.FindStringSubmatch(body)
	check(t, "title есть", m != nil)
	check(t, "тема", strings.Contains(m[1], "Касса невест"), m[1])

	rel := parseRelease(body)
	check(t, "гость видит магнит", rel.Hash == "21F3A8C82E2D2375DD337D06C40D73C7A9D09B60", rel.Hash)
	check(t, "постер найден (не рейтинг КП)",
		rel.Poster != "" && !strings.Contains(rel.Poster, "kinopoisk.ru/rating"), rel.Poster)
	check(t, "описание найдено",
		strings.Contains(rel.Descr, "Недалекое будущее"), rel.Descr)

	// техданные раздачи
	tech := map[string]string{}
	for _, f := range rel.Tech {
		tech[f.Name] = f.Value
	}
	check(t, "жанр", strings.Contains(tech["Жанр"], "фантастика"), tech["Жанр"])
	check(t, "режиссер", strings.Contains(tech["Режиссер"], "Бальтцер"), tech["Режиссер"])
	check(t, "видео", strings.Contains(tech["Видео"], "AVC"), tech["Видео"])
	check(t, "аудио", strings.Contains(tech["Аудио"], "AC3"), tech["Аудио"])
	check(t, "описание в техполях на своём месте", strings.Contains(tech["Описание"], "Недалекое будущее"), tech["Описание"][:60])
	check(t, "tech string для фильтров", strings.Contains(rel.TechString(), "AVC"))
}

// ---------- топ трекера (популярное за N дней)

func TestParseTrackerTop(t *testing.T) {
	body := fixture(t, "nnm_tracker_o10.html")

	rows := parseTrackerTop(body)
	check(t, "строк >= 40", len(rows) >= 40, len(rows))
	if len(rows) == 0 {
		return
	}
	check(t, "id и название", rows[0].TopicID > 0 && rows[0].Title != "", rows[0])
	check(t, "время добавления", rows[0].Added > 1_600_000_000, rows[0].Added)

	video := nnmVideoSubtree(body)
	check(t, "видео-поддерево непустое", len(video) > 10, len(video))

	cats := parseTrackerCategories(body)
	check(t, "верхние разделы", len(cats) >= 10, len(cats))
	for _, c := range cats {
		check(t, "имя без |-", !strings.Contains(c.Name, "|-"), c.Name)
		// мусорные опции чужих селектов (сортировки, медальки) не проходят
		check(t, "не мусор", !strings.Contains(strings.ToLower(c.Name), "seeders") &&
			!strings.Contains(strings.ToLower(c.Name), "золот"), c.Name)
	}

	// опции-фильтры: тип раздачи (sds) — отдельным списком
	types := trackerOptions(body, "sds")
	check(t, "5 типов раздач", len(types) == 5, len(types))
	if len(types) == 5 {
		check(t, "обычные..платиновые", strings.Contains(types[0].Name, "обычн") &&
			strings.Contains(types[1].Name, "золот") && strings.Contains(types[4].Name, "платин"),
			types[0].Name, types[1].Name, types[4].Name)
	}
	// сортировки и окна времени не попадают в sds
	check(t, "sds без сортировок", len(trackerOptions(body, "o")) >= 10)
}

// ---------- магнит: btih без трекеров (DHT)

func TestMagnetLink(t *testing.T) {
	m := magnetLink("21F3A8C82E2D2375DD337D06C40D73C7A9D09B60")
	check(t, "ровно btih", m == "magnet:?xt=urn:btih:21F3A8C82E2D2375DD337D06C40D73C7A9D09B60", m)
	check(t, "без трекеров", !strings.Contains(m, "tr=") && !strings.Contains(m, "announce"), m)
}

// ---------- подписки: URL → вид + id

func TestParseNNMURL(t *testing.T) {
	cases := []struct {
		url  string
		kind string
		id   int
	}{
		{"https://nnmclub.to/forum/viewtopic.php?t=830137", "topic", 830137},
		{"https://nnmclub.to/forum/viewtopic.php?p=13108518#13108518", "post", 13108518},
		{"https://nnmclub.to/forum/tracker.php?f=270", "forum", 270},
		{"https://nnmclub.to/forum/viewforum.php?f=954", "forum", 954},
		{"https://example.com/nothing", "", 0},
	}
	for _, c := range cases {
		kind, id := parseNNMURL(c.url)
		check(t, c.url, kind == c.kind && id == c.id, kind, id)
	}
}

// p= ссылка резолвится в свою тему: виджет благодарностей под каждым постом
// ведёт в служебную «Доску почета» и по частоте побеждал бы саму тему
// (репро p=13119948 → t=1891932, 13 ссылок t=15061 против 7 своих)
func TestPostTopicSelf(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<h1><a class="maintitle" href="viewtopic.php?t=1891932">Унабомбер / Unabomber (2026)</a></h1>`)
	for i := 0; i < 13; i++ {
		b.WriteString(`<div><a href="viewtopic.php?t=15061" rel="nofollow"><img src="5star.gif"></a></div>`)
	}
	b.WriteString(`Страницы: <a href="viewtopic.php?t=1891932&amp;start=15#pagestart">2</a>`)
	check(t, "maintitle бьёт частоту благодарностей", postTopicSelf(b.String()) == 1891932)

	page := `<a href="viewtopic.php?t=1891932&amp;start=15#pagestart">2</a>` +
		`<a href="viewtopic.php?t=1891932&amp;start=30#pagestart">След.</a>`
	check(t, "без maintitle — тема из пагинации", postTopicSelf(page) == 1891932)

	check(t, "без своей темы — 0, а не угаданная", postTopicSelf(`<a href="viewtopic.php?t=15061">x</a>`) == 0)
}

// ---------- gid ↔ (kind, id)

func TestGidParse(t *testing.T) {
	check(t, "topic", k2kind("topic1888923") == "topic" && k2id("topic1888923") == 1888923)
	check(t, "post", k2kind("post13108518") == "post" && k2id("post13108518") == 13108518)
}

func TestFilmKeyAndQuality(t *testing.T) {
	// один фильм под разными русскими названиями и качествами — один ключ
	a := filmKey("Мэйдэй / Mayday (2026) WEB-DL [H.264/1080p]")
	b := filmKey("Мэйдэй / Сигнал бедствия / Mayday (2026) WEB-DL [H.264/1080p]")
	c := filmKey("Моана / Moana (2026) WEB-DL [H.265/2160p] [4K, HDR10, DV 8, 10-bit]")
	d := filmKey("Моана / Moana (2026) WEB-DL [H.264/720p]")
	if a != b {
		t.Errorf("filmKey: вариант перевода названия должен склеиваться: %q != %q", a, b)
	}
	if c != d {
		t.Errorf("filmKey: разные качества одного фильма должны склеиваться: %q != %q", c, d)
	}
	if a == c {
		t.Errorf("filmKey: разные фильмы не должны склеиваться")
	}

	ranks := map[string]int{
		"X (2026) WEB-DL [H.265/2160p] [4K]": 3,
		"X (2026) WEBRip [H.264/1080p]":      2,
		"X (2026) WEB-DL [H.264/720p]":       1,
		"X (2026) WEB-DLRip":                 0,
	}
	for title, want := range ranks {
		if got := filmQualityRank(title); got != want {
			t.Errorf("filmQualityRank(%q) = %d, хочу %d", title, got, want)
		}
	}
}

func TestDedupeTopRows(t *testing.T) {
	rows := []topRow{
		{TopicID: 1, Title: "Моана / Moana (2026) WEB-DL [H.264/720p]", Added: 200},
		{TopicID: 2, Title: "Моана / Moana (2026) WEB-DL [H.264/1080p]", Added: 100},
		{TopicID: 3, Title: "Моана / Moana (2026) WEB-DL [H.265/2160p] [4K, SDR]", Added: 150},
		{TopicID: 4, Title: "Мэйдэй / Mayday (2026) WEB-DL [H.264/1080p]", Added: 300},
		{TopicID: 5, Title: "Мэйдэй / Сигнал бедствия / Mayday (2026) WEB-DL [H.265/2160p]", Added: 280},
		{TopicID: 6, Title: "Другой фильм (2026) WEBRip [H.264/1080p]", Added: 400},
	}
	out := dedupeTopRows(rows)
	if len(out) != 3 {
		t.Fatalf("дедуп: %d позиций, хочу 3 (Моана, Мэйдэй, Другой)", len(out))
	}
	byTitle := map[int]bool{}
	for _, r := range out {
		byTitle[r.TopicID] = true
	}
	// Моана: лучшее качество 2160 → тема 3; Мэйдэй: 2160 → тема 5
	if !byTitle[3] || !byTitle[5] || !byTitle[6] {
		t.Errorf("дедуп выбрал не лучшие раздачи: %+v", byTitle)
	}
	if byTitle[1] || byTitle[2] || byTitle[4] {
		t.Errorf("дедуп оставил лишние темы: %+v", byTitle)
	}
}

// ---------- разовые раздачи: магнет → hash-подписка

func TestParseMagnet(t *testing.T) {
	// btih того же хэша в base32: 21F3A8C8… → "BIPGCDSMZZF3…" посчитаем честно
	hexH := "21f3a8c82e2d2375dd337d06c40d73c7a9d09b60"

	cases := []struct {
		url  string
		hash string
		name string
	}{
		{"magnet:?xt=urn:btih:" + hexH, hexH, ""},
		{"magnet:?xt=urn:btih:" + strings.ToUpper(hexH) + "&dn=Movie+2026", hexH, "Movie 2026"},
		{"magnet:?dn=First&xt=urn:btih:" + hexH, hexH, "First"},
		{"magnet:?xt=urn:sha1:AAAABBBBCCCCDDDD", "", ""},                   // не btih
		{"magnet:?xt=urn:btih:12345", "", ""},                              // короткий
		{"https://nnmclub.to/forum/viewtopic.php?t=1", "", ""},             // не магнет
		{"magnet:?xt=urn:btih:ehz2rsbofurxlxjtpudmidlty6u5bg3a", hexH, ""}, // base32
	}
	for _, c := range cases {
		hash, name := parseMagnet(c.url)
		check(t, c.url, hash == c.hash && name == c.name, hash, name)
	}
}

func TestBuildFeedHashSub(t *testing.T) {
	// профиль только с hash-подпиской: сети нет, selfURL пуст — link = магнет
	p := &Profile{Passkey: randHex(16)}
	p.Subs = []*Sub{{
		ID: "ab12", Kind: "hash", Hash: "21f3a8c82e2d2375dd337d06c40d73c7a9d09b60",
		Title: "Movie 2026 WEB-DL", Auto: true, Enabled: true,
		Added: time.Now().Add(-time.Hour),
	}}

	body, hist, err := buildFeed(p, false)
	if err != nil {
		t.Fatal(err)
	}
	check(t, "авто: один item", len(hist) == 1, len(hist))
	check(t, "guid с хэшем", hist[0].GUID == "nnm-hash21f3a8c82e2d2375dd337d06c40d73c7a9d09b60", hist[0].GUID)
	check(t, "link — магнет", strings.HasPrefix(hist[0].URL, "magnet:?xt=urn:btih:21f3a8"), hist[0].URL)
	check(t, "название", hist[0].Title == "Movie 2026 WEB-DL", hist[0].Title)
	check(t, "item в XML", strings.Contains(string(body), "nnm-hash21f3a8c8"), len(body))

	// общая лента (/all) разовых раздач не содержит
	_, histAll, err := buildFeed(p, true)
	if err != nil {
		t.Fatal(err)
	}
	check(t, "all: пусто", len(histAll) == 0, len(histAll))

	// фильтр auto на разовую раздачу не действует (явное желание юзера)
	p.Filters = map[string]*FeedFilter{"auto": {Exclude: "WEB-DL"}}
	cacheMu.Lock()
	feedCache = map[string]feedCacheEntry{}
	cacheMu.Unlock()
	_, hist2, _ := buildFeed(p, false)
	check(t, "фильтр не гасит hash-айтем", len(hist2) == 1, len(hist2))
}

// ---------- страница раздачи /t/<hash>

func TestTorrentPage(t *testing.T) {
	old := os.Getenv("DATA_DIR")
	os.Setenv("DATA_DIR", t.TempDir())
	t.Cleanup(func() { os.Setenv("DATA_DIR", old) })
	initTorrents()

	h := "47064afc564cca0128dda85b98ecfb517a9ad638"

	// без файла: страница-ожидание с авторефрешем; инфа от парсера — в тексте
	p := torrentPageHTML(&Sub{Kind: "hash", Hash: h, Title: "Runner.2026",
		Tracker: "nnm-club", Size: "7.2 ГБ", Added: time.Now()})
	check(t, "ожидание: заголовок", strings.Contains(p, "Runner.2026"))
	check(t, "ожидание: авторефреш", strings.Contains(p, "http-equiv=\"refresh\""))
	check(t, "ожидание: магнет с трекерами", strings.Contains(p, "magnet:?xt=urn:btih:"+h+"&tr="))
	check(t, "ожидание: трекер и размер от парсера", strings.Contains(p, "nnm-club · 7.2 ГБ"))

	// минимальный валидный .torrent (один файл)
	info := map[string]any{"name": "Runner.2026.mkv", "length": int64(1234567),
		"piece length": 262144, "pieces": strings.Repeat("x", 20)}
	ib, _ := bencode.Marshal(info)
	raw, _ := bencode.Marshal(metainfo.MetaInfo{InfoBytes: ib})
	if err := os.WriteFile(torrentPath(h), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	p = torrentPageHTML(&Sub{Kind: "hash", Hash: h, Title: "Runner.2026 (из Lampa)",
		Page: "https://nnmclub.to/forum/viewtopic.php?t=123"})
	check(t, "готово: не ждёт", !strings.Contains(p, "ещё собирает"))
	check(t, "готово: h1 — настоящее имя раздачи", strings.Contains(p, "<h1>Runner.2026.mkv</h1>"))
	check(t, "готово: файл с размером", strings.Contains(p, "Runner.2026.mkv") && strings.Contains(p, "1.2 МБ"))
	check(t, "готово: ссылка на .torrent", strings.Contains(p, "/t/"+h+".torrent"))
	check(t, "готово: ссылка на трекер", strings.Contains(p, "viewtopic.php?t=123"))
}

func TestMagnetPageURL(t *testing.T) {
	// nnm-магнит парсера: viewtopic прячется в tr=retracker…&comment=… (вложенное кодирование)
	nnm := "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20) +
		"&tr=" + url.QueryEscape("http://retracker.local/announce.php?size=6111021357&comment=http%3A%2F%2Fnnmclub.to%2Fforum%2Fviewtopic.php%3Fp%3D13105047&name=x")
	got := magnetPageURL(nnm)
	check(t, "viewtopic из tr-comment", strings.Contains(got, "http://nnmclub.to/forum/viewtopic.php?p=13105047"), got)

	pub := "magnet:?xt=urn:btih:" + strings.Repeat("cd", 20) + "&tr=" + url.QueryEscape("udp://tracker.opentrackr.org:1337/announce")
	check(t, "без viewtopic — пусто", magnetPageURL(pub) == "")

	check(t, "не магнет — пусто", magnetPageURL("https://x/y") == "")
}

// ---------- рейтинги TMDB (карточка /t/<hash>)

func TestNameOrigYear(t *testing.T) {
	cases := []struct{ in, name, orig, year string }{
		// dn плагина Lampa: имя / оригинал / год / техданные
		{"Курьер / Runner / 2026 / ДБ / WEB-DL (1080p)", "Курьер", "Runner", "2026"},
		{"Пастор и рыжий пес / A Pastor and a Red Dog / 2025 / ДБ, ПД / WEB-DL (1080p)", "Пастор и рыжий пес", "A Pastor and a Red Dog", "2025"},
		// название темы NNM: год в скобках
		{"Заложница / Taken (2008) BDRip", "Заложница", "Taken", "2008"},
		// без оригинала и без года
		{"Просто название", "Просто название", "", ""},
		// год-сегмент не применим за оригинал
		{"Сериал / 2024 / ДБ / HDTVRip", "Сериал", "", "2024"},
	}
	for _, c := range cases {
		name, orig, year := nameOrigYear(c.in)
		check(t, "name «"+c.in+"»", name == c.name, name)
		check(t, "orig «"+c.in+"»", orig == c.orig, orig)
		check(t, "year «"+c.in+"»", year == c.year, year)
	}
}

func TestTmdbPick(t *testing.T) {
	list := []tmdbHit{{VoteAverage: 10, VoteCount: 1}, {VoteAverage: 6.8, VoteCount: 345}}
	h, ok := tmdbPick(list)
	check(t, "первый с ≥10 голосов", ok && h.VoteCount == 345)

	h, ok = tmdbPick([]tmdbHit{{VoteAverage: 7, VoteCount: 2}})
	check(t, "нет массовых — берём любой с голосами", ok && h.VoteCount == 2)

	_, ok = tmdbPick([]tmdbHit{{VoteAverage: 0, VoteCount: 0}})
	check(t, "без голосов — пусто", !ok)
	_, ok = tmdbPick(nil)
	check(t, "пустой список — пусто", !ok)
}

func TestGroupDigits(t *testing.T) {
	check(t, "12345", groupDigits(12345) == "12 345")
	check(t, "999", groupDigits(999) == "999")
	check(t, "1 000 000", groupDigits(1000000) == "1 000 000")
}
