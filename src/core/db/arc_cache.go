/*
 * TgMusicBot - Telegram Music Bot
 *  Copyright (c) 2025-2026 Ashok Shau
 *
 *  Licensed under GNU GPL v3
 *  See https://github.com/AshokShau/TgMusicBot
 */

package db

// arc_cache.go manages a dedicated MongoDB connection to the ArcMusic cache database.
//
// This mirrors the Python _api.py Cache class exactly:
//   - URI:        config.CACHE_DB  (separate from the main MONGO_URI)
//   - Database:   "arcapi"
//   - Collection: "medias"
//   - Document:   { "track_id": "<id>.mp3|mp4", "isVideo": bool, "message_id": int }
//
// The lookup function is registered into utils.LookupArcCache (via RegisterArcCacheLookup)
// so the dl package can call it without importing db (which would cause a cycle).

import (
	"ashokshau/tgmusic/config"
	"ashokshau/tgmusic/src/utils"
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// arcCacheDB holds the dedicated connection to the ArcMusic cache MongoDB.
// It is initialised once by InitArcCacheDB and is nil when CACHE_DB is not set.
var arcCacheDB *arcCacheDatabase

type arcCacheDatabase struct {
	client *mongo.Client
	medias *mongo.Collection // arcapi.medias
}

// arcMediaDoc mirrors the document shape written by the Python ArcMusic logger:
//
//	{ "track_id": "<videoID>.mp3", "isVideo": false, "message_id": 12345 }
type arcMediaDoc struct {
	TrackID   string `bson:"track_id"`
	IsVideo   bool   `bson:"isVideo"`
	MessageID int64  `bson:"message_id"`
}

// InitArcCacheDB connects to the CACHE_DB MongoDB URI and wires up the lookup hook.
// It is called from InitDatabase (mongo.go) after the main DB is ready.
// If CACHE_DB is empty the function is a no-op — lookups will always return 0.
func InitArcCacheDB() {
	// Always register the hook (no-op default already set in utils, but registering
	// the real one here ensures init order doesn't matter).
	utils.RegisterArcCacheLookup(arcCacheLookup)

	uri := config.CacheDb
	if uri == "" {
		slog.Info("[ArcCacheDB] CACHE_DB not set — DB channel cache disabled")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12500*time.Millisecond)
	defer cancel()

	opts := options.Client().ApplyURI(uri).
		SetServerSelectionTimeout(12500 * time.Millisecond).
		SetConnectTimeout(12500 * time.Millisecond)

	client, err := mongo.Connect(opts)
	if err != nil {
		slog.Error("[ArcCacheDB] Failed to connect to CACHE_DB", "error", err)
		return
	}

	// Ping to confirm connectivity — mirrors Python's admin.command("ping").
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err(); err != nil {
		slog.Error("[ArcCacheDB] CACHE_DB ping failed", "error", err)
		_ = client.Disconnect(context.Background())
		return
	}

	arcCacheDB = &arcCacheDatabase{
		client: client,
		medias: client.Database("arcapi").Collection("medias"),
	}

	slog.Info("[ArcCacheDB] Connected to ArcMusic cache database (arcapi.medias)")
}

// CloseArcCacheDB gracefully disconnects the dedicated cache client.
// Called from the main shutdown path alongside the main DB close.
func CloseArcCacheDB() {
	if arcCacheDB == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := arcCacheDB.client.Disconnect(ctx); err != nil {
		slog.Warn("[ArcCacheDB] Error during disconnect", "error", err)
	}
	arcCacheDB = nil
	slog.Info("[ArcCacheDB] Disconnected from ArcMusic cache database")
}

// arcCacheLookup is registered as utils.LookupArcCache.
// It queries arcapi.medias for the message_id of a cached track.
// Returns 0 when not found or the cache DB is unavailable.
func arcCacheLookup(videoID string, video bool) int64 {
	if arcCacheDB == nil {
		return 0
	}

	fname := fmt.Sprintf("%s.mp3", videoID)
	if video {
		fname = fmt.Sprintf("%s.mp4", videoID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	filter := bson.M{
		"track_id": fname,
		"isVideo":  video,
	}

	var doc arcMediaDoc
	if err := arcCacheDB.medias.FindOne(ctx, filter).Decode(&doc); err != nil {
		// mongo.ErrNoDocuments is the normal miss path — not an error worth logging.
		return 0
	}

	slog.Info("[ArcCacheDB] Cache hit", "video_id", videoID, "message_id", doc.MessageID)
	return doc.MessageID
}
