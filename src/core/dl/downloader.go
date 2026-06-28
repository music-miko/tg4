/*
 * TgMusicBot - Telegram Music Bot
 *  Copyright (c) 2025-2026 Ashok Shau
 *
 *  Licensed under GNU GPL v3
 *  See https://github.com/AshokShau/TgMusicBot
 */

package dl

import (
	"ashokshau/tgmusic/src/utils"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	td "github.com/AshokShau/gotdbot"
)

func DownloadCachedTrack(cached *utils.CachedTrack, bot *td.Client) (string, error) {
	if cached.Platform == utils.DirectLink {
		return cached.URL, nil
	}

	if cached.Platform == utils.Telegram {
		path, err := downloadTelegramFile(cached, bot)
		if err != nil {
			return "", err
		}
		return normalizeFileExtension(path), nil
	}

	dlBot := bot
	if DlBot != nil {
		dlBot = DlBot
	}

	path, err := downloadViaWrapper(cached, dlBot)
	if err != nil {
		return "", err
	}
	return normalizeFileExtension(path), nil
}

func downloadViaWrapper(cached *utils.CachedTrack, dlBot *td.Client) (string, error) {
	wrapper := NewDownloaderWrapper(cached.URL)
	if !wrapper.IsValid() {
		return "", fmt.Errorf("invalid cached URL: %s", cached.URL)
	}

	track, err := wrapper.GetTrack()
	if err != nil {
		return "", fmt.Errorf("get track info: %w", err)
	}

	path, err := wrapper.DownloadTrack(track, cached.IsVideo)
	if err != nil {
		return "", err
	}

	if utils.TelegramMessageRegex.MatchString(path) {
		return downloadFromTelegramMessage(dlBot, path)
	}

	return path, nil
}

func downloadTelegramFile(cached *utils.CachedTrack, bot *td.Client) (string, error) {
	file, err := bot.GetRemoteFile(cached.TrackID, nil)
	if err != nil {
		return "", err
	}

	download, err := file.Download(bot, 0, 0, 1, &td.DownloadFileOpts{Synchronous: true})
	if err != nil {
		return "", err
	}

	return verifyDownloadedFile(download)
}

func downloadFromTelegramMessage(bot *td.Client, msgURL string) (string, error) {
	msg, err := utils.GetMessage(bot, msgURL)
	if err != nil {
		return "", fmt.Errorf("get telegram message: %w", err)
	}

	file, err := msg.Download(bot, 1, 0, 0, true)
	if err != nil {
		return "", err
	}

	return verifyDownloadedFile(file)
}

// verifyDownloadedFile checks that a Telegram file download actually finished.
// A synchronous DownloadFile request can still return successfully with a
// partial file (e.g. on a dropped connection, FLOOD_WAIT, or an expired file
// reference) - the request succeeds but file.Local.IsDownloadingCompleted is
// false and only a prefix of the bytes are on disk. Trusting Local.Path alone
// in that case hands a truncated file to ffmpeg, which plays it part-way and
// then reports end-of-stream as if the track had finished normally.
func verifyDownloadedFile(file *td.File) (string, error) {
	if file == nil || file.Local == nil || file.Local.Path == "" {
		return "", fmt.Errorf("failed to download file from Telegram: no local file was returned")
	}

	if !file.Local.IsDownloadingCompleted {
		return "", fmt.Errorf(
			"telegram download did not finish (got %d of %d bytes): %s",
			file.Local.DownloadedSize, file.Size, file.Local.Path,
		)
	}

	if info, err := os.Stat(file.Local.Path); err != nil {
		return "", fmt.Errorf("downloaded file is missing on disk: %w", err)
	} else if file.Size > 0 && info.Size() < file.Size {
		return "", fmt.Errorf(
			"downloaded file is smaller than expected (got %d of %d bytes): %s",
			info.Size(), file.Size, file.Local.Path,
		)
	}

	return file.Local.Path, nil
}

// extensionsByFormat maps ffprobe's format_name output to the file extension
// that best represents it for playback purposes. ffprobe's format_name can
// list several aliases separated by commas (e.g. "matroska,webm",
// "mov,mp4,m4a,3gp,3g2,mj2"); each entry below is checked as a substring.
var extensionsByFormat = []struct {
	formatSubstr string
	ext          string
}{
	{"webm", ".webm"},
	{"matroska", ".webm"},
	{"mp4", ".m4a"},
	{"mov", ".m4a"},
	{"ogg", ".ogg"},
	{"mp3", ".mp3"},
	{"wav", ".wav"},
	{"flac", ".flac"},
}

// detectRealExtension runs ffprobe against a file's actual content and
// returns the file extension that matches its real container format, or ""
// if detection fails or the format isn't recognised. This exists because
// some upstream sources (e.g. a shared media cache populated by an external
// service) save files under a filename whose extension doesn't match the
// file's actual content - for example, a WebM/Opus stream saved as ".mp3".
// ffmpeg's own format probing is normally content-based and tolerates this,
// but a mismatched extension is an unnecessary risk for the shell-spawned
// reader ntgcalls uses for playback, so files are renamed to match their
// real content before being handed off to it.
func detectRealExtension(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_entries", "format=format_name",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		slog.Warn("detectRealExtension: ffprobe failed", "path", path, "error", err, "stderr", strings.TrimSpace(stderr.String()))
		return ""
	}

	formatName := strings.ToLower(strings.TrimSpace(stdout.String()))
	if formatName == "" {
		return ""
	}

	for _, entry := range extensionsByFormat {
		if strings.Contains(formatName, entry.formatSubstr) {
			return entry.ext
		}
	}

	slog.Info("detectRealExtension: unrecognised format, leaving extension unchanged", "path", path, "format_name", formatName)
	return ""
}

// normalizeFileExtension checks a downloaded file's real container format
// and renames it to match if its current extension doesn't already agree.
// On any failure (probe error, rename error, unrecognised format, or a
// remote URL/non-local path) it returns the original path unchanged, since
// this is a best-effort correctness improvement, not a required step.
func normalizeFileExtension(path string) string {
	if path == "" || isURL(path) {
		return path
	}

	realExt := detectRealExtension(path)
	if realExt == "" {
		return path
	}

	currentExt := strings.ToLower(filepath.Ext(path))
	if currentExt == realExt {
		return path
	}

	newPath := strings.TrimSuffix(path, filepath.Ext(path)) + realExt
	if err := os.Rename(path, newPath); err != nil {
		slog.Warn("normalizeFileExtension: rename failed, keeping original path",
			"old_path", path, "new_path", newPath, "error", err)
		return path
	}

	slog.Info("normalizeFileExtension: corrected mismatched file extension",
		"old_path", path, "new_path", newPath, "real_format", realExt)
	return newPath
}

// isURL reports whether a path is actually a remote URL rather than a local
// filesystem path, so normalizeFileExtension can skip those untouched.
func isURL(path string) bool {
	return strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://")
}
