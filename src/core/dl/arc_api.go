/*
 * TgMusicBot - Telegram Music Bot
 *  Copyright (c) 2025-2026 Ashok Shau
 *
 *  Licensed under GNU GPL v3
 *  See https://github.com/AshokShau/TgMusicBot
 */

package dl

// arcApi provides YouTube-specific downloading via the ArcMusic API (API_URL2 / API_KEY2).
// It mirrors the logic from the Python _api.py reference implementation:
//   - Check in-memory cache first
//   - Check DB channel cache (Telegram channel identified by DL_LOGGER)
//   - Create a download job, poll for its URL, then save the file locally
//
// The ArcMusic API exposes two endpoints:
//   POST /youtube/v2/download  → queues a job, returns {"status":"queued","job_id":"..."}
//   GET  /youtube/jobStatus    → polls, returns {"status":"success","job":{"status":"done","result":{"public_url":"..."}}}

import (
	"ashokshau/tgmusic/config"
	"ashokshau/tgmusic/src/utils"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ---- in-memory caches (shared across all calls in this process) ----

var (
	arcAudioCache = map[string]string{} // videoID → local file path
	arcVideoCache = map[string]string{} // videoID → local file path
)

// ---- ArcMusic API response shapes ----

type arcJobResponse struct {
	Status string `json:"status"`
	JobID  string `json:"job_id"`
}

type arcJobStatusResponse struct {
	Status string `json:"status"`
	Job    struct {
		Status string `json:"status"`
		Result struct {
			PublicURL string `json:"public_url"`
		} `json:"result"`
	} `json:"job"`
}

// ---- DB channel cache (mirrors Python Cache class) ----

// arcCacheGetTrack looks up a cached message in the Telegram DL_LOGGER channel.
// Returns the local file path on success, or "" if not cached / DL_LOGGER is not set.
func arcCacheGetTrack(videoID string, video bool) string {
	if DlBot == nil || config.DlLogger == 0 {
		return ""
	}

	// We store the file as "<videoID>.mp4" (video) or "<videoID>.mp3" (audio).
	// Because the Go bot layer does not expose a MongoDB cache the same way the Python
	// layer does, we rely on the channel's pinned-style naming convention used elsewhere
	// in the bot (see src/core/db/logger.go) and attempt to download from the channel.
	// This is intentionally a best-effort lookup; a miss is not an error.
	//
	// NOTE: The MongoDB cache lookup (fetch_id → get_messages → download) requires the
	// dl package to have access to the database collection. Rather than coupling dl to db,
	// we expose the lookup via the existing db.Instance helper that already knows about
	// the logger channel.  If the collection returns a message ID we download it.

	msgID := utils.LookupArcCache(videoID, video) // see utils/arc_cache.go
	if msgID <= 0 {
		return ""
	}

	msg, err := DlBot.GetMessage(config.DlLogger, msgID)
	if err != nil {
		slog.Warn("arcCacheGetTrack: failed to get message from DL_LOGGER", "err", err)
		return ""
	}

	file, err := msg.Download(DlBot, 1, 0, 0, true)
	if err != nil || file == nil || file.Local == nil {
		slog.Warn("arcCacheGetTrack: failed to download cached file", "err", err)
		return ""
	}

	return file.Local.Path
}

// ---- HTTP helpers ----

func arcGet(rawURL string, queryParams map[string]string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}

	q := u.Query()
	for k, v := range queryParams {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	resp, err := http.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("arc api: unexpected status %s for %s", resp.Status, u.String())
	}

	return io.ReadAll(resp.Body)
}

func arcPost(rawURL string, formParams map[string]string) ([]byte, error) {
	form := url.Values{}
	for k, v := range formParams {
		form.Set(k, v)
	}

	resp, err := http.PostForm(rawURL, form)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("arc api: unexpected status %s for %s", resp.Status, rawURL)
	}

	return io.ReadAll(resp.Body)
}

// ---- Job lifecycle ----

const (
	arcMaxJobAttempts    = 3
	arcMaxPollAttempts   = 10
	arcPollIntervalSec   = 3
	arcDownloadAttempts  = 2
)

// arcCreateJob queues a download job on the ArcMusic API.
// Returns the job ID or "" on failure.
func arcCreateJob(videoID string, video bool) string {
	apiURL := strings.TrimRight(config.ApiUrl2, "/")
	endpoint := apiURL + "/youtube/v2/download"

	isVideoStr := "false"
	if video {
		isVideoStr = "true"
	}

	params := map[string]string{
		"api_key":  config.ApiKey2,
		"query":    videoID,
		"isVideo":  isVideoStr,
	}

	for attempt := 0; attempt < arcMaxJobAttempts; attempt++ {
		body, err := arcGet(endpoint, params)
		if err != nil {
			slog.Warn("arcCreateJob: request failed", "attempt", attempt+1, "err", err)
			time.Sleep(time.Second)
			continue
		}

		var resp arcJobResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			slog.Warn("arcCreateJob: decode failed", "attempt", attempt+1, "err", err)
			time.Sleep(time.Second)
			continue
		}

		if resp.Status != "queued" || resp.JobID == "" {
			slog.Warn("arcCreateJob: unexpected response", "status", resp.Status, "job_id", resp.JobID)
			time.Sleep(time.Second)
			continue
		}

		return resp.JobID
	}

	return ""
}

// arcGetURL polls the job-status endpoint until the download URL is ready.
// Returns the full public URL or "" on failure.
func arcGetURL(jobID string) string {
	apiURL := strings.TrimRight(config.ApiUrl2, "/")
	endpoint := apiURL + "/youtube/jobStatus"

	for attempt := 1; attempt <= arcMaxPollAttempts; attempt++ {
		body, err := arcGet(endpoint, map[string]string{"job_id": jobID})
		if err != nil {
			slog.Warn("arcGetURL: request failed", "attempt", attempt, "err", err)
			time.Sleep(arcPollIntervalSec * time.Second)
			continue
		}

		var resp arcJobStatusResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			slog.Warn("arcGetURL: decode failed", "err", err)
			time.Sleep(arcPollIntervalSec * time.Second)
			continue
		}

		if resp.Status != "success" || resp.Job.Status != "done" {
			time.Sleep(arcPollIntervalSec * time.Second)
			continue
		}

		publicURL := resp.Job.Result.PublicURL
		if publicURL == "" {
			break
		}

		// The API returns a path like "/files/abc.mp3"; prepend the base URL.
		if !strings.HasPrefix(publicURL, "http") {
			publicURL = apiURL + publicURL
		}

		slog.Info("arcGetURL: received download URL", "attempt", attempt, "url", publicURL)
		return publicURL
	}

	return ""
}

// arcSaveFile downloads the file at dlURL to the configured downloads directory.
func arcSaveFile(dlURL string) (string, error) {
	fname := filepath.Base(strings.Split(dlURL, "?")[0]) // strip query params from filename
	fpath := filepath.Join(config.DownloadsDir, fname)

	resp, err := http.Get(dlURL)
	if err != nil {
		return "", fmt.Errorf("arcSaveFile: GET failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("arcSaveFile: unexpected status %s", resp.Status)
	}

	out, err := os.Create(fpath)
	if err != nil {
		return "", fmt.Errorf("arcSaveFile: create file: %w", err)
	}
	defer out.Close()

	buf := make([]byte, 1024*1024) // 1 MB chunks
	if _, err := io.CopyBuffer(out, resp.Body, buf); err != nil {
		return "", fmt.Errorf("arcSaveFile: write failed: %w", err)
	}

	return fpath, nil
}

// ---- Public entry point ----

// ArcDownload is the primary function called by the YouTube downloader.
// Priority order:
//  1. In-memory cache (fastest)
//  2. Telegram DL_LOGGER channel DB cache
//  3. ArcMusic API → job → poll → save
//
// Returns the local file path or an error.
func ArcDownload(videoID string, video bool) (string, error) {
	// 1. In-memory cache
	if video {
		if p, ok := arcVideoCache[videoID]; ok {
			return p, nil
		}
	} else {
		if p, ok := arcAudioCache[videoID]; ok {
			return p, nil
		}
	}

	// 2. DB channel cache
	if fpath := arcCacheGetTrack(videoID, video); fpath != "" {
		slog.Info("ArcDownload: retrieved from DB channel cache", "video_id", videoID)
		if video {
			arcVideoCache[videoID] = fpath
		} else {
			arcAudioCache[videoID] = fpath
		}
		return fpath, nil
	}

	// 3. ArcMusic API
	for attempt := 0; attempt < arcDownloadAttempts; attempt++ {
		jobID := arcCreateJob(videoID, video)
		if jobID == "" {
			if attempt == 0 {
				time.Sleep(2 * time.Second)
			}
			continue
		}

		dlURL := arcGetURL(jobID)
		if dlURL == "" {
			if attempt == 0 {
				time.Sleep(2 * time.Second)
			}
			continue
		}

		fpath, err := arcSaveFile(dlURL)
		if err != nil {
			slog.Warn("ArcDownload: save failed", "err", err, "attempt", attempt+1)
			if attempt == 0 {
				time.Sleep(2 * time.Second)
			}
			continue
		}

		if video {
			arcVideoCache[videoID] = fpath
		} else {
			arcAudioCache[videoID] = fpath
		}

		return fpath, nil
	}

	return "", fmt.Errorf("ArcDownload: all attempts failed for video_id=%s video=%v", videoID, video)
}

// ArcIsConfigured returns true when both API_URL2 and API_KEY2 are set.
func ArcIsConfigured() bool {
	return config.ApiUrl2 != "" && config.ApiKey2 != ""
}

// ---- Stub for utils.LookupArcCache (resolved at link time via utils/arc_cache.go) ----
// The real implementation lives in utils/arc_cache.go so that the db package can
// register a lookup function without creating an import cycle.
var _ = utils.LookupArcCache // ensure the symbol is referenced
