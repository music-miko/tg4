/*
 * ArcMusicBot - Telegram Music Bot
 *  Copyright (c) 2025-2026 Team Arc
 *
 *  Licensed under GNU GPL v3
 *
 *  Media-DB: queries MongoDB arcapi.medias for a cached Telegram message ID.
 *  Mirrors tosu4's _is_media / _get_media_msg_id helpers.
 */

package dl

import (
	"context"
	"log/slog"
	"time"

	"ashokshau/tgmusic/config"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	arcDBName         = "arcapi"
	arcMediaCollection = "medias"
)

var (
	_arcMongoClient *mongo.Client
)

func getArcMediaCollection() (*mongo.Collection, error) {
	if _arcMongoClient == nil {
		opts := options.Client().ApplyURI(config.MongoUri).
			SetConnectTimeout(10 * time.Second)
		c, err := mongo.Connect(opts)
		if err != nil {
			return nil, err
		}
		_arcMongoClient = c
	}
	return _arcMongoClient.Database(arcDBName).Collection(arcMediaCollection), nil
}

// mediaDBLookup checks arcapi.medias for a cached Telegram message_id.
// Returns (message_id, nil) on hit, (0, err) on miss or error.
func mediaDBLookup(trackID string, isVideo bool) (int64, error) {
	if config.MongoUri == "" || config.MediaChannelId == 0 {
		return 0, nil
	}

	col, err := getArcMediaCollection()
	if err != nil {
		slog.Warn("[MediaDB] connect error", "error", err)
		return 0, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Try canonical key patterns (mirrors tosu4 keys list)
	ext := "mp3"
	if isVideo {
		ext = "mp4"
	}
	keys := []string{
		trackID + "." + ext,
		trackID,
	}

	for _, k := range keys {
		filter := bson.D{
			{Key: "track_id", Value: k},
			{Key: "isVideo", Value: isVideo},
		}
		var doc struct {
			MessageID int64 `bson:"message_id"`
		}
		err := col.FindOne(ctx, filter, options.FindOne().SetProjection(bson.D{{Key: "message_id", Value: 1}})).Decode(&doc)
		if err == nil && doc.MessageID != 0 {
			slog.Info("[MediaDB] cache hit", "key", k, "msg_id", doc.MessageID)
			return doc.MessageID, nil
		}
	}

	return 0, nil
}
