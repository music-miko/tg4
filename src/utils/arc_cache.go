/*
 * TgMusicBot - Telegram Music Bot
 *  Copyright (c) 2025-2026 Ashok Shau
 *
 *  Licensed under GNU GPL v3
 *  See https://github.com/AshokShau/TgMusicBot
 */

package utils

// LookupArcCacheFunc is a function that looks up a cached YouTube track's Telegram
// message ID from the database. A non-positive return value means "not cached".
// This is a hook that the db package registers at init time to avoid an import cycle
// between dl → db.
type LookupArcCacheFunc func(videoID string, video bool) int64

// LookupArcCache is the registered lookup function. It defaults to a no-op that always
// returns 0 (not cached) until the db package replaces it via RegisterArcCacheLookup.
var LookupArcCache LookupArcCacheFunc = func(_ string, _ bool) int64 { return 0 }

// RegisterArcCacheLookup is called once by the db package (in its init function) to
// wire up the real MongoDB-backed lookup.
func RegisterArcCacheLookup(fn LookupArcCacheFunc) {
	LookupArcCache = fn
}
