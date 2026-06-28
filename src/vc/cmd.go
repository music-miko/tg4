package vc

import (
	"ashokshau/tgmusic/src/vc/ntgcalls"
	"fmt"
	"regexp"
	"strings"
)

var isURLRegex = regexp.MustCompile(`^https?://`)

// getMediaDescription creates a media description for ntgcalls based on the provided file path, video status, and ffmpeg parameters.
func getMediaDescription(filePath string, isVideo bool, ffmpegParameters string) ntgcalls.MediaDescription {
	audioDescription := &ntgcalls.AudioDescription{
		MediaSource:  ntgcalls.MediaSourceShell,
		SampleRate:   48000,
		ChannelCount: 2,
		// KeepOpen must be true — without it ntgcalls closes the pipe as soon
		// as the first internal read completes, cutting the stream short.
		KeepOpen: true,
	}

	quotedPath := fmt.Sprintf("\"%s\"", filePath)
	isURL := isURLRegex.MatchString(filePath)

	// Separate seek/position flags (go BEFORE -i) from filter flags (go AFTER -i).
	// ffmpegParameters may be either:
	//   • seek flags:   "-ss <n> -to <n>"   (from SeekStream)
	//   • filter flags: "-filter:v ... -filter:a ..."  (from ChangeSpeed)
	var preInputFlags, postInputFlags string
	if ffmpegParameters != "" {
		if strings.Contains(ffmpegParameters, "filter:") {
			postInputFlags = ffmpegParameters
		} else {
			preInputFlags = ffmpegParameters
		}
	}

	// --- Audio command ---
	var audioCmd strings.Builder
	audioCmd.WriteString("ffmpeg ")
	if isURL {
		audioCmd.WriteString("-reconnect 1 -reconnect_at_eof 1 -reconnect_streamed 1 -reconnect_delay_max 2 ")
	}
	if preInputFlags != "" {
		audioCmd.WriteString(preInputFlags + " ")
	}
	audioCmd.WriteString("-i " + quotedPath + " ")
	if postInputFlags != "" {
		audioCmd.WriteString(postInputFlags + " ")
	}
	audioCmd.WriteString(fmt.Sprintf("-f s16le -ac %d -ar %d -loglevel warning pipe:1",
		audioDescription.ChannelCount,
		audioDescription.SampleRate,
	))
	audioDescription.Input = audioCmd.String()

	if !isVideo {
		return ntgcalls.MediaDescription{
			Microphone: audioDescription,
		}
	}

	// --- Video dimensions ---
	originalWidth, originalHeight := getVideoDimensions(filePath)

	width := 1280
	height := 720

	if originalWidth > 0 && originalHeight > 0 {
		ratio := float64(originalWidth) / float64(originalHeight)
		newW := min(originalWidth, width)
		newH := int(float64(newW) / ratio)

		if newH > height {
			newH = height
			newW = int(float64(newH) * ratio)
		}

		if newW%2 != 0 {
			newW--
		}
		if newH%2 != 0 {
			newH--
		}

		width = newW
		height = newH
	}

	videoDescription := &ntgcalls.VideoDescription{
		MediaSource: ntgcalls.MediaSourceShell,
		Width:       int16(width),
		Height:      int16(height),
		Fps:         30,
		// KeepOpen must be true on the video track too — an early close on
		// the video pipe causes ntgcalls to fire an audio StreamEnd prematurely.
		KeepOpen: true,
	}

	// --- Video command ---
	var videoCmd strings.Builder
	videoCmd.WriteString("ffmpeg ")
	if isURL {
		videoCmd.WriteString("-reconnect 1 -reconnect_at_eof 1 -reconnect_streamed 1 -reconnect_delay_max 2 ")
	}
	if preInputFlags != "" {
		videoCmd.WriteString(preInputFlags + " ")
	}
	videoCmd.WriteString("-i " + quotedPath + " ")
	if postInputFlags != "" {
		videoCmd.WriteString(postInputFlags + " ")
	}
	videoCmd.WriteString(fmt.Sprintf("-f rawvideo -r %d -pix_fmt yuv420p -vf scale=%d:%d -loglevel warning pipe:1",
		videoDescription.Fps,
		videoDescription.Width,
		videoDescription.Height,
	))
	videoDescription.Input = videoCmd.String()

	return ntgcalls.MediaDescription{
		Microphone: audioDescription,
		Camera:     videoDescription,
	}
}
