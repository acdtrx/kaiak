package limits

import (
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
)

// The media limits (docs/specs/GATEWAY.md, Limits → Check and reserve): images reserve
// the request's count and settle to the answer's, audio generated and audio processed
// reserve nothing and settle after. Each applies only to a request that would add to
// it.

// mediaRecord is a record carrying media units, with tokens beside them.
func mediaRecord(images, audioOut, audioIn int64) accounting.UsageRecord {
	rec := record(10, 0, 0, 5, 0, 0)
	rec.Units[config.UnitImages] = images
	rec.Units[config.UnitAudioSecondsOut] = audioOut
	rec.Units[config.UnitAudioSecondsIn] = audioIn
	return rec
}

// The needs of an image request asking for n images, a speech request and a
// transcription, each with a token estimate.
func imageNeed(n int64) Need {
	return Need{Tokens: 10, Media: map[config.Measure]int64{config.MeasureImages: n}}
}

var speechNeed = Need{Tokens: 10, Media: map[config.Measure]int64{config.MeasureAudioSecondsOut: 0}}

var transcriptionNeed = Need{Tokens: 10, Media: map[config.Measure]int64{config.MeasureAudioSecondsIn: 0}}

// reserve reserves need for s and fails if it is refused.
func reserve(t *testing.T, l *Limiter, s Subject, need Need) *Reservation {
	t.Helper()
	res, rej := l.Reserve(s, need)
	if rej != nil {
		t.Fatalf("refused: %+v", rej)
	}
	return res
}

// refusedNeed reserves need for s and fails if it is admitted.
func refusedNeed(t *testing.T, l *Limiter, s Subject, need Need) *Rejection {
	t.Helper()
	res, rej := l.Reserve(s, need)
	if rej == nil {
		t.Fatalf("admitted, want refused (headers %+v)", res.Headers())
	}
	return rej
}

func limitJSON(typ config.LimitType, value string) string {
	return `[{ "type": "` + string(typ) + `", "value": ` + value + ` }]`
}

// An image request reserves the images it asks for, admitted while they fit, as
// tokens are; it settles to the images its answer carried — a layered model's answer
// can take the window past its value. Other requests meet no image limit.
func TestImageLimitsReserveTheRequestsImagesAndSettleToTheAnswers(t *testing.T) {
	for _, typ := range []config.LimitType{config.LimitImagesPerMinute, config.LimitImagesPerHour} {
		t.Run(string(typ), func(t *testing.T) {
			l := newClock("2026-09-24T10:30:00Z").limiter(holderOf(snapshot(t, limitsDoc{workload: limitJSON(typ, "10")})))
			first := reserve(t, l, workload, imageNeed(4))
			reserve(t, l, workload, imageNeed(4))
			if got := used(t, l, "w", typ); got != 8 {
				t.Fatalf("used %d after two reservations of 4, want 8", got)
			}
			rej := refusedNeed(t, l, workload, imageNeed(4))
			if rej.Type != typ || rej.Measure != config.MeasureImages || rej.Group != "w" ||
				rej.Limit != 10 || rej.Used != 8 || rej.Requested != 4 {
				t.Errorf("rejection %+v, want the image limit: 4 requested, 8 of 10 used", rej)
			}
			if rej.Headers.Requests != nil || rej.Headers.Tokens != nil {
				t.Errorf("an image limit sends no rate-limit headers: %+v", rej.Headers)
			}
			// The refused request reserved nothing; 2 more fit.
			last := reserve(t, l, workload, imageNeed(2))

			// The first answer carried 6 images (one per layer): the window holds 12,
			// past its value of 10.
			l.Settle(first, mediaRecord(6, 0, 0))
			if got := used(t, l, "w", typ); got != 12 {
				t.Errorf("used %d after settling 4 reserved to 6 made, want 12", got)
			}
			refusedNeed(t, l, workload, imageNeed(1))
			// A request that makes no images is not refused by a full image window.
			admitN(t, l, workload, 1, 10)
			reserve(t, l, workload, speechNeed)

			// A request with no record (refused downstream) releases its images.
			l.Settle(last)
			if got := used(t, l, "w", typ); got != 10 {
				t.Errorf("used %d after releasing 2, want 10", got)
			}
		})
	}
}

// An image request asking for more images than the limit's whole value is refused as
// too large, never admitted however empty the window.
func TestImageLimitRefusesARequestLargerThanTheLimit(t *testing.T) {
	l := newClock("2026-09-24T10:30:00Z").limiter(holderOf(snapshot(t, limitsDoc{team: limitJSON(config.LimitImagesPerHour, "3")})))
	rej := refusedNeed(t, l, workload, imageNeed(4))
	if rej.Measure != config.MeasureImages || rej.Requested != 4 || rej.Max != 3 || rej.Used != 0 {
		t.Errorf("rejection %+v, want 4 images refused by a limit of 3", rej)
	}
	reserve(t, l, workload, imageNeed(3))
}

// An image limit blocked only by a request still running answers the short retry, as
// a token limit does: the running request's images are counted again when it
// settles, in the window it ends in, so its reservation's expiry is no promise — a
// client told to come back then is refused again. Once settled images block, the wait
// is theirs, and it holds (docs/specs/GATEWAY.md, Limits → Refusal).
func TestImageLimitBlockedOnlyByARunningRequestAnswersAShortRetry(t *testing.T) {
	const shortRetry = 2 * time.Second // the spec's fixed value
	for _, c := range []struct {
		typ config.LimitType
		// settledWait is the wait once the running request settled at 10:30:30 and
		// a request is refused at 10:30:40: its bucket leaves the minute at 10:31:30,
		// the hour ends at 11:00.
		settledWait time.Duration
	}{
		{config.LimitImagesPerMinute, 50 * time.Second},
		{config.LimitImagesPerHour, 29*time.Minute + 20*time.Second},
	} {
		t.Run(string(c.typ), func(t *testing.T) {
			clk := newClock("2026-09-24T10:30:00Z")
			l := clk.limiter(holderOf(snapshot(t, limitsDoc{workload: limitJSON(c.typ, "4")})))
			running := reserve(t, l, workload, imageNeed(4))
			clk.advance(10 * time.Second)
			if rej := refusedNeed(t, l, workload, imageNeed(1)); rej.RetryAfter != shortRetry {
				t.Errorf("retry after %v, want %v: only a running request blocks", rej.RetryAfter, shortRetry)
			}
			clk.advance(20 * time.Second)
			l.Settle(running, mediaRecord(4, 0, 0))
			clk.advance(10 * time.Second)
			rej := refusedNeed(t, l, workload, imageNeed(1))
			if rej.RetryAfter != c.settledWait {
				t.Errorf("retry after %v once settled, want %v", rej.RetryAfter, c.settledWait)
			}
			clk.advance(rej.RetryAfter)
			reserve(t, l, workload, imageNeed(1))
		})
	}
}

// Audio generated reserves nothing — its length is known once the audio is relayed —
// so, as USD, a limit refuses once its window has reached its value, and requests in
// flight then can pass it. Only requests generating audio meet it.
func TestAudioGeneratedLimitsSettleAfterTheRequest(t *testing.T) {
	for _, typ := range []config.LimitType{config.LimitAudioSecondsOutPerMinute, config.LimitAudioSecondsOutPerHour} {
		t.Run(string(typ), func(t *testing.T) {
			l := newClock("2026-09-24T10:30:00Z").limiter(holderOf(snapshot(t, limitsDoc{team: limitJSON(typ, "30")})))
			first := reserve(t, l, workload, speechNeed)
			if got := used(t, l, "t", typ); got != 0 {
				t.Fatalf("used %d after a speech reservation, want 0: nothing is reserved", got)
			}
			l.Settle(first, mediaRecord(0, 20, 0))
			if got := used(t, l, "t", typ); got != 20 {
				t.Fatalf("used %d after settling 20 s, want 20", got)
			}
			// 20 of 30: two more are admitted, and both run past the value.
			second, third := reserve(t, l, workload, speechNeed), reserve(t, l, workload, speechNeed)
			l.Settle(second, mediaRecord(0, 20, 0))
			l.Settle(third, mediaRecord(0, 15, 0))
			if got := used(t, l, "t", typ); got != 55 {
				t.Fatalf("used %d, want 55: the requests in flight passed the value", got)
			}
			rej := refusedNeed(t, l, workload, speechNeed)
			if rej.Type != typ || rej.Measure != config.MeasureAudioSecondsOut || rej.Group != "t" ||
				rej.Limit != 30 || rej.Used != 55 || rej.Requested != 0 {
				t.Errorf("rejection %+v, want the team's audio limit: 55 of 30 used, nothing requested", rej)
			}
			if rej.Headers.Requests != nil || rej.Headers.Tokens != nil {
				t.Errorf("an audio limit sends no rate-limit headers: %+v", rej.Headers)
			}
			// Chat and image requests meet no audio limit.
			admitN(t, l, workload, 1, 10)
			reserve(t, l, workload, imageNeed(1))
		})
	}
}

// Audio processed reserves nothing — a transcription's seconds are known only from its
// answer — so, as audio generated, a limit refuses once its window has reached its
// value, and requests in flight then can pass it. Only transcriptions meet it.
func TestAudioProcessedLimitsSettleAfterTheRequest(t *testing.T) {
	for _, typ := range []config.LimitType{config.LimitAudioSecondsInPerMinute, config.LimitAudioSecondsInPerHour} {
		t.Run(string(typ), func(t *testing.T) {
			l := newClock("2026-09-24T10:30:00Z").limiter(holderOf(snapshot(t, limitsDoc{global: limitJSON(typ, "100")})))
			first := reserve(t, l, workload, transcriptionNeed)
			if got := used(t, l, "", typ); got != 0 {
				t.Fatalf("used %d after a transcription's reservation, want 0: nothing is reserved", got)
			}
			l.Settle(first, mediaRecord(0, 0, 58))
			if got := used(t, l, "", typ); got != 58 {
				t.Fatalf("used %d after settling, want the record's 58", got)
			}
			// 58 of 100: two more are admitted, and both run past the value.
			second, third := reserve(t, l, workload, transcriptionNeed), reserve(t, l, workload, transcriptionNeed)
			l.Settle(second, mediaRecord(0, 0, 30))
			l.Settle(third, mediaRecord(0, 0, 30))
			if got := used(t, l, "", typ); got != 118 {
				t.Fatalf("used %d, want 118: the requests in flight passed the value", got)
			}
			rej := refusedNeed(t, l, workload, transcriptionNeed)
			if rej.Type != typ || rej.Measure != config.MeasureAudioSecondsIn || rej.Scope() != ScopeGlobal ||
				rej.Limit != 100 || rej.Used != 118 || rej.Requested != 0 {
				t.Errorf("rejection %+v, want the global audio-processed limit: 118 of 100 used, nothing requested", rej)
			}
			if rej.Headers.Requests != nil || rej.Headers.Tokens != nil {
				t.Errorf("an audio limit sends no rate-limit headers: %+v", rej.Headers)
			}
			// Chat, speech and image requests meet no audio-processed limit.
			admitN(t, l, workload, 1, 10)
			reserve(t, l, workload, speechNeed)
			reserve(t, l, workload, imageNeed(1))
		})
	}
}

// Every scope keeps its media hour counts, limited or not, in file mode as the token
// hour is kept; per-minute media windows exist only for the limits that have them. A
// settled record counts each media unit toward its own window only.
func TestEveryScopeKeepsItsMediaHourCounts(t *testing.T) {
	l := newClock("2026-09-24T10:30:00Z").limiter(holderOf(snapshot(t, limitsDoc{})))
	l.Settle(reserve(t, l, workload, imageNeed(2)), mediaRecord(3, 0, 0))
	l.Settle(reserve(t, l, workload, speechNeed), mediaRecord(0, 7, 0))
	l.Settle(reserve(t, l, workload, transcriptionNeed), mediaRecord(0, 0, 5))
	for _, group := range []string{"", "t", "w"} {
		for typ, want := range map[config.LimitType]int64{
			config.LimitImagesPerHour: 3, config.LimitAudioSecondsOutPerHour: 7, config.LimitAudioSecondsInPerHour: 5,
			config.LimitTokensPerHour: 45,
		} {
			if got := used(t, l, group, typ); got != want {
				t.Errorf("%q %s used %d, want %d", group, typ, got, want)
			}
		}
		for _, typ := range []config.LimitType{config.LimitImagesPerMinute, config.LimitAudioSecondsOutPerMinute,
			config.LimitAudioSecondsInPerMinute} {
			if _, ok := l.Used(group, typ); ok {
				t.Errorf("%q has a %s counter without a limit", group, typ)
			}
		}
	}
	// ann is not on the requests' path.
	if got := used(t, l, "ann", config.LimitImagesPerHour); got != 0 {
		t.Errorf("ann images %d, want 0", got)
	}
}

// In control-plane mode the media hour windows count the control plane's totals plus
// this gateway's own usage not yet counted there; the per-minute ones are local
// shares of their limit.
func TestMediaWindowsInControlPlaneMode(t *testing.T) {
	c := newClock("2026-09-24T10:30:00Z")
	l, _ := c.shared(holderOf(snapshot(t, limitsDoc{
		workload: limitJSON(config.LimitImagesPerHour, "100"),
		team: `[{ "type": "audio_seconds_out_per_hour", "value": 600 },
      { "type": "audio_seconds_in_per_hour", "value": 900 },
      { "type": "images_per_minute", "value": 10 }]`,
	})))
	l.TakeTotals(Totals{LiveGateways: 2, Complete: true, Windows: []PushedWindow{
		pushedWindow("w", config.LimitImagesPerHour, "2026-09-24T10:00:00Z", 97),
		pushedWindow("t", config.LimitAudioSecondsOutPerHour, "2026-09-24T10:00:00Z", 600),
		pushedWindow("t", config.LimitAudioSecondsInPerHour, "2026-09-24T10:00:00Z", 850),
		pushedWindow("", config.LimitImagesPerHour, "2026-09-24T10:00:00Z", 5000),
	}}, 0)

	// 97 pushed: 4 images do not fit 100, 3 do.
	if rej := refusedNeed(t, l, workload, imageNeed(4)); rej.Type != config.LimitImagesPerHour || rej.Used != 97 {
		t.Errorf("rejection %+v, want the workload's image hour at 97 of 100", rej)
	}
	res := reserve(t, l, workload, imageNeed(3))
	l.Settle(res, inGeneration(1, mediaRecord(3, 0, 0)))
	if got := used(t, l, "w", config.LimitImagesPerHour); got != 100 {
		t.Errorf("image hour %d, want 97 pushed + 3 own", got)
	}
	if got := used(t, l, "", config.LimitImagesPerHour); got != 5003 {
		t.Errorf("global image hour %d, want 5000 pushed + 3 own: every scope is counted", got)
	}
	// The team's audio generated is spent by the other gateways.
	if rej := refusedNeed(t, l, workload, speechNeed); rej.Type != config.LimitAudioSecondsOutPerHour || rej.Used != 600 {
		t.Errorf("rejection %+v, want the team's audio hour at 600 of 600", rej)
	}
	// 850 pushed: a transcription is admitted, and its 60 s settled take the hour to
	// 910 of 900, the next refused.
	l.Settle(reserve(t, l, workload, transcriptionNeed), inGeneration(1, mediaRecord(0, 0, 60)))
	if rej := refusedNeed(t, l, workload, transcriptionNeed); rej.Type != config.LimitAudioSecondsInPerHour || rej.Used != 910 {
		t.Errorf("rejection %+v, want the team's audio-processed hour at 910 of 900", rej)
	}

	// The push counting batch 1 takes the 3 images into the base.
	l.TakeTotals(Totals{LiveGateways: 2, Windows: []PushedWindow{
		pushedWindow("w", config.LimitImagesPerHour, "2026-09-24T10:00:00Z", 100)}}, 1)
	if got := used(t, l, "w", config.LimitImagesPerHour); got != 100 {
		t.Errorf("image hour %d after the push, want 100 pushed and nothing own", got)
	}

	// The team's images per minute: a share of 5 among 2 gateways, its own count.
	l2, _ := c.shared(holderOf(snapshot(t, limitsDoc{team: limitJSON(config.LimitImagesPerMinute, "10")})))
	l2.TakeTotals(Totals{LiveGateways: 2, Complete: true}, 0)
	reserve(t, l2, workload, imageNeed(5))
	if rej := refusedNeed(t, l2, workload, imageNeed(1)); rej.Limit != 5 || rej.Max != 10 || rej.Used != 5 {
		t.Errorf("rejection %+v, want the share 5 of 10, all used", rej)
	}
	c.advance(time.Minute)
	reserve(t, l2, workload, imageNeed(5))
}
