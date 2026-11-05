package pairing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeCode(t *testing.T) {
	for in, want := range map[string]string{
		"K7M2-9QWD-X3": "K7M29QWDX3", "k7m2 9qwd x3": "K7M29QWDX3", "K7M29QWDX3": "K7M29QWDX3", "k7m29qwdx3": "K7M29QWDX3",
		"O7M2-9QWD-XL": "07M29QWDX1", "i7m2-9qwd-x3": "17M29QWDX3", " K7M2 - 9QWD - X3 ": "K7M29QWDX3",
	} {
		got, ok := NormalizeCode(in)
		if !ok || got != want {
			t.Errorf("%q → %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "K7M2", "K7M2-9QWD-X3X", "K7M2-9QWD-U3", "K7M2_9QWD_X3", strings.Repeat("A", 30)} {
		if _, ok := NormalizeCode(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
	if FormatCode("K7M29QWDX3") != "K7M2-9QWD-X3" {
		t.Fatal("format")
	}
}

// The key is exactly what the hub's own patterns accept (hub/src/core/keys.ts), from A-Z and 2-7.
func TestMintKey(t *testing.T) {
	hubKey := regexp.MustCompile(`^crm_([A-Za-z2-7]{8})_([A-Za-z2-7]{52})$`)
	strict := regexp.MustCompile(`^crm_[A-Z2-7]{8}_[A-Z2-7]{52}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id, secret, key := MintKey()
		if !hubKey.MatchString(key) || !strict.MatchString(key) || !ValidKey(key) || key != "crm_"+id+"_"+secret || seen[secret] || seen[id] {
			t.Fatalf("bad key shape for id %s", id)
		}
		seen[secret], seen[id] = true, true
	}
	for _, bad := range []string{"", "crm_x_y", "crm_ABCDEFGH_" + strings.Repeat("A", 51), "crm_ABCDEFG1_" + strings.Repeat("A", 52),
		"crm_ABCDEFGH_" + strings.Repeat("A", 53), "crai_ABCDEFGH_" + strings.Repeat("A", 52), "crm_ABCDEFGH_" + strings.Repeat("A", 52) + "\n"} {
		if ValidKey(bad) {
			t.Errorf("accepted a malformed key of %d characters", len(bad))
		}
	}
}

// fakeHub mirrors the state machine of the hub's POST /agent/pair (hub/src/web/pairing.ts).
type fakeHub struct {
	t   *testing.T
	srv *httptest.Server
	ws  string

	mu       sync.Mutex
	state    string // waiting | choice | paired | burned | gone
	prior    string // the code owner's live machine id, when there is one
	boundKey string
	attempts int
	takenIDs int // the first n key ids are "taken"
	slugBusy bool
	posts    []pairRequest
	rate     int // answer 429 to the next n non-poll requests
	rate202  int // answer 429 to the next n polls (the hub never does; the agent must cope)
}

func newFakeHub(t *testing.T) *fakeHub {
	h := &fakeHub{t: t, state: "waiting"}
	h.srv = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.srv.Close)
	h.ws = "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/agent"
	return h
}

func hubErr(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write([]byte(`{"error":{"code":"` + code + `","type":"hub","message":"<b>hub text must never be shown</b>"}}`))
}

func (h *fakeHub) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/agent/pair" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if r.ContentLength > 2048 {
		h.t.Errorf("body of %d bytes: the hub caps it at 2 KB", r.ContentLength)
	}
	var req pairRequest
	json.NewDecoder(r.Body).Decode(&req)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.posts = append(h.posts, req)
	key := req.KeyID + "/" + req.KeySecret
	isPoll := h.boundKey != "" && h.boundKey == key
	if isPoll && h.rate202 > 0 {
		h.rate202--
		w.Header().Set("Retry-After", "1")
		hubErr(w, 429, "rate_limit_exceeded")
		return
	}
	if !isPoll && h.rate > 0 {
		h.rate--
		w.Header().Set("Retry-After", "42")
		hubErr(w, 429, "rate_limit_exceeded")
		return
	}
	bad := func(code string, status int) {
		h.attempts++
		if h.attempts >= 5 {
			h.state = "burned"
			hubErr(w, 410, "pairing_code_burned")
			return
		}
		hubErr(w, status, code)
	}
	switch {
	case req.Code != "K7M29QWDX3" || h.state == "gone":
		hubErr(w, 404, "pairing_code_invalid")
	case h.state == "burned":
		hubErr(w, 410, "pairing_code_burned")
	case !regexp.MustCompile(`^[A-Za-z2-7]{8}$`).MatchString(req.KeyID) || !regexp.MustCompile(`^[A-Za-z2-7]{52}$`).MatchString(req.KeySecret):
		bad("invalid_request", 400)
	case h.state == "paired":
		if isPoll {
			w.Write([]byte(`{"machine_id":"01JCKZ3Q9X8YB7N6M5P4R3S2T1","slug":"anna-pc"}`))
		} else {
			hubErr(w, 404, "pairing_code_invalid") // somebody else's code now
		}
	case h.state == "choice":
		if isPoll {
			w.WriteHeader(202)
			w.Write([]byte(`{"pending":true}`))
		} else {
			bad("pairing_code_invalid", 404)
		}
	case h.takenIDs > 0:
		h.takenIDs--
		bad("key_id_taken", 409)
	case h.slugBusy:
		hubErr(w, 409, "slug_taken")
	default: // waiting
		h.boundKey = key
		if req.PriorMachineID != "" && req.PriorMachineID == h.prior {
			h.state = "choice"
			w.WriteHeader(202)
			w.Write([]byte(`{"pending":true}`))
			return
		}
		h.state = "paired"
		w.Write([]byte(`{"machine_id":"01JCKZ3Q9X8YB7N6M5P4R3S2T1","slug":"anna-pc"}`))
	}
}

func (h *fakeHub) set(fn func()) {
	h.mu.Lock()
	fn()
	h.mu.Unlock()
}

func fast() *Client { return &Client{PollEvery: 2 * time.Millisecond, PollFor: 2 * time.Second} }

func problem(t *testing.T, err error) *Error {
	t.Helper()
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("want a pairing error, got %v", err)
	}
	if strings.Contains(pe.Message(), "hub text") || strings.Contains(pe.Error(), "hub text") {
		t.Fatalf("hub text leaked into the message: %q", pe.Message())
	}
	return pe
}

func TestPairAsNew(t *testing.T) {
	h := newFakeHub(t)
	res, err := fast().Pair(context.Background(), h.ws, "K7M29QWDX3", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.MachineID != "01JCKZ3Q9X8YB7N6M5P4R3S2T1" || res.Slug != "anna-pc" || res.Hub != h.ws || !ValidKey(res.Key) {
		t.Fatalf("result for machine %s", res.MachineID)
	}
	got := h.posts[0]
	if len(h.posts) != 1 || got.PriorMachineID != "" || res.Key != "crm_"+got.KeyID+"_"+got.KeySecret || res.KeyID != got.KeyID {
		t.Fatal("the key sent is not the key returned")
	}
	// A stale prior id that is not the owner's live machine is ignored by the hub: paired as new.
	h2 := newFakeHub(t)
	if _, err := fast().Pair(context.Background(), h2.ws, "K7M29QWDX3", "01JCKZ3Q9X8YB7N6M5P4R3S2T7", nil); err != nil {
		t.Fatal(err)
	}
}

// A reinstall: 202, polls with the same key until the host chooses, then the machine.
func TestPairChoiceThenPaired(t *testing.T) {
	h := newFakeHub(t)
	h.prior = "01JCKZ3Q9X8YB7N6M5P4R3S2T1"
	waits := 0
	go func() {
		time.Sleep(30 * time.Millisecond)
		h.set(func() { h.state = "paired" }) // the host clicked Replace key
	}()
	res, err := fast().Pair(context.Background(), h.ws, "K7M29QWDX3", h.prior, func() { waits++ })
	if err != nil || res.Slug != "anna-pc" {
		t.Fatalf("%v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if waits != 1 || len(h.posts) < 3 {
		t.Fatalf("waiting called %d times over %d posts", waits, len(h.posts))
	}
	for _, p := range h.posts {
		if p.KeyID != h.posts[0].KeyID || p.KeySecret != h.posts[0].KeySecret || p.PriorMachineID != h.prior {
			t.Fatal("polls must present the same key and prior id")
		}
	}
}

func TestPairChoiceOutcomes(t *testing.T) {
	// The code burns while the host is deciding.
	h := newFakeHub(t)
	h.prior = "01JCKZ3Q9X8YB7N6M5P4R3S2T1"
	go func() { time.Sleep(20 * time.Millisecond); h.set(func() { h.state = "burned" }) }()
	_, err := fast().Pair(context.Background(), h.ws, "K7M29QWDX3", h.prior, nil)
	if pe := problem(t, err); pe.Problem != ProblemBurned || !strings.Contains(pe.Message(), "tried too often") {
		t.Fatalf("burned: %v", err)
	}
	// The code expires while the host is deciding.
	h = newFakeHub(t)
	h.prior = "01JCKZ3Q9X8YB7N6M5P4R3S2T1"
	go func() { time.Sleep(20 * time.Millisecond); h.set(func() { h.state = "gone" }) }()
	_, err = fast().Pair(context.Background(), h.ws, "K7M29QWDX3", h.prior, nil)
	if pe := problem(t, err); pe.Problem != ProblemInvalid {
		t.Fatalf("expired while pending: %v", err)
	}
	// The host never chooses.
	h = newFakeHub(t)
	h.prior = "01JCKZ3Q9X8YB7N6M5P4R3S2T1"
	c := &Client{PollEvery: 5 * time.Millisecond, PollFor: 40 * time.Millisecond}
	_, err = c.Pair(context.Background(), h.ws, "K7M29QWDX3", h.prior, nil)
	if pe := problem(t, err); pe.Problem != ProblemTimeout || !strings.Contains(pe.Message(), "Replace key") {
		t.Fatalf("no choice: %v", err)
	}
	// A poll that is rate limited anyway waits Retry-After and goes on.
	h = newFakeHub(t)
	h.prior, h.rate202 = "01JCKZ3Q9X8YB7N6M5P4R3S2T1", 2
	var slept []time.Duration
	c = fast()
	c.Sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	go func() { time.Sleep(10 * time.Millisecond); h.set(func() { h.state = "paired" }) }()
	if _, err := c.Pair(context.Background(), h.ws, "K7M29QWDX3", h.prior, nil); err != nil {
		t.Fatalf("429 on a poll: %v", err)
	}
	sawSecond := false
	for _, d := range slept {
		sawSecond = sawSecond || d == time.Second
	}
	if !sawSecond {
		t.Fatalf("Retry-After not honoured on a poll: %v", slept)
	}
}

func TestPairErrorsByName(t *testing.T) {
	ctx := context.Background()
	// Mistyped, expired and used codes are one answer from the hub: 404 pairing_code_invalid.
	h := newFakeHub(t)
	_, err := fast().Pair(ctx, h.ws, "WR0NGC0DE1", "", nil)
	if pe := problem(t, err); pe.Problem != ProblemInvalid || !strings.Contains(pe.Message(), "mistyped") ||
		!strings.Contains(pe.Message(), "10 minutes") || !strings.Contains(pe.Message(), "already used") {
		t.Fatalf("invalid: %v", err)
	}
	// A code that already paired another PC.
	if _, err := fast().Pair(ctx, h.ws, "K7M29QWDX3", "", nil); err != nil {
		t.Fatal(err)
	}
	_, err = fast().Pair(ctx, h.ws, "K7M29QWDX3", "", nil)
	if pe := problem(t, err); pe.Problem != ProblemInvalid {
		t.Fatalf("used code: %v", err)
	}
	// Burned.
	h = newFakeHub(t)
	h.state = "burned"
	_, err = fast().Pair(ctx, h.ws, "K7M29QWDX3", "", nil)
	if pe := problem(t, err); pe.Problem != ProblemBurned {
		t.Fatalf("burned: %v", err)
	}
	// The name typed in the hub is taken (a PC that lost its state, re-paired under its old name).
	h = newFakeHub(t)
	h.slugBusy = true
	_, err = fast().Pair(ctx, h.ws, "K7M29QWDX3", "", nil)
	if pe := problem(t, err); pe.Problem != ProblemSlugTaken || !strings.Contains(pe.Message(), "name") {
		t.Fatalf("slug taken: %v", err)
	}
	// 429 with Retry-After on the first request: the host is told how long to wait.
	h = newFakeHub(t)
	h.rate = 1
	_, err = fast().Pair(ctx, h.ws, "K7M29QWDX3", "", nil)
	if pe := problem(t, err); pe.Problem != ProblemRate || pe.RetryAfter != 42*time.Second || !strings.Contains(pe.Message(), "42 seconds") {
		t.Fatalf("rate: %v", err)
	}
	if len(h.posts) != 1 {
		t.Fatalf("a rate-limited first request was retried %d times", len(h.posts)-1)
	}
}

// key_id_taken: the agent mints a fresh key and tries again, at most twice more.
func TestKeyIDTaken(t *testing.T) {
	h := newFakeHub(t)
	h.takenIDs = 2
	res, err := fast().Pair(context.Background(), h.ws, "K7M29QWDX3", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.posts) != 3 || h.posts[0].KeyID == h.posts[1].KeyID || h.posts[1].KeyID == h.posts[2].KeyID || res.KeyID != h.posts[2].KeyID {
		t.Fatalf("%d posts", len(h.posts))
	}
	h = newFakeHub(t)
	h.takenIDs = 10
	_, err = fast().Pair(context.Background(), h.ws, "K7M29QWDX3", "", nil)
	if pe := problem(t, err); pe.Problem != ProblemHub || len(h.posts) != 3 {
		t.Fatalf("endless key_id_taken: %v after %d posts (each one counts against the code)", err, len(h.posts))
	}
}

func TestPairFallbacksAndBadAnswers(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   Problem
	}{
		{"404 without a body", 404, ``, ProblemInvalid},
		{"410 without a body", 410, `not json`, ProblemBurned},
		{"429 without a body", 429, ``, ProblemRate},
		{"409 with an unknown code", 409, `{"error":{"code":"something_new"}}`, ProblemHub},
		{"400 invalid_request", 400, `{"error":{"code":"invalid_request"}}`, ProblemHub},
		{"pairing_not_pending", 409, `{"error":{"code":"pairing_not_pending"}}`, ProblemNotPending},
		{"a future expired code", 410, `{"error":{"code":"pairing_code_expired"}}`, ProblemExpired},
		{"a future used code", 409, `{"error":{"code":"pairing_code_used"}}`, ProblemUsed},
		{"server error", 500, ``, ProblemHub},
		{"200 with a bad id", 200, `{"machine_id":"../../x","slug":"anna-pc"}`, ProblemHub},
		{"200 with a bad slug", 200, `{"machine_id":"01JCKZ3Q9X8YB7N6M5P4R3S2T1","slug":"Anna PC!"}`, ProblemHub},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			}))
			defer srv.Close()
			_, err := fast().Pair(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/agent", "K7M29QWDX3", "", nil)
			if pe := problem(t, err); pe.Problem != c.want || pe.Message() == "" {
				t.Fatalf("got problem %d, want %d", pe.Problem, c.want)
			}
		})
	}
}

func TestPairNeverFollowsRedirects(t *testing.T) {
	var leaked atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/agent/pair", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := fast().Pair(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http")+"/agent", "K7M29QWDX3", "", nil)
	if err == nil || leaked.Load() {
		t.Fatalf("redirect followed (leaked=%v, err=%v)", leaked.Load(), err)
	}
}

func TestPairNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ws := "ws" + strings.TrimPrefix(srv.URL, "http") + "/agent"
	srv.Close()
	_, err := fast().Pair(context.Background(), ws, "K7M29QWDX3", "", nil)
	if pe := problem(t, err); pe.Problem != ProblemNetwork {
		t.Fatalf("got %v", err)
	}
}
