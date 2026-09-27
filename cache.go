package main

// Персистентный кэш на томе (cache.json рядом со state.json): резолвы тем и
// тела родных лент/страниц топа переживают рестарт контейнера. Записываем раз
// в минуту и только при изменениях, старше 72 ч вычищаем.
// (Фоновое обновление по таймеру и «только новое» — следующий шаг, отдельной
// правкой логики запросов к nnm.)

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

var (
	cacheFile  = filepath.Join(env("DATA_DIR", "data"), "cache.json")
	cacheDirty bool // под cacheMu
)

type diskCache struct {
	Resolve map[string]diskResolve `json:"resolve"` // темы → Release, рейтинги imdb:/mal:
	RSS     map[string]diskRSS     `json:"rss"`     // родные ленты подписок + страницы топа
	Saved   time.Time              `json:"saved"`
}

// тени с экспортированными полями: resolveEntry/rssCacheEntry в памяти имеют
// неэкспортированные поля — json.Marshal выгрузил бы их как {} (так и случилось)
type diskResolve struct {
	TS  time.Time `json:"ts"`
	Rel Release   `json:"rel"`
}

type diskRSS struct {
	TS   time.Time `json:"ts"`
	Body string    `json:"body"`
}

// loadDiskCache — warm start: накопленное не теряется на рестартах
func loadDiskCache() {
	raw, err := os.ReadFile(cacheFile)
	if err != nil {
		return
	}
	var dc diskCache
	if json.Unmarshal(raw, &dc) != nil {
		return
	}
	cacheMu.Lock()
	nr, ns := 0, 0
	for k, v := range dc.Resolve {
		if !v.TS.IsZero() && v.Rel.Hash != "" {
			resolveCache[k] = resolveEntry{ts: v.TS, rel: v.Rel}
			nr++
		}
	}
	for k, v := range dc.RSS {
		if !v.TS.IsZero() && v.Body != "" {
			rssCache[k] = rssCacheEntry{ts: v.TS, body: v.Body}
			ns++
		}
	}
	cacheMu.Unlock()
	log.Printf("кэш: загружено %d резолвов, %d лент (сохранено %s)",
		nr, ns, dc.Saved.Format("02.01 15:04"))
}

// markCacheDirty — под cacheMu, в блоках записи кэшей
func markCacheDirty() { cacheDirty = true }

// startCacheSaver — фоновый сброс на диск
func startCacheSaver() {
	go func() {
		for range time.Tick(time.Minute) {
			cacheMu.Lock()
			dirty := cacheDirty
			cacheDirty = false
			cutoff := time.Now().Add(-72 * time.Hour)
			dresolve := make(map[string]diskResolve, len(resolveCache))
			keepResolve := make(map[string]resolveEntry, len(resolveCache))
			for k, v := range resolveCache {
				if v.ts.After(cutoff) {
					dresolve[k] = diskResolve{TS: v.ts, Rel: v.rel}
					keepResolve[k] = v
				}
			}
			drss := make(map[string]diskRSS, len(rssCache))
			keepRSS := make(map[string]rssCacheEntry, len(rssCache))
			for k, v := range rssCache {
				if v.ts.After(cutoff) {
					drss[k] = diskRSS{TS: v.ts, Body: v.body}
					keepRSS[k] = v
				}
			}
			resolveCache, rssCache = keepResolve, keepRSS // 72 ч: старое вычищаем
			cacheMu.Unlock()
			if !dirty {
				continue
			}
			raw, err := json.Marshal(diskCache{Resolve: dresolve, RSS: drss, Saved: time.Now()})
			if err != nil {
				continue
			}
			tmp := cacheFile + ".tmp"
			if os.WriteFile(tmp, raw, 0o644) == nil {
				os.Rename(tmp, cacheFile)
			}
		}
	}()
}
