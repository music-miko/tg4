/*
 * TgMusicBot - Telegram Music Bot
 *  Copyright (c) 2025-2026 Ashok Shau
 *
 *  Licensed under GNU GPL v3
 *  See https://github.com/AshokShau/TgMusicBot
 */

package handlers

import (
	"ashokshau/tgmusic/config"
	"fmt"
	"runtime"
	"strings"
	"time"

	"ashokshau/tgmusic/src/core"
	"ashokshau/tgmusic/src/core/db"

	td "github.com/AshokShau/gotdbot"
)

// pingHandler handles the /ping command.
func pingHandler(c *td.Client, m *td.Message) error {

	start := time.Now()

	msg, err := m.ReplyText(c, "Pinging… please wait…", nil)
	if err != nil {
		return err
	}

	latency := time.Since(start).Milliseconds()
	uptime := getFormattedDuration(time.Since(startTime))

	response := fmt.Sprintf(
		"<b>📊 System Performance Metrics</b>\n\n"+
			"<b>Bot Latency:</b> <code>%d ms</code>\n"+
			"<b>Uptime:</b> <code>%s</code>\n"+
			"<b>Go Routines:</b> <code>%d</code>\n"+
			"<b>Version:</b> <code>%s</code>\n",
		latency, uptime, runtime.NumGoroutine(), config.Version,
	)

	_, err = msg.EditText(c, response, &td.EditTextMessageOpts{ParseMode: "HTML"})
	return err
}

// startHandler handles the /start command.
func startHandler(c *td.Client, m *td.Message) error {
	chatID := m.ChatId

	if m.IsPrivate() {
		go func(chatID int64) {
			_ = db.Instance.AddUser(chatID)
		}(chatID)

		response := fmt.Sprintf(
			"👋 Hello, %s.\n\n%s is a music bot for Telegram — stream from YouTube, Spotify, Apple Music, SoundCloud, Deezer, JioSaavn and more, right inside any group voice chat.\n\nUse /help to explore all commands.",
			firstName(c, m),
			c.Me.FirstName,
		)

		_, err := m.ReplyPhoto(c, td.InputFileRemote{Id: config.StartImg}, &td.SendPhotoOpts{
			ParseMode:   "HTML",
			Caption:     response,
			ReplyMarkup: core.PrivateStartMarkup(c.Me.Usernames.EditableUsername),
		})

		return err
	}

	go func(chatID int64) {
		_ = db.Instance.AddChat(chatID)
	}(chatID)

	uptime := getFormattedDuration(time.Since(startTime))
	response := fmt.Sprintf(
		"👋<b>%s is ready</b>\n\n<b>Uptime:</b> <code>%s</code>\n\nA music player with support for YouTube, Spotify, Apple Music, SoundCloud, Deezer, JioSaavn and more.",
		c.Me.FirstName,
		uptime,
	)

	_, err := m.ReplyText(c, response, &td.SendTextMessageOpts{
		ParseMode:             "HTML",
		DisableWebPagePreview: true,
		ReplyMarkup:           core.GroupWelcomeMarkup(),
	})

	return err
}

// setupGuideText returns the step-by-step setup guide shown via the Setup Guide button.
func setupGuideText(botName string) string {
	return fmt.Sprintf(
		"🅰️<b> Setup Guide</b>\n\n"+
			"Get %s up and running in your group in under a minute:\n\n"+
			"<b>Step 1 — Add the bot</b>\n"+
			"Tap <b>Add to Group</b> and select your group.\n\n"+
			"<b>Step 2 — Promote the bot</b>\n"+
			"Make %s an admin and grant the <b>Invite Users via Link</b> (also shown as <b>Add Users</b>) permission. This is required for voice chats.\n\n"+
			"<b>Step 3 — Start a voice chat</b>\n"+
			"Open your group and start a video/voice chat.\n\n"+
			"<b>Step 4 — Play music</b>\n"+
			"Use <code>/play song name</code> or <code>/vplay song name</code> for video.\n\n"+
			"Example: <code>/play shape of you</code>\n\n"+
			"That's it — enjoy the music! 🎶",
		botName, botName,
	)
}

// setupCallbackHandler handles the Setup Guide button and its Back navigation.
func setupCallbackHandler(c *td.Client, cb *td.UpdateNewCallbackQuery) error {
	data := cb.DataString()

	msg, _ := cb.GetMessage(c)
	isPhoto := false
	if msg != nil && msg.Content != nil {
		if _, ok := msg.Content.(*td.MessagePhoto); ok {
			isPhoto = true
		}
	}

	switch {
	case strings.Contains(data, "setup_guide"):
		_ = cb.Answer(c, 0, false, "Opening setup guide...", "")
		text := setupGuideText(c.Me.FirstName)

		if isPhoto {
			_, err := cb.EditMessageCaption(c, text, &td.EditCaptionOpts{ReplyMarkup: core.GuideBackMarkup(), ParseMode: "HTML"})
			return err
		}

		_, err := cb.EditMessageText(c, text, &td.EditTextMessageOpts{ReplyMarkup: core.GuideBackMarkup(), ParseMode: "HTML", DisableWebPagePreview: true})
		return err

	case strings.Contains(data, "setup_back"):
		_ = cb.Answer(c, 0, false, "Returning...", "")

		if isPhoto {
			user, err := c.GetUser(cb.SenderUserId)
			name := "there"
			if err == nil && user != nil {
				name = user.FirstName
			}
			text := fmt.Sprintf(
				"👋 Hello, %s.\n\n%s is a music bot for Telegram — stream from YouTube, Spotify, Apple Music, SoundCloud, Deezer, JioSaavn and more, right inside any group voice chat.\n\nUse /help to explore all commands.",
				name, c.Me.FirstName,
			)
			_, err = cb.EditMessageCaption(c, text, &td.EditCaptionOpts{ReplyMarkup: core.PrivateStartMarkup(c.Me.Usernames.EditableUsername), ParseMode: "HTML"})
			return err
		}

		uptime := getFormattedDuration(time.Since(startTime))
		text := fmt.Sprintf(
			"👋<b>%s is ready</b>\n\n<b>Uptime:</b> <code>%s</code>\n\nA music player with support for YouTube, Spotify, Apple Music, SoundCloud, Deezer, JioSaavn and more.",
			c.Me.FirstName, uptime,
		)
		_, err := cb.EditMessageText(c, text, &td.EditTextMessageOpts{ReplyMarkup: core.GroupWelcomeMarkup(), ParseMode: "HTML", DisableWebPagePreview: true})
		return err
	}

	return nil
}
