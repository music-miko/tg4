/*
 * TgMusicBot - Telegram Music Bot
 *  Copyright (c) 2025-2026 Ashok Shau
 *
 *  Licensed under GNU GPL v3
 *  See https://github.com/AshokShau/TgMusicBot
 */

package dl

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ashokshau/tgmusic/config"
)

// arcMusic is a dedicated client for the ArcMusic API, used exclusively for
// resolving and downloading YouTube tracks. Other platforms (Spotify, Apple
// Music, SoundCloud, Deezer, etc.) continue to use the generic apiData client
// configured via API_URL / API_KEY.
type arcMusic struct {
	ApiUrl string
	ApiKey string
}

const (
	arcCreateRetries  = 3               // matches _api.py's create_job: for _ in range(3)
	arcPollRetries    = 10              // matches _api.py's API(retries=10) default
	arcPollInterval   = 3 * time.Second // matches _api.py's get_url: await asyncio.sleep(3)
	arcDownloadCycles = 2               // matches _api.py's download(): for attempt in range(2)
	arcCycleDelay     = 2 * time.Second // matches _api.py's download(): await asyncio.sleep(2) on attempt == 0
)

// newArcMusic creates a new ArcMusic API client using the configured ARC_API_URL / ARC_API_KEY.
func newArcMusic() *arcMusic {
	return &arcMusic{
		ApiUrl: strings.TrimRight(config.ArcApiUrl, "/"),
		ApiKey: config.ArcApiKey,
	}
}

// isConfigured reports whether the ArcMusic API has been configured.
func (a *arcMusic) isConfigured() bool {
	return a.ApiUrl != ""
}

// arcJobResponse models the response of the job-creation endpoint.
type arcJobResponse struct {
	Status string `json:"status"`
	JobId  string `json:"job_id"`
}

// arcJobStatusResponse models the response of the job-status (poll) endpoint.
type arcJobStatusResponse struct {
	Status string `json:"status"`
	Job    struct {
		Status string `json:"status"`
		Result struct {
			PublicUrl string `json:"public_url"`
		} `json:"result"`
	} `json:"job"`
}

// createJob requests a new download job for the given YouTube video ID.
func (a *arcMusic) createJob(videoID string, isVideo bool) (string, error) {
	endpoint := fmt.Sprintf("%s/youtube/v2/download", a.ApiUrl)
	params := url.Values{
		"query":   {videoID},
		"isVideo": {strconv.FormatBool(isVideo)},
	}
	if a.ApiKey != "" {
		params.Set("api_key", a.ApiKey)
	}

	var lastErr error
	for attempt := 0; attempt < arcCreateRetries; attempt++ {
		resp, err := sendRequest(http.MethodGet, endpoint+"?"+params.Encode(), nil, nil)
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}

		var data arcJobResponse
		if err := json.Unmarshal(body, &data); err != nil {
			lastErr = fmt.Errorf("failed to decode create_job response: %w", err)
			time.Sleep(time.Second)
			continue
		}

		if data.Status != "queued" || data.JobId == "" {
			lastErr = fmt.Errorf("unexpected create_job status: %q", data.Status)
			time.Sleep(time.Second)
			continue
		}

		return data.JobId, nil
	}

	if lastErr == nil {
		lastErr = errors.New("create_job failed after retries")
	}
	return "", lastErr
}

// pollJob polls the job-status endpoint until the job completes, then returns
// the CDN public URL of the downloaded file.
func (a *arcMusic) pollJob(jobID string) (string, error) {
	endpoint := fmt.Sprintf("%s/youtube/jobStatus", a.ApiUrl)
	params := url.Values{"job_id": {jobID}}

	var lastErr error
	for attempt := 0; attempt < arcPollRetries; attempt++ {
		resp, err := sendRequest(http.MethodGet, endpoint+"?"+params.Encode(), nil, nil)
		if err != nil {
			lastErr = err
			time.Sleep(arcPollInterval)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			time.Sleep(arcPollInterval)
			continue
		}

		var data arcJobStatusResponse
		if err := json.Unmarshal(body, &data); err != nil {
			lastErr = fmt.Errorf("failed to decode jobStatus response: %w", err)
			time.Sleep(arcPollInterval)
			continue
		}

		if data.Status != "success" || data.Job.Status != "done" {
			lastErr = fmt.Errorf("job not ready (status=%q, job.status=%q)", data.Status, data.Job.Status)
			time.Sleep(arcPollInterval)
			continue
		}

		publicURL := data.Job.Result.PublicUrl
		if publicURL == "" {
			return "", errors.New("job completed but no public_url was returned")
		}

		if strings.HasPrefix(publicURL, "/") {
			publicURL = a.ApiUrl + publicURL
		}

		return publicURL, nil
	}

	if lastErr == nil {
		lastErr = errors.New("jobStatus polling exhausted retries")
	}
	return "", lastErr
}

// resolve first checks the shared ArcMusic media cache for a Telegram-channel
// cache hit ("direct DB downloading" - see media_db.go), and only calls the
// ArcMusic job API (create -> poll -> save) if that lookup misses. This
// mirrors tosu4's _optimized_download: media-DB cache first, then API-1.
//
// The job API cycle (create_job -> get_url -> save_file) mirrors _api.py's
// API.download(): up to arcDownloadCycles attempts, sleeping arcCycleDelay
// between attempts only after a non-final cycle fails.
func (a *arcMusic) resolve(videoID string, isVideo bool) (string, error) {
	if link, ok := lookupDirectDb(videoID, isVideo); ok {
		return link, nil
	}

	if !a.isConfigured() {
		return "", errors.New("ArcMusic API is not configured")
	}

	var lastErr error
	for cycle := 0; cycle < arcDownloadCycles; cycle++ {
		jobID, err := a.createJob(videoID, isVideo)
		if err != nil {
			lastErr = fmt.Errorf("create job: %w", err)
			slog.Warn("ArcMusic create_job failed", "video_id", videoID, "cycle", cycle+1, "error", err)
			if cycle == 0 {
				time.Sleep(arcCycleDelay)
			}
			continue
		}

		publicURL, err := a.pollJob(jobID)
		if err != nil {
			lastErr = fmt.Errorf("poll job: %w", err)
			slog.Warn("ArcMusic jobStatus failed", "video_id", videoID, "job_id", jobID, "cycle", cycle+1, "error", err)
			if cycle == 0 {
				time.Sleep(arcCycleDelay)
			}
			continue
		}

		ext := ".m4a"
		if isVideo {
			ext = ".mp4"
		}
		fileName := determineFilename(publicURL, "")
		if !strings.HasSuffix(fileName, ext) {
			fileName = strings.TrimSuffix(fileName, filepath.Ext(fileName)) + ext
		}

		filePath, err := downloadFile(publicURL, fileName, false)
		if err != nil {
			lastErr = fmt.Errorf("save file: %w", err)
			slog.Warn("ArcMusic save_file failed", "video_id", videoID, "url", publicURL, "cycle", cycle+1, "error", err)
			if cycle == 0 {
				time.Sleep(arcCycleDelay)
			}
			continue
		}

		return filePath, nil
	}

	if lastErr == nil {
		lastErr = errors.New("ArcMusic download failed after all cycles")
	}
	return "", lastErr
}
