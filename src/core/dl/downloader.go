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
	"fmt"
	"os"

	td "github.com/AshokShau/gotdbot"
)

func DownloadCachedTrack(cached *utils.CachedTrack, bot *td.Client) (string, error) {
	if cached.Platform == utils.DirectLink {
		return cached.URL, nil
	}

	if cached.Platform == utils.Telegram {
		return downloadTelegramFile(cached, bot)
	}

	dlBot := bot
	if DlBot != nil {
		dlBot = DlBot
	}

	return downloadViaWrapper(cached, dlBot)
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
