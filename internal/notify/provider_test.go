package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func testHTTPClient() *http.Client { return &http.Client{Timeout: 2 * time.Second} }

func TestWebhookProviderSendsJSONAndBearerToken(t *testing.T) {
	var gotAuth string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &webhookProvider{client: testHTTPClient()}
	err := p.Send(context.Background(), Destination{URL: srv.URL, Token: "tk_abc"},
		Notification{Event: EventExhausted, Title: "spillway: all accounts exhausted", Body: "wait"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotAuth != "Bearer tk_abc" {
		t.Errorf("Authorization = %q, want Bearer tk_abc", gotAuth)
	}
	if gotBody["text"] == "" || gotBody["event"] != EventExhausted {
		t.Errorf("body = %+v", gotBody)
	}
}

// A network-level failure (DNS, TLS, connection refused) returns a
// *url.Error from client.Do whose own Error() string embeds the full
// request URL — dest material, exactly what must never be logged. This
// pins doAndCheck's defence: the returned error must never mention the
// destination even when the underlying transport error does.
func TestWebhookProviderNetworkErrorDoesNotLeakDestination(t *testing.T) {
	const secretURL = "https://this-host-does-not-resolve.invalid.example/secret-path-xyz"
	p := &webhookProvider{client: &http.Client{Timeout: 2 * time.Second}}
	err := p.Send(context.Background(), Destination{URL: secretURL}, Notification{Title: "t", Body: "b"})
	if err == nil {
		t.Fatal("expected a network error resolving an .invalid host")
	}
	if strings.Contains(err.Error(), "this-host-does-not-resolve") || strings.Contains(err.Error(), "secret-path-xyz") {
		t.Errorf("returned error leaks the destination: %v", err)
	}
}

func TestWebhookProviderRequiresURL(t *testing.T) {
	p := &webhookProvider{client: testHTTPClient()}
	if err := p.Send(context.Background(), Destination{}, Notification{}); err == nil {
		t.Error("expected an error with no URL configured")
	}
}

func TestNtfyProviderPostsToTopicWithTitleAndBearer(t *testing.T) {
	var gotPath, gotTitle, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTitle = r.Header.Get("Title")
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &ntfyProvider{client: testHTTPClient()}
	err := p.Send(context.Background(), Destination{URL: srv.URL, Topic: "my-topic", Token: "tk_reserved"},
		Notification{Title: "spillway: pool exhausted", Body: "holding until 07:00"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotPath != "/my-topic" {
		t.Errorf("path = %q, want /my-topic", gotPath)
	}
	if gotTitle != "spillway: pool exhausted" {
		t.Errorf("Title header = %q", gotTitle)
	}
	if gotAuth != "Bearer tk_reserved" {
		t.Errorf("Authorization = %q, want Bearer tk_reserved (issue #101: reserved/self-hosted topic auth)", gotAuth)
	}
	if gotBody != "holding until 07:00" {
		t.Errorf("body = %q", gotBody)
	}
}

func TestNtfyTargetURLDefaultsToNtfySh(t *testing.T) {
	got := ntfyTargetURL(Destination{Topic: "my-topic"})
	want := "https://ntfy.sh/my-topic"
	if got != want {
		t.Errorf("ntfyTargetURL = %q, want %q", got, want)
	}
}

func TestNtfyTargetURLUsesSelfHostedBase(t *testing.T) {
	got := ntfyTargetURL(Destination{Topic: "my-topic", URL: "https://ntfy.example.internal/"})
	want := "https://ntfy.example.internal/my-topic"
	if got != want {
		t.Errorf("ntfyTargetURL = %q, want %q", got, want)
	}
}

func TestNtfyProviderRequiresTopic(t *testing.T) {
	p := &ntfyProvider{client: testHTTPClient()}
	if err := p.Send(context.Background(), Destination{}, Notification{}); err == nil {
		t.Error("expected an error with no topic configured")
	}
}

func TestPushoverProviderSendsEmergencyPriority(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.Form
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := &pushoverProvider{client: testHTTPClient(), api: srv.URL}
	err := p.Send(context.Background(), Destination{Token: "apptok", UserKey: "userkey"},
		Notification{Title: "spillway: pool exhausted", Body: "holding"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if gotForm.Get("priority") != "2" {
		t.Errorf("priority = %q, want 2 (emergency) — the whole reason to use Pushover", gotForm.Get("priority"))
	}
	if gotForm.Get("retry") == "" || gotForm.Get("expire") == "" {
		t.Errorf("emergency priority requires retry+expire, got form %+v", gotForm)
	}
	if gotForm.Get("token") != "apptok" || gotForm.Get("user") != "userkey" {
		t.Errorf("credentials not forwarded: %+v", gotForm)
	}
}

func TestPushoverProviderRequiresCredentials(t *testing.T) {
	p := &pushoverProvider{client: testHTTPClient()}
	if err := p.Send(context.Background(), Destination{}, Notification{}); err == nil {
		t.Error("expected an error with no token/user key configured")
	}
}

func TestProviderForKnownNames(t *testing.T) {
	for _, name := range KnownProviders() {
		if _, _, ok := providerFor(name, nil, false); !ok {
			t.Errorf("providerFor(%q) not found, but it's in KnownProviders()", name)
		}
	}
}

func TestProviderForUnknownName(t *testing.T) {
	if _, _, ok := providerFor("carrier-pigeon", nil, false); ok {
		t.Error("expected ok=false for an unregistered provider")
	}
}

func TestSendDispatchesToRegisteredProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := Send(context.Background(), "webhook", Destination{URL: srv.URL}, Notification{Title: "t", Body: "b"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

func TestSendUnknownProvider(t *testing.T) {
	if err := Send(context.Background(), "carrier-pigeon", Destination{}, Notification{}); err == nil {
		t.Error("expected an error for an unknown provider")
	}
}
