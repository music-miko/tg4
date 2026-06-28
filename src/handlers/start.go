/*
 * ArcMusicBot - Telegram Music Bot
 *  Copyright (c) 2025-2026 Team Arc
 *
 *  Licensed under GNU GPL v3
 */

package handlers

import (
	"ashokshau/tgmusic/config"
	"fmt"
	"runtime"
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
			"<b>Go Routines:</b> <code>%d</code>\n",
		latency, uptime, runtime.NumGoroutine(),
	)

	_, err = msg.EditText(c, response, &td.EditTextMessageOpts{ParseMode: "HTML"})
	return err
}

// startHandler handles the /start command.
// Private chat: shows a welcome photo with add-to-group + support buttons (tosu4 style).
// Group chat: shows bot-ready message with support button.
func startHandler(c *td.Client, m *td.Message) error {
	chatID := m.ChatId
	botName := c.Me.FirstName
	username := c.Me.Usernames.EditableUsername

	if m.IsPrivate() {
		go func(chatID int64) {
			_ = db.Instance.AddUser(chatID)
		}(chatID)

		// tosu4-style greeting: user mention + bot name + supported platforms + setup guide hint
		caption := fmt.Sprintf(
			"Hey <b>%s</b>!\n\n"+
				"I'm <b>%s</b>, your ultimate music companion for Telegram. 🎵\n\n"+
				"<b>Supported Platforms:</b>\n"+
				"• YouTube  • Spotify  • Apple Music\n"+
				"• SoundCloud  • JioSaavn  • Deezer\n"+
				"• MXPlayer  • Twitch  • Kick  • Tidal\n\n"+
				"<b>Just add me to your group and use</b> <code>/play [song]</code> <b>to start!</b>\n\n"+
				"<i>Tap <b>Setup Guide</b> to learn how to configure me in your group.</i>",
			firstName(c, m),
			botName,
		)

		_, err := m.ReplyPhoto(c, td.InputFileRemote{Id: config.StartImg}, &td.SendPhotoOpts{
			ParseMode:   "HTML",
			Caption:     caption,
			ReplyMarkup: core.StartPrivateMarkup(username),
		})
		return err
	}

	// Group
	go func(chatID int64) {
		_ = db.Instance.AddChat(chatID)
	}(chatID)

	uptime := getFormattedDuration(time.Since(startTime))
	response := fmt.Sprintf(
		"🎵 <b>%s</b> is online and ready!\n"+
			"<b>Uptime:</b> <code>%s</code>\n\n"+
			"Use <code>/play [song]</code> to start streaming music.",
		botName,
		uptime,
	)

	_, err := m.ReplyText(c, response, &td.SendTextMessageOpts{
		ParseMode:             "HTML",
		DisableWebPagePreview: true,
		ReplyMarkup:           core.SupportBtn(),
	})

	return err
}
