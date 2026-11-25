package dispatch

import (
	"context"
	"time"

	"github.com/trick77/ship/kit/logging"

	"github.com/trick77/trakk/backend/internal/alert"
)

// helloQuiet is how long after a channel took a message a start sends it no test: a restart loop, or an update right
// after an alert, tells the recipients nothing they do not know. A failure never quiets the next start's test: that
// start is the one checking the fix.
const helloQuiet = 10 * time.Minute

// hello sends every channel a test message at the start and records its outcome as any delivery's (the footer shows
// it): a channel that cannot deliver is known now, not at the first alert. Every channel whatever its least
// severity, as -notify-test; outside the hourly cap, or a cap of one would hold back the hour's first alert. A send
// the shutdown cut is no attempt, and no failure stops the engine.
func (e *Engine) hello(ctx context.Context, now time.Time) {
	if !e.o.Hello || !e.sending {
		return
	}
	known, err := e.o.Store.AlertChannels(ctx)
	if err != nil {
		e.log.Warn("Start's test message not sent: the channels' state not read", logging.Err(err))
		return
	}
	for _, ch := range e.o.Config.Channels {
		sender := e.o.Senders[ch.Name]
		if sender == nil {
			continue // New said so
		}
		if k, ok := known[ch.Name]; ok && k.LastOK != nil {
			if since := now.Sub(time.UnixMilli(*k.LastOK)); since >= 0 && since < helloQuiet {
				e.log.Info("Start's test message skipped: the channel took a message minutes ago", "channel", ch)
				continue
			}
		}
		title, body := testMessage(ch.Name, "sent at every start")
		if e.o.PublicFQDN != "" {
			body = "Trakk on " + e.o.PublicFQDN + " started. " + body
		}
		failure := Failure(sender.Deliver(ctx, title, body, alert.Low))
		if ctx.Err() != nil {
			return
		}
		if err := e.o.Store.ChannelDelivered(ctx, ch.Name, now, failure); err != nil {
			e.log.Warn("Start's test message: outcome not recorded", "channel", ch, logging.Err(err))
		}
		if failure != "" {
			e.log.Warn("Start's test message not delivered", "channel", ch, "error", failure)
			continue
		}
		e.log.Info("Start's test message sent", "channel", ch)
	}
}

// testMessage is the test message to a channel; how says what sent it.
func testMessage(channel, how string) (title, body string) {
	return "[low] Trakk test message", "A test message from Trakk, " + how + ": the " + channel + " channel works."
}
