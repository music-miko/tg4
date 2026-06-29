/*
 * TgMusicBot - Telegram Music Bot
 *  Copyright (c) 2025-2026 Ashok Shau
 *
 *  Licensed under GNU GPL v3
 *  See https://github.com/AshokShau/TgMusicBot
 */

package dl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"ashokshau/tgmusic/config"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mediaDbName / mediaCollectionName identify the shared, externally-populated
// ArcMusic media cache. This bot only ever reads from it - it is the same
// collection ArcMusic itself (and other bots using the same backend) write
// into when a YouTube track has already been cached in a Telegram channel.
const (
	mediaDbName         = "arcapi"
	mediaCollectionName = "medias"
)

// mediaDoc mirrors a single cache record: {track_id, isVideo, message_id}.
type mediaDoc struct {
	MessageId int64 `bson:"message_id"`
}

var (
	mediaMongoClient *mongo.Client
	mediaMongoOnce   sync.Once
	mediaMongoErr    error
)

// getMediaCollection lazily connects to the DB_URI Mongo instance and returns
// the shared media-cache collection. Returns nil if DB_URI is not configured.
func getMediaCollection() *mongo.Collection {
	if config.DbUri == "" {
		return nil
	}

	mediaMongoOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		opts := options.Client().ApplyURI(config.DbUri).SetConnectTimeout(10 * time.Second)
		mongoClient, err := mongo.Connect(opts)
		if err != nil {
			mediaMongoErr = err
			return
		}

		if err := mongoClient.Ping(ctx, nil); err != nil {
			mediaMongoErr = err
			return
		}

		mediaMongoClient = mongoClient
	})

	if mediaMongoErr != nil || mediaMongoClient == nil {
		return nil
	}

	return mediaMongoClient.Database(mediaDbName).Collection(mediaCollectionName)
}

// getMediaMessageId looks up the cached Telegram message ID for a track, trying
// a few key variants for compatibility with how the cache may have been written.
func getMediaMessageId(trackID string, isVideo bool) (int64, bool) {
	col := getMediaCollection()
	if col == nil {
		return 0, false
	}

	ext := "mp3"
	if isVideo {
		ext = "mp4"
	}

	keys := []string{
		fmt.Sprintf("%s.%s", trackID, ext),
		trackID,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, key := range keys {
		var doc mediaDoc
		err := col.FindOne(ctx, bson.M{"track_id": key, "isVideo": isVideo}).Decode(&doc)
		if err == nil && doc.MessageId != 0 {
			return doc.MessageId, true
		}
		if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
			slog.Warn("ArcMusic media-DB lookup failed", "track_id", key, "error", err)
		}
	}

	return 0, false
}

// directDbLink builds the path that points the existing Telegram-message
// download pipeline (see downloadViaWrapper in downloader.go, which detects
// utils.TelegramMessageRegex) at the cached media channel message.
//
// MediaChannelId is expected to be a full chat ID (e.g. -1001234567890); the
// public t.me/c/<id>/<msg> link format requires the internal ID with the
// "-100" channel marker stripped off.
func directDbLink(messageId int64) (string, bool) {
	channelID := config.MediaChannelId
	if channelID == 0 {
		return "", false
	}

	internalID := channelID
	if internalID < 0 {
		internalID = -internalID
		const supergroupMarker = 1000000000000
		if internalID > supergroupMarker {
			internalID -= supergroupMarker
		}
	}

	return fmt.Sprintf("https://t.me/c/%d/%d", internalID, messageId), true
}

// lookupDirectDb checks the shared ArcMusic media cache for a track and, on a
// hit, returns a t.me link to the cached file in the configured media channel.
// This is "direct DB downloading": skipping the ArcMusic API/yt-dlp entirely
// when the file has already been cached in Telegram by a prior download.
func lookupDirectDb(trackID string, isVideo bool) (string, bool) {
	if config.DbUri == "" || config.MediaChannelId == 0 || trackID == "" {
		return "", false
	}

	msgID, ok := getMediaMessageId(trackID, isVideo)
	if !ok {
		return "", false
	}

	link, ok := directDbLink(msgID)
	if !ok {
		return "", false
	}

	slog.Info("ArcMusic direct DB cache hit", "track_id", trackID, "message_id", msgID)
	return link, true
}
