/*
 * TgMusicBot - Telegram Music Bot
 *  Copyright (c) 2025-2026 Ashok Shau
 *
 *  Licensed under GNU GPL v3
 *  See https://github.com/AshokShau/TgMusicBot
 */

package dl

import (
	"ashokshau/tgmusic/config"
	"ashokshau/tgmusic/src/utils"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	defaultFilePerm = 0644
)

var (
	errMissingKey    = errors.New("missing CDN key")
	errFileNotFound  = errors.New("file not found")
	errInvalidHexKey = errors.New("invalid hex key")
	errInvalidAESIV  = errors.New("invalid AES IV")
)

// uniqueSpotifyTrackID returns a filesystem-safe identifier for a Spotify
// track's local cache filename. track.Id is normally used, but some API
// gateways don't populate "id" on /api/track responses for Spotify — when
// that happens every track in a playlist collapses to the same filename
// (e.g. "<dir>/.ogg"), and the existence check above causes every track
// after the first to silently reuse the first track's cached file instead
// of downloading its own. This is exactly why Spotify playlists were stuck
// replaying the first song instead of advancing. Falling back to a hash of
// the track's URL (or CDN URL) guarantees a unique, stable filename per
// track even when Id is missing.
func uniqueSpotifyTrackID(track utils.TrackInfo) string {
	if id := filepath.Base(track.Id); id != "" && id != "." && id != string(filepath.Separator) {
		return id
	}

	seed := track.URL
	if seed == "" {
		seed = track.CdnURL
	}
	if seed == "" {
		seed = track.Platform + track.Key
	}

	sum := sha1.Sum([]byte(seed))
	return hex.EncodeToString(sum[:])[:16]
}

// processSpotify manages the download and decryption of Spotify tracks.
func (d *download) processSpotify() (string, error) {
	track := d.Track
	downloadsDir := config.DownloadsDir
	sanitizedTrackID := uniqueSpotifyTrackID(track)

	outputFile := filepath.Join(downloadsDir, fmt.Sprintf("%s.ogg", sanitizedTrackID))
	if _, err := os.Stat(outputFile); err == nil {
		slog.Info("The file already exists", "arg1", outputFile)
		return outputFile, nil
	}

	if track.Key == "" {
		return "", errMissingKey
	}

	startTime := time.Now()
	defer func() {
		slog.Info("The process was completed in .", "duration", time.Since(startTime))
	}()

	encryptedFile := filepath.Join(downloadsDir, fmt.Sprintf("%s.encrypted", sanitizedTrackID))
	decryptedFile := filepath.Join(downloadsDir, fmt.Sprintf("%s_decrypted.ogg", sanitizedTrackID))

	defer func() {
		_ = os.Remove(encryptedFile)
		_ = os.Remove(decryptedFile)
	}()

	if err := d.downloadAndDecrypt(encryptedFile, decryptedFile); err != nil {
		slog.Info("Failed to download and decrypt the file", "error", err)
		return "", err
	}

	if err := rebuildOGG(decryptedFile); err != nil {
		slog.Info("Failed to rebuild the OGG headers", "error", err)
	}

	return fixOGG(decryptedFile, track)
}

// downloadAndDecrypt handles the download and decryption of a file.
func (d *download) downloadAndDecrypt(encryptedPath, decryptedPath string) error {
	resp, err := http.Get(d.Track.CdnURL)
	if err != nil {
		return fmt.Errorf("failed to download the file: %w", err)
	}

	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read the response body: %w", err)
	}

	if err := os.WriteFile(encryptedPath, data, defaultFilePerm); err != nil {
		return fmt.Errorf("failed to write the encrypted file: %w", err)
	}

	decryptedData, decryptTime, err := decryptAudioFile(encryptedPath, d.Track.Key)
	if err != nil {
		return fmt.Errorf("failed to decrypt the audio file: %w", err)
	}

	slog.Info("Decryption was completed in .", "duration", decryptTime)
	return os.WriteFile(decryptedPath, decryptedData, defaultFilePerm)
}

// decryptAudioFile decrypts an audio file using AES-CTR encryption.
func decryptAudioFile(filePath, hexKey string) ([]byte, string, error) {
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		return nil, "", fmt.Errorf("%w: %s", errFileNotFound, filePath)
	}

	key, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errInvalidHexKey, err)
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read the file: %w", err)
	}

	audioAesIv, err := hex.DecodeString("72e067fbddcbcf77ebe8bc643f630d93")
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errInvalidAESIV, err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create the AES cipher: %w", err)
	}

	startTime := time.Now()
	ctr := cipher.NewCTR(block, audioAesIv)
	decrypted := make([]byte, len(data))
	ctr.XORKeyStream(decrypted, data)

	return decrypted, fmt.Sprintf("%dms", time.Since(startTime).Milliseconds()), nil
}

// rebuildOGG reconstructs the OGG header of a given file by patching specific offsets.
func rebuildOGG(filename string) error {
	file, err := os.OpenFile(filename, os.O_RDWR, defaultFilePerm)
	if err != nil {
		return fmt.Errorf("error opening the file: %w", err)
	}
	defer func(file *os.File) {
		_ = file.Close()
	}(file)

	writeAt := func(offset int64, data string) error {
		_, err := file.WriteAt([]byte(data), offset)
		return err
	}

	patches := map[int64]string{
		0:  "OggS",
		6:  "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00",
		26: "\x01\x1E\x01vorbis",
		39: "\x02",
		40: "\x44\xAC\x00\x00",
		48: "\x00\xE2\x04\x00",
		56: "\xB8\x01",
		58: "OggS",
		62: "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00",
	}

	for offset, data := range patches {
		if err := writeAt(offset, data); err != nil {
			return fmt.Errorf("failed to write at offset %d: %w", offset, err)
		}
	}

	return nil
}

// fixOGG uses ffmpeg to correct any remaining issues in the OGG file, ensuring it is playable.
//
// This re-encodes (rather than stream-copies) the audio. rebuildOGG only
// patches a handful of fixed byte offsets assuming a specific header
// layout; "-c copy" would just repackage those pages as-is without
// validating that the resulting bitstream is actually well-formed
// end-to-end. If it isn't, the corruption doesn't show up here — it shows
// up later when ntgcalls' ffmpeg pipe (getMediaDescription) tries to
// stream it live during actual voice-chat playback, where a mid-stream
// decode failure can make the native layer silently die/restart on the
// same input with no Go-level callback (no OnStreamEnd, no "Started
// streaming" message, track just plays from position 0 again — which
// matches the reported symptom). Forcing ffmpeg to fully decode+re-encode
// here means any structural problem surfaces immediately as a caching
// error we can log and retry, instead of as a silent failure mid-stream.
func fixOGG(inputFile string, track utils.TrackInfo) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sanitizedTrackID := uniqueSpotifyTrackID(track)
	outputFile := filepath.Join(config.DownloadsDir, fmt.Sprintf("%s.ogg", sanitizedTrackID))
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", inputFile, "-c:a", "libvorbis", "-qscale:a", "6", "-vn", outputFile)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("ffmpeg failed with error: %w\nOutput: %s", err, string(output))
	}

	if dur := utils.GetMediaDuration(outputFile); dur <= 0 {
		_ = os.Remove(outputFile)
		return "", fmt.Errorf("fixOGG produced an unplayable file for track %q (probed duration was 0)", track.Id)
	}

	return outputFile, nil
}
