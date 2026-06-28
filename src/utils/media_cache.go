/*
 * TgMusicBot - Telegram Music Bot
 *  Copyright (c) 2025-2026 Ashok Shau
 *
 *  Licensed under GNU GPL v3
 *  See https://github.com/AshokShau/TgMusicBot
 */

package utils

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	mediaDBName         = "arcapi"
	mediaCollectionName = "medias"
)

var (
	mediaMongoClient *mongo.Client
	mediaMongoMu     sync.Mutex
)

// mediaRecord mirrors the document structure stored by the Arc API media cache.
type mediaRecord struct {
	TrackID   string `bson:"track_id"`
	IsVideo   bool   `bson:"isVideo"`
	MessageID int64  `bson:"message_id"`
}

func getMediaCollection(dbURI string) (*mongo.Collection, error) {
	mediaMongoMu.Lock()
	defer mediaMongoMu.Unlock()

	if mediaMongoClient == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		opts := options.Client().ApplyURI(dbURI).
			SetConnectTimeout(10 * time.Second).
			SetServerSelectionTimeout(10 * time.Second)

		client, err := mongo.Connect(opts)
		if err != nil {
			return nil, fmt.Errorf("media cache: mongo connect: %w", err)
		}

		if err := client.Ping(ctx, nil); err != nil {
			return nil, fmt.Errorf("media cache: mongo ping: %w", err)
		}

		mediaMongoClient = client
		slog.Info("[MediaCache] Connected to media MongoDB (DB_URI)")
	}

	return mediaMongoClient.Database(mediaDBName).Collection(mediaCollectionName), nil
}

// LookupMediaCache queries the arcapi.medias collection in the DB_URI MongoDB instance
// and returns the Telegram message_id for the given track, or 0 if not found.
func LookupMediaCache(dbURI, trackID string, isVideo bool) (int64, error) {
	if dbURI == "" || trackID == "" {
		return 0, nil
	}

	col, err := getMediaCollection(dbURI)
	if err != nil {
		return 0, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Try all key variants used by the Arc API
	ext := "mp3"
	if isVideo {
		ext = "mp4"
	}

	keys := []string{
		trackID,
		fmt.Sprintf("%s.%s", trackID, ext),
	}

	for _, key := range keys {
		filter := bson.M{"track_id": key, "isVideo": isVideo}
		var rec mediaRecord
		err := col.FindOne(ctx, filter).Decode(&rec)
		if err == nil && rec.MessageID != 0 {
			slog.Info("[MediaCache] Cache hit", "track_id", key, "msg_id", rec.MessageID)
			return rec.MessageID, nil
		}
	}

	return 0, nil
}
