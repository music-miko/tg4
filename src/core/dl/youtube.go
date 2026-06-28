/*
 * ArcMusicBot - Telegram Music Bot
 *  Copyright (c) 2025-2026 Team Arc
 *
 *  Licensed under GNU GPL v3
 *
 *  YouTube platform: uses Arc Music API (job-based) + direct DB (media-channel)
 *  cache before falling back to yt-dlp. Mirrors tosu4's _optimized_download flow.
 */

package dl

import (

	"ashokshau/tgmusic/config"
	"ashokshau/tgmusic/src/utils"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// youTubeData provides an interface for fetching track and playlist information from YouTube.
type youTubeData struct {
	Query    string
	ApiUrl   string
	APIKey   string
	Patterns map[string]*regexp.Regexp
}

var youtubePatterns = map[string]*regexp.Regexp{
	"youtube":   regexp.MustCompile(`(?i)^(?:https?://)?(?:www\.)?youtube\.com/.*`),
	"youtu_be":  regexp.MustCompile(`(?i)^(?:https?://)?(?:www\.)?youtu\.be/.*`),
	"yt_music":  regexp.MustCompile(`(?i)^(?:https?://)?music\.youtube\.com/.*`),
	"yt_shorts": regexp.MustCompile(`(?i)^(?:https?://)?(?:www\.)?youtube\.com/shorts/.*`),
}

// newYouTubeData initializes a youTubeData instance with pre-compiled regex patterns and a cleaned query.
func newYouTubeData(query string) *youTubeData {
	return &youTubeData{
		Query:    strings.TrimSpace(query),
		ApiUrl:   strings.TrimRight(config.ApiUrl, "/"),
		APIKey:   config.ApiKey,
		Patterns: youtubePatterns,
	}
}

func (y *youTubeData) isValid() bool {
	if y.Query == "" {
		slog.Info("The query or patterns are empty.")
		return false
	}

	for _, pattern := range y.Patterns {
		if pattern.MatchString(y.Query) {
			return true
		}
	}
	return false
}

func (y *youTubeData) getInfo() (utils.PlatformTracks, error) {
	if !y.isValid() {
		return utils.PlatformTracks{}, errors.New("the provided URL is invalid or the platform is not supported")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()

	y.Query = normalizeYouTubeURL(y.Query)
	videoID := extractVideoID(y.Query)
	playlistID := extractPlaylistID(y.Query)

	switch {
	case playlistID != "":
		if strings.HasPrefix(playlistID, "RD") {
			return getYouTubeMixPlaylist(ctx, playlistID)
		}
		return getYouTubePlaylist(ctx, playlistID)

	case videoID != "":
		for _, query := range []string{videoID, y.Query} {
			tracks, err := searchYouTube(query, 10)
			if err != nil {
				continue
			}

			for _, track := range tracks {
				if track.Id == videoID {
					return utils.PlatformTracks{Results: []utils.MusicTrack{track}}, nil
				}
			}
		}

		if title, err := getYouTubeTitleFromOEmbed(videoID); err == nil && title != "" {
			tracks, err := searchYouTube(title, 10)
			if err == nil {
				for _, track := range tracks {
					if track.Id == videoID {
						return utils.PlatformTracks{Results: []utils.MusicTrack{track}}, nil
					}
				}
			}
		}

		slog.Warn("Video ID was extracted but no matching track was found in search results", "video_id", videoID)
		return getYouTubeVideo(ctx, videoID)
	}

	return utils.PlatformTracks{}, errors.New("no video or playlist results were found")
}

func (y *youTubeData) search() (utils.PlatformTracks, error) {
	tracks, err := searchYouTube(y.Query, 5)
	if err != nil {
		return utils.PlatformTracks{}, err
	}

	if len(tracks) == 0 {
		return utils.PlatformTracks{}, errors.New("no video results were found")
	}

	return utils.PlatformTracks{Results: tracks}, nil
}

func (y *youTubeData) getTrack() (utils.TrackInfo, error) {
	if y.Query == "" {
		return utils.TrackInfo{}, errors.New("the query is empty")
	}

	if !y.isValid() {
		return utils.TrackInfo{}, errors.New("the provided URL is invalid or the platform is not supported")
	}

	if y.ApiUrl != "" && y.APIKey != "" {
		if trackInfo, err := newApiData(y.Query).getTrack(); err == nil {
			return trackInfo, nil
		}
	}

	getInfo, err := y.getInfo()
	if err != nil {
		return utils.TrackInfo{}, err
	}
	if len(getInfo.Results) == 0 {
		return utils.TrackInfo{}, errors.New("no video results were found")
	}

	track := getInfo.Results[0]
	trackInfo := utils.TrackInfo{
		Id:       track.Id,
		URL:      track.Url,
		Platform: utils.YouTube,
	}

	return trackInfo, nil
}

// downloadTrack orchestrates YouTube download:
//  1. Direct DB (media-channel) cache
//  2. Arc Music API (job-based)
//  3. yt-dlp fallback
func (y *youTubeData) downloadTrack(info utils.TrackInfo, video bool) (string, error) {
	videoID := extractVideoID(info.URL)
	if videoID == "" {
		videoID = info.Id
	}

	// 1. Direct DB / media-channel cache
	if videoID != "" && config.MediaChannelId != 0 {
		if path, err := downloadFromMediaChannel(videoID, video); err == nil && path != "" {
			slog.Info("[YT] MediaDB cache hit", "id", videoID)
			return path, nil
		}
	}

	// 2. Arc Music API (job → poll → save)
	if y.ApiUrl != "" && y.APIKey != "" {
		if path, err := y.downloadWithArcAPI(videoID, video); err == nil && path != "" {
			return path, nil
		}
	}

	// 3. yt-dlp fallback
	return y.downloadWithYtDlp(videoID, video)
}

// ── Arc Music API (job-based, mirrors tosu4 _api1_download) ──────────────────

const (
	arcAPICreateRetries = 3
	arcAPIPollRetries   = 12
	arcAPIPollDelay     = 3 * time.Second
)

type arcJobResponse struct {
	Status string `json:"status"`
	JobID  string `json:"job_id"`
}

type arcJobStatus struct {
	Status string `json:"status"`
	Job    struct {
		Status string `json:"status"`
		Result struct {
			PublicURL string `json:"public_url"`
		} `json:"result"`
	} `json:"job"`
}

func (y *youTubeData) arcAPICreateJob(videoID string, isVideo bool) (string, error) {
	endpoint := y.ApiUrl + "/youtube/v2/download"
	params := url.Values{
		"api_key": {y.APIKey},
		"query":   {videoID},
		"isVideo": {fmt.Sprintf("%v", isVideo)},
	}
	fullURL := endpoint + "?" + params.Encode()

	for attempt := 0; attempt < arcAPICreateRetries; attempt++ {
		resp, err := sendRequest(http.MethodGet, fullURL, nil, nil)
		if err != nil {
			slog.Warn("[ArcAPI] create_job request failed", "attempt", attempt+1, "error", err)
			time.Sleep(time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			slog.Warn("[ArcAPI] create_job HTTP error", "status", resp.StatusCode, "attempt", attempt+1)
			time.Sleep(time.Second)
			continue
		}
		var data arcJobResponse
		if err := json.Unmarshal(body, &data); err != nil || data.JobID == "" {
			slog.Warn("[ArcAPI] create_job bad response", "attempt", attempt+1)
			time.Sleep(time.Second)
			continue
		}
		return data.JobID, nil
	}
	return "", errors.New("arc API: create_job exhausted retries")
}

func (y *youTubeData) arcAPIPollJob(jobID string) (string, error) {
	endpoint := y.ApiUrl + "/youtube/jobStatus"
	params := url.Values{"job_id": {jobID}}
	fullURL := endpoint + "?" + params.Encode()

	for attempt := 0; attempt < arcAPIPollRetries; attempt++ {
		time.Sleep(arcAPIPollDelay)
		resp, err := sendRequest(http.MethodGet, fullURL, nil, nil)
		if err != nil {
			slog.Warn("[ArcAPI] poll_job error", "attempt", attempt+1, "error", err)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			continue
		}
		var data arcJobStatus
		if err := json.Unmarshal(body, &data); err != nil {
			continue
		}
		if data.Status == "success" && data.Job.Status == "done" {
			pubURL := data.Job.Result.PublicURL
			if pubURL == "" {
				break
			}
			if strings.HasPrefix(pubURL, "/") {
				pubURL = y.ApiUrl + pubURL
			}
			return pubURL, nil
		}
	}
	return "", fmt.Errorf("arc API: poll exhausted for job %s", jobID)
}

// downloadWithArcAPI runs the full Arc Music API download flow.
func (y *youTubeData) downloadWithArcAPI(videoID string, video bool) (string, error) {
	if videoID == "" {
		return "", errors.New("videoID is empty")
	}
	ext := "m4a"
	if video {
		ext = "mp4"
	}
	outPath := filepath.Join(config.DownloadsDir, videoID+"."+ext)
	if info, err := os.Stat(outPath); err == nil && info.Size() > 0 {
		slog.Info("[ArcAPI] local cache hit", "path", outPath)
		return outPath, nil
	}

	jobID, err := y.arcAPICreateJob(videoID, video)
	if err != nil {
		return "", err
	}
	dlURL, err := y.arcAPIPollJob(jobID)
	if err != nil {
		return "", err
	}

	// stream to file
	resp, err := sendRequest(http.MethodGet, dlURL, nil, nil)
	if err != nil {
		return "", fmt.Errorf("arc API: download request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("arc API: download HTTP %d", resp.StatusCode)
	}
	_ = os.MkdirAll(config.DownloadsDir, 0755)
	f, err := os.Create(outPath)
	if err != nil {
		return "", fmt.Errorf("arc API: create file: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err = io.Copy(f, resp.Body); err != nil {
		_ = os.Remove(outPath)
		return "", fmt.Errorf("arc API: save file: %w", err)
	}
	slog.Info("[ArcAPI] download complete", "path", outPath)
	return outPath, nil
}

// ── Media-channel (direct DB) download ───────────────────────────────────────
// Mirrors tosu4 _download_from_media_db: looks up the track in MongoDB (arcapi.medias),
// then downloads the cached Telegram message via DlBot.

// downloadFromMediaChannel tries to fetch a previously cached media file from the
// configured MEDIA_CHANNEL_ID using the DlBot client.
func downloadFromMediaChannel(videoID string, isVideo bool) (string, error) {
	if DlBot == nil || config.MediaChannelId == 0 {
		return "", errors.New("media channel not configured")
	}

	ext := "mp3"
	if isVideo {
		ext = "mp4"
	}
	outPath := filepath.Join(config.DownloadsDir, videoID+"."+ext)
	if info, err := os.Stat(outPath); err == nil && info.Size() > 0 {
		return outPath, nil
	}

	// Look up message ID in the media-DB collection (MongoDB arcapi.medias)
	msgID, err := lookupMediaDB(videoID, isVideo)
	if err != nil || msgID == 0 {
		return "", errors.New("not in media DB")
	}

	_ = os.MkdirAll(config.DownloadsDir, 0755)
	msgs, err := DlBot.GetMessages(config.MediaChannelId, []int32{int32(msgID)}, nil)
	if err != nil || len(msgs.Messages) == 0 {
		return "", fmt.Errorf("media channel get message failed: %w", err)
	}

	file, err := msgs.Messages[0].Download(DlBot, 1, 0, 0, true)
	if err != nil || file == nil || file.Local == nil || file.Local.Path == "" {
		return "", fmt.Errorf("media channel download failed: %w", err)
	}

	localPath := file.Local.Path
	if localPath != outPath {
		if err := os.Rename(localPath, outPath); err != nil {
			return localPath, nil // return what we have
		}
	}
	return outPath, nil
}

// lookupMediaDB checks the MongoDB arcapi.medias collection for a cached message ID.
// This is a lightweight HTTP call to a simple lookup endpoint exposed by the bot itself
// or read directly via the MongoURI. For now we use a best-effort approach: if
// MONGO_URI is set we query directly via the go mongo driver.
func lookupMediaDB(trackID string, isVideo bool) (int64, error) {
	// Prefer a cached lookup via the existing mongo connection in db package.
	// Imported lazily to avoid circular deps; returns 0 if not found.
	return mediaDBLookup(trackID, isVideo)
}

// buildYtdlpParams constructs the command-line parameters for yt-dlp to download media.
func (y *youTubeData) buildYtdlpParams(videoID string, video bool) ([]string, string) {
	outputTemplate := filepath.Join(config.DownloadsDir, "%(id)s.%(ext)s")
	var cookieFile string

	params := []string{
		"yt-dlp",
		"--no-warnings",
		"--quiet",
		"--geo-bypass",
		"--retries", "2",
		"--continue",
		"--no-part",
		"--concurrent-fragments", "3",
		"--socket-timeout", "10",
		"--throttled-rate", "100K",
		"--retry-sleep", "1",
		"--no-write-thumbnail",
		"--no-write-info-json",
		"--no-embed-metadata",
		"--no-embed-chapters",
		"--no-embed-subs",
		"--extractor-args", "youtube:player_js_version=actual",
		"-o", outputTemplate,
	}

	if video {
		formatSelector := "bestvideo[height<=720]+bestaudio/best[height<=720]"
		params = append(params, "-f", formatSelector, "--merge-output-format", "mp4")
	} else {
		params = append(params, "-f", "bestaudio[ext=m4a]/bestaudio")
	}

	cookieFile = y.getCookieFile()
	if cookieFile != "" {
		params = append(params, "--cookies", cookieFile)
	} else if config.Proxy != "" {
		params = append(params, "--proxy", config.Proxy)
	}

	videoURL := "https://www.youtube.com/watch?v=" + videoID
	params = append(params, videoURL, "--print", "after_move:filepath")

	return params, cookieFile
}

// downloadWithYtDlp downloads media from YouTube using the yt-dlp command-line tool.
func (y *youTubeData) downloadWithYtDlp(videoID string, video bool) (string, error) {
	if videoID == "" {
		return "", errors.New("videoID is empty")
	}

	ytdlpParams, cookieFile := y.buildYtdlpParams(videoID, video)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, ytdlpParams[0], ytdlpParams[1:]...)

	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr := string(exitErr.Stderr)
			if cookieFile != "" && strings.Contains(stderr, "Sign in to confirm you're not a bot") {
				_ = os.Remove(cookieFile)
			}
			return "", fmt.Errorf("yt-dlp failed with exit code %d: %s", exitErr.ExitCode(), stderr)
		}

		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("yt-dlp timed out for video ID: %s", videoID)
		}

		return "", fmt.Errorf("an unexpected error occurred while downloading %s: %w", videoID, err)
	}

	downloadedPathStr := strings.TrimSpace(string(output))
	if downloadedPathStr == "" {
		return "", fmt.Errorf("no output path was returned for %s", videoID)
	}

	if _, err := os.Stat(downloadedPathStr); os.IsNotExist(err) {
		return "", fmt.Errorf("the file was not found at the reported path: %s", downloadedPathStr)
	}

	return downloadedPathStr, nil
}

// getCookieFile retrieves the path to a cookie file from the configured list.
func (y *youTubeData) getCookieFile() string {
	cookiesPath := config.CookiesPath
	if len(cookiesPath) == 0 {
		return ""
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(cookiesPath))))
	if err != nil {
		slog.Info("Could not generate a random number", "error", err)
		return cookiesPath[0]
	}

	return cookiesPath[n.Int64()]
}

// downloadWithApi is kept for backward compatibility but now delegates to downloadTrack.
func (y *youTubeData) downloadWithApi(videoID string, video bool) (string, error) {
	return y.downloadWithArcAPI(videoID, video)
}
