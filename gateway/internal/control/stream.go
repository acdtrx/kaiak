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
	eventResync = "resync"
)

// streamResult is how one config stream ended.
type streamResult struct {
	// resync: the control plane cannot resume from the version sent; the snapshot
	// must be fetched again.
	resync bool
	// lasted is how long the stream was open; 0 when it never opened.
	lasted time.Duration
	// err is why the stream failed; nil when the control plane ended it.
	err error
}

// followStream opens the config stream after the latest version taken and handles
// its events until it ends: config events go through the apply path, totals to
// the totals consumer, heartbeats only prove the connection alive. Opening — the
// connection and the answer's headers — must complete within IdleTimeout, and a
// stream silent for IdleTimeout once open is closed: a connection that died without
// a close, or an endpoint that never answers, never ends on its own.
func (c *Client) followStream(ctx context.Context) streamResult {
	since := c.currentPosition()
	streamCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	errOpen := fmt.Errorf("config stream not open within %s", c.opts.IdleTimeout)
	opening := time.AfterFunc(c.opts.IdleTimeout, func() { cancel(errOpen) })
	resp, err := c.openStream(streamCtx, since)
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
	c.logger.Info("config stream connected", "since", since.version, "config_epoch", since.epoch)
	c.status.requestReport(statusTriggerConnect)

	errIdle := fmt.Errorf("config stream silent for %s", c.opts.IdleTimeout)
	idle := time.AfterFunc(c.opts.IdleTimeout, func() { cancel(errIdle) })
	defer idle.Stop()

	events := sse.NewReader(resp.Body, maxMessageBytes)
	for {
		block, err := events.Next()
		if err != nil {
			result := streamResult{lasted: time.Since(start)}
			switch cause := context.Cause(streamCtx); {
			case errors.Is(err, io.EOF):
			case errors.Is(cause, errIdle):
				result.err = errIdle
			default:
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
			snapshot, err := DecodeConfigSnapshot(block.Data)
			if err != nil {
				c.logger.Error("config event ignored: malformed", "error", err)
				continue
			}
			c.takeStreamConfig(snapshot)
		case eventTotals:
			totals, err := DecodeTotals(block.Data)
			if err != nil {
				c.logger.Error("totals event ignored: malformed", "error", err)
				continue
			}
			c.takeTotals(totals, 0)
		case eventResync:
			// The event name is the whole instruction; its data is {}.
			return streamResult{resync: true, lasted: time.Since(start)}
		default:
			c.logger.Debug("stream event ignored: unknown event", "event", block.Event)
		}
	}
}
