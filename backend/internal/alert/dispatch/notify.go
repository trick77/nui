package dispatch

import (
	"context"
	"fmt"
	"strings"

	"github.com/trick77/toolkit/notify"

	"github.com/trick77/trakk/backend/internal/alert"
)

// severities map an alert's severity onto notify's: the priority ntfy, Pushover and Gotify send at.
var severities = map[alert.Severity]notify.Severity{alert.Low: notify.Low, alert.Normal: notify.Normal,
	alert.High: notify.High}

// channelSender sends a named channel's messages through toolkit/notify: one URL, its shoutrrr service.
type channelSender struct {
	ch      *notify.Channels
	service string
}

// Deliver sends and returns when the channel answered, timed out (notify.SendTimeout) or ctx ended: the shutdown
// cancels the send rather than waits for it.
func (s channelSender) Deliver(ctx context.Context, title, body string, sev alert.Severity) []Delivery {
	ds, _ := s.ch.DeliverMessage(ctx, notify.Message{Title: title, Body: body, Severity: severities[sev]})
	out := make([]Delivery, 0, len(ds))
	for _, d := range ds {
		out = append(out, Delivery{Service: s.service, Err: d.Err})
	}
	return out
}

// Wire builds a sender per channel of c and names each channel's service as notify reads it. A channel notify
// refuses (an invalid URL, a service outside notify.Schemes) is a fault of the file like any other: c comes back
// with it, sending nothing (alert.Config.WithFaults). The faults name the channel and the supported services, never
// the URL.
func Wire(c alert.Config) (alert.Config, map[string]Sender) {
	senders := map[string]Sender{}
	var faults []string
	for i := range c.Channels {
		ch := &c.Channels[i]
		ch.Service = notify.Scheme(ch.URL())
		chs, err := notify.NewChannels([]string{ch.URL()})
		if err != nil {
			// "channel 1 (smtp): invalid URL": the reason, without notify's own label.
			reason := err.Error()
			if _, r, ok := strings.Cut(reason, "): "); ok {
				reason = r
			}
			faults = append(faults, fmt.Sprintf("channel %q (%s): %s; supported: %s", ch.Name, ch.Service, reason,
				strings.Join(notify.Schemes(), ", ")))
			continue
		}
		senders[ch.Name] = channelSender{ch: chs, service: ch.Service}
	}
	return c.WithFaults(faults...), senders
}
