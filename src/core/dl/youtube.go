/*
 * TgMusicBot - Telegram Music Bot
 *  Copyright (c) 2025-2026 Ashok Shau
 *
 *  Licensed under GNU GPL v3
 *  See https://github.com/AshokShau/TgMusicBot
 */

package dl

import (
	"time"

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
		ApiUrl:   strings.TrimRight(config.ArcApiUrl, "/"),
		APIKey:   config.ArcApiKey,
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

// downloadTrack handles the download of a track from YouTube.
// Priority order: media channel DB cache → Arc API v2 → yt-dlp fallback
func (y *youTubeData) downloadTrack(info utils.TrackInfo, video bool) (string, error) {
	videoID := info.Id

	// 1. Try media channel DB cache (DB_URI + MEDIA_CHANNEL_ID)
	if config.DbUri != "" && config.MediaChannelId != 0 {
		if filePath, err := downloadFromMediaDB(videoID, video); err == nil && filePath != "" {
			slog.Info("[YouTube] MediaDB cache hit", "video_id", videoID)
			return filePath, nil
		}
	}

	// 2. Try Arc API v2 (job-based download)
	if y.ApiUrl != "" && y.APIKey != "" {
		if filePath, err := y.downloadWithArcAPI(videoID, video); err == nil && filePath != "" {
			slog.Info("[YouTube] Arc API download success", "video_id", videoID)
			return filePath, nil
		}
		slog.Warn("[YouTube] Arc API failed, falling back to yt-dlp", "video_id", videoID)
	}

	// 3. Fallback: yt-dlp
	return y.downloadWithYtDlp(videoID, video)
}

// ── Arc API v2 (job-based) ────────────────────────────────────────────────────

const (
	arcV2CreateRetries = 3
	arcV2PollRetries   = 12
	arcV2Cycles        = 2
	arcPollInterval    = 3 * time.Second
)

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

// downloadWithArcAPI downloads a YouTube track via Arc API v2 job flow.
func (y *youTubeData) downloadWithArcAPI(videoID string, video bool) (string, error) {
	ext := "m4a"
	if video {
		ext = "mp4"
	}

	outPath := filepath.Join(config.DownloadsDir, fmt.Sprintf("%s.%s", videoID, ext))

	// Return cached local file if it exists and has content
	if stat, err := os.Stat(outPath); err == nil && stat.Size() > 0 {
		slog.Info("[ArcAPI] Local file cache hit", "path", outPath)
		return outPath, nil
	}

	for cycle := 0; cycle < arcV2Cycles; cycle++ {
		// Step 1: Create download job
		jobID, err := y.arcCreateJob(videoID, video)
		if err != nil || jobID == "" {
			slog.Error("[ArcAPI] create_job failed", "cycle", cycle+1, "video_id", videoID, "error", err)
			if cycle == 0 {
				time.Sleep(2 * time.Second)
			}
			continue
		}

		// Step 2: Poll for job completion
		dlURL, err := y.arcPollJob(jobID)
		if err != nil || dlURL == "" {
			slog.Error("[ArcAPI] poll_job failed", "cycle", cycle+1, "job_id", jobID, "error", err)
			if cycle == 0 {
				time.Sleep(2 * time.Second)
			}
			continue
		}

		// Step 3: Download the file
		filePath, err := downloadFile(dlURL, outPath, false)
		if err != nil || filePath == "" {
			slog.Error("[ArcAPI] save_file failed", "cycle", cycle+1, "url", dlURL, "error", err)
			if cycle == 0 {
				time.Sleep(2 * time.Second)
			}
			continue
		}

		return filePath, nil
	}

	return "", fmt.Errorf("arc API: all %d cycles failed for video %s", arcV2Cycles, videoID)
}

func (y *youTubeData) arcCreateJob(videoID string, video bool) (string, error) {
	endpoint := fmt.Sprintf("%s/youtube/v2/download", y.ApiUrl)
	params := url.Values{
		"api_key": {y.APIKey},
		"query":   {videoID},
		"isVideo": {fmt.Sprintf("%v", video)},
	}

	for attempt := 0; attempt < arcV2CreateRetries; attempt++ {
		resp, err := sendRequest(http.MethodGet, endpoint+"?"+params.Encode(), nil, nil)
		if err != nil {
			slog.Warn("[ArcAPI] create_job request error", "attempt", attempt+1, "error", err)
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
		if err := json.Unmarshal(body, &data); err != nil {
			slog.Warn("[ArcAPI] create_job JSON parse error", "attempt", attempt+1, "error", err)
			time.Sleep(time.Second)
			continue
		}

		if data.Status != "queued" || data.JobID == "" {
			slog.Warn("[ArcAPI] create_job unexpected status", "status", data.Status, "attempt", attempt+1)
			time.Sleep(time.Second)
			continue
		}

		return data.JobID, nil
	}

	return "", fmt.Errorf("arc API: create_job exhausted %d retries", arcV2CreateRetries)
}

func (y *youTubeData) arcPollJob(jobID string) (string, error) {
	endpoint := fmt.Sprintf("%s/youtube/jobStatus", y.ApiUrl)
	params := url.Values{"job_id": {jobID}}

	for attempt := 1; attempt <= arcV2PollRetries; attempt++ {
		resp, err := sendRequest(http.MethodGet, endpoint+"?"+params.Encode(), nil, nil)
		if err != nil {
			slog.Warn("[ArcAPI] poll_job request error", "attempt", attempt, "error", err)
			time.Sleep(arcPollInterval)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			time.Sleep(arcPollInterval)
			continue
		}

		var data arcJobStatusResponse
		if err := json.Unmarshal(body, &data); err != nil {
			time.Sleep(arcPollInterval)
			continue
		}

		if data.Status != "success" || data.Job.Status != "done" {
			time.Sleep(arcPollInterval)
			continue
		}

		publicURL := data.Job.Result.PublicURL
		if publicURL == "" {
			slog.Warn("[ArcAPI] poll_job: no public_url in response")
			break
		}

		if strings.HasPrefix(publicURL, "/") {
			publicURL = y.ApiUrl + publicURL
		}

		slog.Info("[ArcAPI] Download URL ready", "attempt", attempt)
		return publicURL, nil
	}

	return "", fmt.Errorf("arc API: poll_job exhausted %d retries for job %s", arcV2PollRetries, jobID)
}

// ── Media channel DB cache (DB_URI + MEDIA_CHANNEL_ID) ───────────────────────

type mediaDBRecord struct {
	TrackID   string `bson:"track_id" json:"track_id"`
	IsVideo   bool   `bson:"isVideo"   json:"isVideo"`
	MessageID int64  `bson:"message_id" json:"message_id"`
}

// downloadFromMediaDB tries to fetch a pre-uploaded file from the Telegram media channel.
// It looks up the message ID from MongoDB (DB_URI) and then downloads from MEDIA_CHANNEL_ID.
func downloadFromMediaDB(videoID string, isVideo bool) (string, error) {
	if DlBot == nil {
		return "", errors.New("download bot not initialized")
	}

	ext := "mp3"
	if isVideo {
		ext = "mp4"
	}

	finalPath := filepath.Join(config.DownloadsDir, fmt.Sprintf("%s.%s", videoID, ext))
	if stat, err := os.Stat(finalPath); err == nil && stat.Size() > 0 {
		slog.Info("[MediaDB] Local cache hit", "video_id", videoID)
		return finalPath, nil
	}

	msgID, err := lookupMediaDBMessageID(videoID, isVideo)
	if err != nil || msgID == 0 {
		return "", fmt.Errorf("media DB: no record for %s", videoID)
	}

	slog.Info("[MediaDB] Found message in channel, downloading", "video_id", videoID, "msg_id", msgID)

	// t.me/c/ requires the numeric channel ID without the -100 prefix
	channelIDStr := fmt.Sprintf("%d", config.MediaChannelId)
	channelIDStr = strings.TrimPrefix(channelIDStr, "-100")
	msgURL := fmt.Sprintf("https://t.me/c/%s/%d", channelIDStr, msgID)
	path, err := downloadFromTelegramMessage(DlBot, msgURL)
	if err != nil {
		return "", fmt.Errorf("media DB: download from channel failed: %w", err)
	}

	// Rename to standard path if different
	if path != finalPath {
		if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err == nil {
			_ = os.Rename(path, finalPath)
			path = finalPath
		}
	}

	return path, nil
}

// lookupMediaDBMessageID queries MongoDB via DB_URI for the cached message ID of a track.
// This is a lightweight HTTP call to an internal helper or direct mongo — we use the utils package.
func lookupMediaDBMessageID(trackID string, isVideo bool) (int64, error) {
	// We access DB_URI from config and query the arcapi.medias collection.
	// Since this is Go and we don't want to add a full MongoDB driver just for this,
	// we delegate to utils.LookupMediaCache which handles the DB call.
	return utils.LookupMediaCache(config.DbUri, trackID, isVideo)
}

// ── yt-dlp fallback ───────────────────────────────────────────────────────────

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
