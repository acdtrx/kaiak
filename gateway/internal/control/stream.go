package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"kaiak/internal/sse"
)

// Stream event names (CONTROL-PROTOCOL.md, Messages: the event name picks the data's
// schema).
const (
	eventConfig = "config"
	eventTotals = "totals"
)

// errMalformedTotals ends a stream whose totals event could not be decoded: each later
// totals event lists only what changed since the one before, so the stream cannot go on
// without it — the reconnect brings complete totals (docs/specs/GATEWAY.md,
// Control-plane mode → Stream).
var errMalformedTotals = errors.New("totals event malformed: the stream is ended, the reconnect brings complete totals")

// streamResult is how one config stream ended.
type streamResult struct {
	// first is the stream's first config event, when the stream was opened to boot
	// from it (followStream's firstOnly) and one arrived; the stream ended there.
	first *ConfigEvent
	// lasted is how long the stream was open; 0 when it never opened.
	lasted time.Duration
	// err is why the stream failed; nil when the control plane ended it.
	err error
}

// followStream opens the config stream and handles its events until it ends: config
// events go through the apply path, totals to the totals consumer — a totals event
// that cannot be decoded ends the stream —, heartbeats only prove the connection
// alive. With firstOnly — the boot — it ends at the first config
// event and returns it unapplied, for the boot to apply. Opening — the connection and
// the answer's headers — must complete within IdleTimeout, and a stream silent for
// IdleTimeout once open is closed: a connection that died without a close, or an
// endpoint that never answers, never ends on its own.
func (c *Client) followStream(ctx context.Context, firstOnly bool) streamResult {
	streamCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	errOpen := fmt.Errorf("config stream not open within %s", c.opts.IdleTimeout)
	opening := time.AfterFunc(c.opts.IdleTimeout, func() { cancel(errOpen) })
	resp, err := c.openStream(streamCtx)
	if !opening.Stop() && err == nil {
		// The bound ran out as the answer arrived: the request is cancelled.
		resp.Body.Close()
		err = errOpen
	}
	if err != nil {
		if errors.Is(context.Cause(streamCtx), errOpen) {
			err = errOpen
		}
		return streamResult{err: err}
	}
	defer resp.Body.Close()
	start := time.Now()
	c.touch()
	c.streamOpen.Store(true)
	defer c.streamOpen.Store(false)
	c.logger.Info("config stream connected")
	c.status.requestReport(statusTriggerConnect)

	errIdle := fmt.Errorf("config stream silent for %s", c.opts.IdleTimeout)
	idle := time.AfterFunc(c.opts.IdleTimeout, func() { cancel(errIdle) })
	defer idle.Stop()

	events := sse.NewReader(resp.Body, maxMessageBytes)
	// The first totals of each connection are complete; later ones list the windows
	// that changed (CONTROL-PROTOCOL.md, Config stream → Totals).
	firstTotals := true
	for {
		block, err := events.Next()
		if err != nil {
			result := streamResult{lasted: time.Since(start)}
			switch cause := context.Cause(streamCtx); {
			case errors.Is(err, io.EOF):
			case errors.Is(cause, errIdle):
				result.err = errIdle
			case ctx.Err() != nil:
				result.err = ctx.Err()
			default:
				// The stream's format broken, or the connection's failure by its
				// class (sse.Reader.Next).
				result.err = fmt.Errorf("read config stream: %w", err)
			}
			return result
		}
		idle.Reset(c.opts.IdleTimeout)
		c.touch()
		if !block.HasData {
			continue // a heartbeat or another comment
		}
		switch block.Event {
		case eventConfig:
			event, err := DecodeConfigEvent(block.Data)
			if firstOnly {
				if err != nil {
					// At boot a config event the message rules refuse ends the boot:
					// waiting cannot fix it.
					return streamResult{lasted: time.Since(start), err: fmt.Errorf("config event refused: %w", err)}
				}
				return streamResult{first: &event, lasted: time.Since(start)}
			}
			if err != nil {
				c.logger.Error("config event ignored: malformed", "exception.message", err)
				continue
			}
			c.takeConfig(event)
		case eventTotals:
			if firstOnly {
				continue // the totals follow the config; the stream Run opens takes them
			}
			totals, err := DecodeTotals(block.Data)
			if err != nil {
				return streamResult{lasted: time.Since(start), err: fmt.Errorf("%w: %w", errMalformedTotals, err)}
			}
			c.takeTotals(totals, firstTotals)
			firstTotals = false
		default:
			c.logger.Debug("stream event ignored: unknown event", "kaiak.control.event", block.Event)
		}
	}
}
