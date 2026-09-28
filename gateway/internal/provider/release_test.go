package provider

import (
	"context"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
	"weak"

	"kaiak/internal/config"
	"kaiak/internal/fakebackend"
)

// capturingTransport records the upstream requests it carries.
type capturingTransport struct {
	next http.RoundTripper
	mu   sync.Mutex
	reqs []*http.Request
}

func (c *capturingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.reqs = append(c.reqs, r)
	c.mu.Unlock()
	return c.next.RoundTrip(r)
}

// M2: once Send has returned (the first event is in), neither the client's body nor
// the edited copy sent upstream is reachable from the response, which may stream on
// for minutes: no retry can use them any more.
func TestSendKeepsNoRequestBodyOnceTheFirstEventIsIn(t *testing.T) {
	fb := fakebackend.New()
	defer fb.Close()
	pace := make(chan struct{})
	defer close(pace)
	fb.SetReply(fakebackend.Reply{Pace: pace})
	b := &config.Backend{ID: "local", Type: config.BackendOpenAICompatible, BaseURL: fb.URL() + "/v1",
		ConnectTimeout: time.Second, FirstEventTimeout: 10 * time.Second, StallTimeout: time.Hour}
	transport := &capturingTransport{next: newClient(time.Second).Transport}
	p := &openAIFormat{backend: b, client: &http.Client{Transport: transport}}

	// The client's body is reachable only through the provider once send returns.
	send := func() (Response, weak.Pointer[byte]) {
		body := []byte(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		resp, err := p.Send(context.Background(), &Request{Endpoint: ChatCompletions,
			Deployment: config.Deployment{Backend: b, Model: "m"}, Body: body, Stream: true,
			RequestID: "req-1", PublicModel: "m", Sent: func() {}})
		if err != nil {
			t.Fatal(err)
		}
		return resp, weak.Make(&body[0])
	}
	resp, clientBody := send()
	defer resp.Close()

	runtime.GC()
	if clientBody.Value() != nil {
		t.Error("the client's body is still reachable from the open response")
	}
	transport.mu.Lock()
	upstream := transport.reqs[0]
	transport.mu.Unlock()
	if upstream.GetBody != nil {
		if again, err := upstream.GetBody(); err == nil {
			again.Close()
			t.Error("the edited body can still be replayed from the upstream request")
		}
	}
	if r, ok := upstream.Body.(*upstreamBodyReader); !ok || r.held() {
		t.Errorf("the upstream request's body %T still holds the edited copy", upstream.Body)
	}
	runtime.KeepAlive(resp)
}
