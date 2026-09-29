package main

// DHT-резолвер: голый btih из ленты → настоящий .torrent (BEP9 ut_metadata).
// Зачем: nasctl и подобные RSS-клиенты берут <link> только как http(s)-.torrent,
// magnet в link игнорируют (пустой диалог добавления); эталон lostfilmfeed
// отдаёт именно .torrent-ссылки. Метаданные хэша неизменны — файлы кэшируются
// навсегда в DATA_DIR/torrents (том /data).

import (
	"fmt"
	"html"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

var (
	torrentsDir     string
	selfURL         = env("PUBLIC_URL", "") // http(s)-база сервиса для link на .torrent
	torrentFetchMu  sync.Mutex
	torrentFetching = map[string]bool{}
)

// публичные трекеры вписываем в собранный .torrent: магниты лент — btih без tr=
var publicTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://open.tracker.cl:6969/announce",
	"udp://explodie.org:6969/announce",
}

func initTorrents() {
	torrentsDir = filepath.Join(env("DATA_DIR", "data"), "torrents")
	os.MkdirAll(torrentsDir, 0o755)
}

func torrentPath(hash string) string {
	return filepath.Join(torrentsDir, strings.ToLower(hash)+".torrent")
}

func torrentReady(hash string) bool {
	if hash == "" {
		return false
	}
	st, err := os.Stat(torrentPath(hash))
	return err == nil && st.Size() > 0
}

// prefetchTorrent — фон; к следующему опросу ленты файл появится и link станет .torrent.
// Префетчей мало параллельно: каждый — отдельный torrent-клиент с сокетами.
var prefetchSem = make(chan struct{}, 3)

// waitForTorrent — короткое ожидание готовности файла при сборке ленты:
// свежий айтем чаще всего уезжает клиенту уже с .torrent-ссылкой (nasctl
// заполняет поле URL в «New Task» только http-ссылками, магнет игнорирует)
func waitForTorrent(hash string, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if torrentReady(hash) {
			return
		}
		time.Sleep(400 * time.Millisecond)
	}
}

func prefetchTorrent(hash string) {
	if selfURL == "" || torrentReady(hash) {
		return
	}
	torrentFetchMu.Lock()
	if torrentFetching[hash] {
		torrentFetchMu.Unlock()
		return
	}
	torrentFetching[hash] = true
	torrentFetchMu.Unlock()
	go func() {
		defer func() {
			torrentFetchMu.Lock()
			delete(torrentFetching, hash)
			torrentFetchMu.Unlock()
		}()
		prefetchSem <- struct{}{}
		defer func() { <-prefetchSem }()
		fetchTorrentFile(hash)
	}()
}

// fetchTorrentFile — достаёт метаданные по DHT и сохраняет .torrent
func fetchTorrentFile(hash string) {
	hash = strings.ToLower(hash)
	tmp := filepath.Join(torrentsDir, "_tmp_"+hash[:8])
	os.MkdirAll(tmp, 0o755)
	// конфиг только через NewDefaultClientConfig: литерал без дефолтов паникует
	// в v1.58.0 (nil ListenHost — listenAll, nil DhtStartingNodes — NewAnacrolixDhtServer)
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = tmp
	cfg.ListenPort = 0
	cfg.NoDefaultPortForwarding = true
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		log.Printf("torrent client: %v", err)
		return
	}
	defer cl.Close()
	defer os.RemoveAll(tmp)
	t, err := cl.AddMagnet("magnet:?xt=urn:btih:" + hash)
	if err != nil {
		log.Printf("addmagnet %s: %v", hash[:8], err)
		return
	}
	select {
	case <-t.GotInfo():
	case <-time.After(2 * time.Minute):
		log.Printf("torrent %s: метаданные не получены (DHT-таймаут)", hash[:8])
		t.Drop()
		return
	}
	mi := t.Metainfo()
	t.Drop()
	mi.Announce = publicTrackers[0]
	for _, tr := range publicTrackers {
		mi.AnnounceList = append(mi.AnnounceList, []string{tr})
	}
	raw, err := bencode.Marshal(mi)
	if err != nil {
		log.Printf("marshal %s: %v", hash[:8], err)
		return
	}
	if err := os.WriteFile(torrentPath(hash), raw, 0o644); err != nil {
		log.Printf("write %s: %v", hash[:8], err)
		return
	}
	// файл готов — ленты пересоберутся при следующем опросе с .torrent-ссылками
	cacheMu.Lock()
	feedCache = map[string]feedCacheEntry{}
	cacheMu.Unlock()
	log.Printf("torrent %s: .torrent собран (%d байт)", hash[:8], len(raw))
}

// torrentFileValid — прочитать и проверить собранный файл (валидный bencode, info на месте)
func torrentFileValid(hash string) bool {
	raw, err := os.ReadFile(torrentPath(hash))
	if err != nil {
		return false
	}
	var mi metainfo.MetaInfo
	return bencode.Unmarshal(raw, &mi) == nil && len(mi.InfoBytes) > 0
}

// humanSize — человекочитаемый размер для страницы раздачи
func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f ГБ", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f МБ", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f КБ", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d Б", n)
}

// magnetWithTrackers — магнет с публичными трекерами (страница раздачи: клик
// должен сразу находить пиров; в лентах по-прежнему голый btih)
func magnetWithTrackers(hash string) string {
	u := "magnet:?xt=urn:btih:" + hash
	for _, tr := range publicTrackers {
		u += "&tr=" + url.QueryEscape(tr)
	}
	return u
}

// torrentPageHTML — страница раздачи /t/<hash> для разовой подписки (kind=hash):
// прямой ссылки на трекер у такой раздачи часто нет (она может быть не с NNM),
// «что это» смотрим у нас — название, размер, список файлов из .torrent,
// магнет и скачивание; если парсер Lampa передал трекер/страницу раздачи
// (nnm-магниты несут viewtopic в tr=) — показываем и их. Файла ещё нет —
// страница сама обновится (meta refresh), параллельно пинаем DHT-резолвер.
func torrentPageHTML(s *Sub) string {
	hash := s.Hash
	ready := torrentFileValid(hash)
	if !ready {
		prefetchTorrent(hash)
	}

	hdr := "<!doctype html><html lang=\"ru\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">" +
		"<title>" + html.EscapeString(s.Title) + " — раздача</title>" +
		"<meta http-equiv=\"refresh\" content=\"15\">" +
		"<style>" +
		":root{--accent:#c2410c;--accent-txt:#fff;--bg:#f2f4f7;--card:#fff;--text:#222;--muted:#7a828c;--line:#dde2e8}" +
		"@media (prefers-color-scheme: dark){:root{--accent:#c2623d;--accent-txt:#2c2525;--bg:#221f20;--card:#2c2828;--text:#f0e6c8;--muted:#8b8481;--line:#413b3b}}" +
		"*{box-sizing:border-box}body{margin:0;font:15px/1.5 -apple-system,BlinkMacSystemFont,\"Segoe UI\",Roboto,sans-serif;background:var(--bg);color:var(--text)}" +
		".wrap{max-width:640px;margin:40px auto;padding:0 16px}" +
		".card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:20px}" +
		"h1{font-size:17px;margin:0 0 6px;word-break:break-word}" +
		".muted{color:var(--muted);font-size:13px}" +
		".row{margin:14px 0 0}.row a{color:var(--accent);text-decoration:none}" +
		"ul{margin:8px 0 0;padding-left:20px;font-size:13px}li{margin:3px 0;word-break:break-all}" +
		".sz{color:var(--muted);margin-left:6px;white-space:nowrap}" +
		"</style></head><body><div class=\"wrap\"><div class=\"card\">"

	heading := s.Title
	var facts []string

	if ready {
		if mi, err := metainfo.LoadFromFile(torrentPath(hash)); err == nil {
			if info, err := mi.UnmarshalInfo(); err == nil {
				// название из .torrent — настоящее имя раздачи (красивее
				// файлового dn из плагина); подпись подписки — запасной вариант
				heading = info.Name
				if heading == "" {
					heading = s.Title
				}
				facts = append(facts, humanSize(info.TotalLength())+" всего")
				fs := info.UpvertedFiles()
				const capFiles = 300
				for i, f := range fs {
					if i == capFiles {
						facts = append(facts, fmt.Sprintf("и ещё %d файлов", len(fs)-capFiles))
						break
					}
					nm := strings.Join(f.Path, "/")
					if nm == "" {
						nm = info.Name // однофайловый торрент: имени в Path нет
					}
					facts = append(facts, html.EscapeString(nm)+
						"<span class=\"sz\">"+humanSize(f.Length)+"</span>")
				}
			}
		}
	} else {
		wait := "DHT-резолвер ещё собирает .torrent — страница обновится сама"
		if s.Size != "" {
			wait += " (по данным парсера: " + html.EscapeString(s.Size) + ")"
		}
		facts = append(facts, wait)
	}

	if info := s.Tracker; info != "" || s.Size != "" {
		if s.Size != "" {
			if info != "" {
				info += " · "
			}
			info += s.Size
		}
		facts = append(facts, html.EscapeString(info))
	}
	if !s.Added.IsZero() {
		facts = append(facts, "добавлена из Lampa: "+s.Added.In(tz).Format("02.01.2006 15:04"))
	}

	links := "<div class=\"row\"><a href=\"" + magnetWithTrackers(hash) + "\">магнет</a>" +
		" · <a href=\"/t/" + hash + ".torrent\">скачать .torrent</a>"
	if s.Page != "" {
		links += " · <a href=\"" + html.EscapeString(s.Page) + "\" target=\"_blank\" rel=\"noopener\">раздача на трекере ↗</a>"
	}
	links += "</div>"

	out := hdr + "<h1>" + html.EscapeString(heading) + "</h1>" +
		"<p class=\"muted\">Разовая раздача · info-hash <code>" + hash + "</code></p>" +
		hashCard(s) +
		"<ul><li>" + strings.Join(facts, "</li><li>") + "</li></ul>" +
		links + "</div></div></body></html>"
	return out
}

// hashCard — карточка на странице /t/<hash>: описание и обложка как в лентах.
// Темы у разовой раздачи нет — находим её на трекере по названию из парсера
// Lampa (поиск и резолв страницы темы уже под кэшем)
func hashCard(s *Sub) string {
	if s.Title == "" {
		return ""
	}
	id := searchTopicID(s.Title)
	if id == 0 {
		return ""
	}
	rel, err := resolveRelease(id)
	if err != nil {
		return ""
	}

	var side, fields strings.Builder
	if rel.Poster != "" {
		side.WriteString(`<img src="` + html.EscapeString(rel.Poster) + `" alt="" style="width:160px;border-radius:8px;flex:none;max-width:40%">`)
	}
	for _, t := range rel.Tech {
		v := strings.ReplaceAll(html.EscapeString(t.Value), "¶", "<br>")
		fields.WriteString(`<div style="margin:2px 0"><b>` + html.EscapeString(t.Name) + `:</b> ` + v + `</div>`)
	}
	block := `<div style="display:flex;gap:14px;flex-wrap:wrap;margin:16px 0 0">` +
		side.String() +
		`<div style="flex:1;min-width:220px;font-size:13.5px">` + fields.String() + `</div></div>`

	if rel.Descr != "" {
		plot := strings.ReplaceAll(html.EscapeString(rel.Descr), "¶", "<br><br>")
		block += `<div style="margin:12px 0 0;font-size:13.5px">` + plot + `</div>`
	}
	return block
}
