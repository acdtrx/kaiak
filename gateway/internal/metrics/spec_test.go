package metrics

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"kaiak/internal/accounting"
	"kaiak/internal/config"
	"kaiak/internal/control"
	"kaiak/internal/routing"
	"kaiak/internal/telemetry/metric"
	"kaiak/internal/telemetry/otlplog"
)

// pendingFamilies are families of the spec's metric list not yet in their listed
// form, left out of the comparison on both sides: the metric exporter's own count,
// which comes with OTLP metric export.
var pendingFamilies = []string{
	"otel_sdk_exporter_metric_data_point_exported_total",
}

// A full scrape of every family the gateway registers, each driven until every
// attribute it can carry shows, holds exactly the families of the spec's metric list
// (docs/specs/GATEWAY.md, Observability → Metric list) under their Prometheus names,
// each of its kind and with exactly its listed attributes as labels.
func TestFullScrapeIsTheSpecsMetricList(t *testing.T) {
	spec := specMetricList(t)
	got := scrapedFamilies(t, fullScrape(t))
	for name, want := range spec {
		if slices.Contains(pendingFamilies, name) {
			continue
		}
		f, ok := got[name]
		if !ok {
			t.Errorf("%s is in the metric list but not in the scrape", name)
			continue
		}
		if f.kind != want.kind {
			t.Errorf("%s is a %s, want a %s", name, f.kind, want.kind)
		}
		if !slices.Equal(f.labels, want.labels) {
			t.Errorf("%s has labels %v, want %v", name, f.labels, want.labels)
		}
	}
	for name := range got {
		if _, ok := spec[name]; !ok && !slices.Contains(pendingFamilies, name) {
			t.Errorf("%s is in the scrape but not in the metric list", name)
		}
	}
}

// family is one metric family: its Prometheus type and its label names, sorted.
type family struct {
	kind   string
	labels []string
}

var labelChars = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// specMetricList reads the metric list's table: each row's Prometheus name, its
// Prometheus type (a counter, a histogram, else a gauge) and its attribute keys as
// label names.
func specMetricList(t *testing.T) map[string]family {
	t.Helper()
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "specs", "GATEWAY.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(doc)
	backticked := regexp.MustCompile("`([^`]+)`")
	names := func(cell string) []string {
		var out []string
		for _, m := range backticked.FindAllStringSubmatch(cell, -1) {
			out = append(out, m[1])
		}
		return out
	}
	_, usage, ok := strings.Cut(text, "The usage labels are ")
	usage, _, ok2 := strings.Cut(usage, "(below)")
	if !ok || !ok2 {
		t.Fatal("no usage label list in the spec")
	}
	usageLabels := names(usage)

	list := map[string]family{}
	inTable := false
	var previous []string
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "| OpenTelemetry name | Kind |") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 7 || strings.HasPrefix(strings.TrimSpace(cells[0]), "---") {
			continue
		}
		kind := "gauge"
		switch k := strings.TrimSpace(cells[1]); {
		case strings.HasPrefix(k, "counter"):
			kind = "counter"
		case strings.HasPrefix(k, "histogram"):
			kind = "histogram"
		}
		var attrs []string
		switch cell := strings.TrimSpace(cells[3]); {
		case cell == "as above":
			attrs = previous
		case strings.HasPrefix(cell, "usage labels"):
			attrs = append(slices.Clone(usageLabels), names(strings.TrimPrefix(cell, "usage labels"))...)
		default:
			attrs = names(cell)
		}
		previous = attrs
		// Attribute keys become label names by the Naming rule: a run of characters
		// outside [a-zA-Z0-9] is one _.
		var labels []string
		for _, a := range attrs {
			labels = append(labels, labelChars.ReplaceAllString(a, "_"))
		}
		slices.Sort(labels)
		prom := names(cells[4])
		if len(prom) != 1 {
			t.Fatalf("row %q: no Prometheus name", line)
		}
		list[prom[0]] = family{kind: kind, labels: labels}
	}
	if len(list) < 40 {
		t.Fatalf("read %d families from the metric list", len(list))
	}
	return list
}

// scrapedFamilies reads a scrape's families: their TYPE and the label names their
// samples carry (a histogram's le aside).
func scrapedFamilies(t *testing.T, scrape string) map[string]family {
	t.Helper()
	families := map[string]family{}
	label := regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="(?:[^"\\]|\\.)*"`)
	var current string
	for line := range strings.Lines(scrape) {
		line = strings.TrimSuffix(line, "\n")
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name, kind, _ := strings.Cut(rest, " ")
			current = name
			families[name] = family{kind: kind}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		f := families[current]
		_, labels, ok := strings.Cut(line, "{")
		if ok {
			labels, _, _ = strings.Cut(labels, "} ")
			for _, m := range label.FindAllStringSubmatch(labels, -1) {
				if m[1] != "le" && !slices.Contains(f.labels, m[1]) {
					f.labels = append(f.labels, m[1])
				}
			}
		}
		slices.Sort(f.labels)
		families[current] = f
	}
	return families
}

// fullScrape registers every family as main does — control-plane mode, OTLP log
// export on — and feeds each event that gives a family its every attribute.
func fullScrape(t *testing.T) string {
	t.Helper()
	capped := &config.Backend{ID: "capped", Type: config.BackendOpenAI, MaxInFlight: 2}
	model := &config.Model{Name: "chat", Deployments: []config.Deployment{{Backend: capped, Model: "chat-7b"}}}
	group := &config.Group{ID: "eval", PathIDs: []string{"research", "eval"}}
	snap := &config.Snapshot{KeyIDLabel: true, GroupLabel: true,
		Backends: map[string]*config.Backend{"capped": capped}, Models: map[string]*config.Model{"chat": model}}
	holder := &config.Holder{}
	reg := metric.NewRegistry()

	RegisterBuildInfo(reg, "1.2.3")
	RegisterLogExport(reg, func() otlplog.Counts {
		return otlplog.Counts{Handed: 3, Exported: 2, Failed: map[string]uint64{"503": 1}}
	})
	circuits := NewCircuits(reg)
	router := routing.New(routing.Options{Observer: circuits})
	ops := NewOps(reg, router, circuits, holder)
	usage := NewUsageMetrics(reg, holder)
	delivery := NewUsageDelivery(reg)
	RegisterControlState(reg, &fakeControlState{connected: true, last: time.Now(), totalsAt: time.Now()})

	holder.Swap(snap)
	router.Configure(snap)
	ops.ConfigLoaded(config.Load{Trigger: config.TriggerControl, Snapshot: snap, At: time.Now(), Document: true,
		Bytes: 1024, Duration: time.Millisecond})
	ops.ObserveLimitsSync(time.Millisecond)
	ops.ObserveRequest("POST", "/v1/chat/completions", 429, "rate_limit_exceeded", "chat", time.Second)
	ops.ObserveTimeToFirstToken("chat", "capped", time.Second)
	ops.ObserveOutputRate("chat", "capped", 40)
	ops.CountError(ErrorRateLimited)
	ops.CountRequestError(group, "k-eval", "chat", "rate_limit_exceeded")
	ops.ObserveQueueWait("chat", time.Second)
	ops.CountQueueRejection("chat", QueueFull)
	ops.CountRetry("chat", "capped", AttemptServerError)
	ops.ObserveUpstreamAttempt("capped", "chat-7b", AttemptSuccess, time.Second)
	ops.ObserveAttempts("chat", 1)
	ops.ConnectionRefused()
	usage.Record(accounting.UsageRecord{KeyID: "k-eval", Groups: group.PathIDs, Model: "chat", Operation: "chat",
		Deployment: accounting.Deployment{Backend: "capped", Model: "chat-7b"},
		Units:      accounting.Units{config.UnitTokensIn: 1}})
	usage.RecordClamped()
	delivery.UsageBatchSent(control.BatchAcked, time.Now())
	delivery.UsageQueueDepth(1, 1, 100)
	delivery.UsageRecordsDropped(control.DroppedInvalid, 1)
	return text(reg)
}
