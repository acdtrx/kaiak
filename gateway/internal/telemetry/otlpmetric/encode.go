package otlpmetric

import (
	"bytes"
	"encoding/json"
	"strconv"
	"time"

	"kaiak/internal/telemetry/metric"
	"kaiak/internal/telemetry/otlp"
)

// The OTLP/HTTP JSON encoding of ExportMetricsServiceRequest (opentelemetry-proto:
// collector/metrics/v1, metrics/v1; docs/specification.md, JSON Protobuf Encoding),
// on the messages every signal shares (otlp): lowerCamelCase keys, 64-bit integers
// as decimal strings, enums as integers. Only the fields the gateway writes are
// here. A request is assembled from separately encoded metrics and points, so that
// an export too large for one request can be cut between them.

type numberDataPoint struct {
	Attributes        []otlp.KeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string          `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string          `json:"timeUnixNano"`
	AsInt             *string         `json:"asInt,omitempty"`
	AsDouble          *otlp.Double    `json:"asDouble,omitempty"`
}

type histogramDataPoint struct {
	Attributes        []otlp.KeyValue `json:"attributes,omitempty"`
	StartTimeUnixNano string          `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string          `json:"timeUnixNano"`
	Count             string          `json:"count"`
	Sum               otlp.Double     `json:"sum"`
	BucketCounts      []string        `json:"bucketCounts"`
	ExplicitBounds    []otlp.Double   `json:"explicitBounds"`
}

// maxRequestSize bounds one request's body: the OpenTelemetry Collector's default
// message limit over gRPC, well under its HTTP receiver's default
// (docs/specs/GATEWAY.md, Observability → OTLP metric export: size).
const maxRequestSize = 4 << 20

// request is one body to post and the data points it carries.
type request struct {
	body   []byte
	points int
}

// encodeRequests encodes one collect's streams, taken at now, as
// ExportMetricsServiceRequests under res, each at most limit bytes: whole metrics
// where they fit, a metric too large for one request split by its points (each
// part repeating the metric's name, unit and description). A single point larger
// than limit goes alone, over the bound. No stream, no request.
func encodeRequests(res otlp.Resource, now time.Time, streams []stream, limit int) ([]request, error) {
	resource, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	scope, err := json.Marshal(otlp.InstrumentationScope{Name: otlp.ScopeName})
	if err != nil {
		return nil, err
	}
	open := append(append(append(append([]byte(`{"resourceMetrics":[{"resource":`), resource...),
		`,"scopeMetrics":[{"scope":`...), scope...), `,"metrics":[`...)
	const closing = `]}]}]}`
	envelope := len(open) + len(closing)

	var (
		requests []request
		metrics  bytes.Buffer
		points   int
	)
	flush := func() {
		if metrics.Len() == 0 {
			return
		}
		body := make([]byte, 0, envelope+metrics.Len())
		body = append(append(append(body, open...), metrics.Bytes()...), closing...)
		requests = append(requests, request{body: body, points: points})
		metrics.Reset()
		points = 0
	}
	add := func(m []byte, n int) {
		if metrics.Len() > 0 && envelope+metrics.Len()+1+len(m) > limit {
			flush()
		}
		if metrics.Len() > 0 {
			metrics.WriteByte(',')
		}
		metrics.Write(m)
		points += n
	}

	timeUnixNano := otlp.UnixNano(now)
	for _, s := range streams {
		head, tail, err := metricFrame(s)
		if err != nil {
			return nil, err
		}
		encoded := make([][]byte, len(s.points))
		size := len(head) + len(tail) + len(s.points) - 1
		for i, p := range s.points {
			if encoded[i], err = encodePoint(s, p, timeUnixNano); err != nil {
				return nil, err
			}
			size += len(encoded[i])
		}
		if envelope+size <= limit {
			add(frame(head, tail, encoded), len(encoded))
			continue
		}
		from := 0
		partSize := len(head) + len(tail)
		for i, e := range encoded {
			if i > from && envelope+partSize+1+len(e) > limit {
				add(frame(head, tail, encoded[from:i]), i-from)
				from, partSize = i, len(head)+len(tail)
			}
			if i > from {
				partSize++
			}
			partSize += len(e)
		}
		add(frame(head, tail, encoded[from:]), len(encoded)-from)
	}
	flush()
	return requests, nil
}

// frame is a metric with the given encoded points.
func frame(head, tail []byte, points [][]byte) []byte {
	var b bytes.Buffer
	b.Write(head)
	for i, p := range points {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(p)
	}
	b.Write(tail)
	return b.Bytes()
}

// metricFrame is a stream's Metric up to its data points and after them: its name,
// description and unit, then a sum (isMonotonic for a counter), gauge or histogram
// with the stream's aggregationTemporality.
func metricFrame(s stream) (head, tail []byte, err error) {
	f := s.family
	var b bytes.Buffer
	b.WriteString(`{"name":`)
	if err := writeString(&b, f.Name); err != nil {
		return nil, nil, err
	}
	if f.Description != "" {
		b.WriteString(`,"description":`)
		if err := writeString(&b, f.Description); err != nil {
			return nil, nil, err
		}
	}
	if f.Unit != "" {
		b.WriteString(`,"unit":`)
		if err := writeString(&b, f.Unit); err != nil {
			return nil, nil, err
		}
	}
	temporality := strconv.Itoa(int(s.temporality))
	switch f.Kind {
	case metric.KindCounter, metric.KindUpDownCounter:
		b.WriteString(`,"sum":{"dataPoints":[`)
		tail = []byte(`],"aggregationTemporality":` + temporality + `,"isMonotonic":` + strconv.FormatBool(f.Kind == metric.KindCounter) + `}}`)
	case metric.KindGauge:
		b.WriteString(`,"gauge":{"dataPoints":[`)
		tail = []byte(`]}}`)
	case metric.KindHistogram:
		b.WriteString(`,"histogram":{"dataPoints":[`)
		tail = []byte(`],"aggregationTemporality":` + temporality + `}}`)
	}
	return b.Bytes(), tail, nil
}

func writeString(b *bytes.Buffer, s string) error {
	q, err := json.Marshal(s)
	b.Write(q)
	return err
}

// encodePoint is one data point: its attributes (an empty value left out, as on
// /metrics; each as its key's type says), its start (sums and histograms) and the
// collect's time, its value as the family's Number says, or a histogram's count,
// sum, buckets and bounds.
func encodePoint(s stream, p metric.Point, timeUnixNano string) ([]byte, error) {
	f := s.family
	var attrs []otlp.KeyValue
	for i, v := range p.Attributes {
		if v == "" {
			continue
		}
		key := f.Attributes[i]
		value := otlp.StringValue(v)
		if f.AttributeTypes[key] == metric.IntAttribute {
			// The registry took only canonical integers for this key.
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, err
			}
			value = otlp.IntValue(n)
		}
		attrs = append(attrs, otlp.KeyValue{Key: key, Value: value})
	}
	start := ""
	if s.temporality != temporalityNone && !p.StartTime.IsZero() {
		start = otlp.UnixNano(p.StartTime)
	}
	if f.Kind == metric.KindHistogram {
		h := histogramDataPoint{Attributes: attrs, StartTimeUnixNano: start, TimeUnixNano: timeUnixNano,
			Count:          strconv.FormatUint(p.Histogram.Count, 10),
			Sum:            otlp.Double(p.Histogram.Sum),
			BucketCounts:   make([]string, len(p.Histogram.BucketCounts)),
			ExplicitBounds: make([]otlp.Double, len(f.Buckets)),
		}
		for i, n := range p.Histogram.BucketCounts {
			h.BucketCounts[i] = strconv.FormatUint(n, 10)
		}
		for i, b := range f.Buckets {
			h.ExplicitBounds[i] = otlp.Double(b)
		}
		return json.Marshal(h)
	}
	n := numberDataPoint{Attributes: attrs, StartTimeUnixNano: start, TimeUnixNano: timeUnixNano}
	if f.Number == metric.Int {
		v := strconv.FormatInt(p.Int, 10)
		n.AsInt = &v
	} else {
		v := otlp.Double(p.Double)
		n.AsDouble = &v
	}
	return json.Marshal(n)
}
