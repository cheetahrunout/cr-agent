// Package pairing implements ruling B1: pairing is an HTTP exchange. The host pastes the pairing string
// from the hub page; the agent mints the machine key, posts it to POST /agent/pair on the accepted hub
// host, and gets the hub-minted machine id back. Nothing the hub answers is shown to the host as text:
// every message comes from the agent's own table (ruling B14).
//
// The wire contract is the hub's code (hub/src/web/pairing.ts): 200 {machine_id, slug}; 202 {pending}
// while the host chooses Replace key or a new name; errors as {error:{code}} with pairing_code_invalid
// (404), pairing_code_burned (410), slug_taken and key_id_taken (409), and 429 with Retry-After.
package pairing

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cheetahrunout/cr-agent/internal/link"
)

// Problem is why a pairing failed.
type Problem int

// The problems a host can meet. The hub answers a mistyped, an expired and an already used code the
// same way (404 pairing_code_invalid), so the agent can only name all three in one message.
const (
	ProblemInput      Problem = iota + 1 // not a pairing code or link
	ProblemHost                          // a link to a hub this build does not accept
	ProblemInvalid                       // pairing_code_invalid: mistyped, expired or already used
	ProblemExpired                       // pairing_code_expired (the hub does not send it today)
	ProblemUsed                          // pairing_code_used (the hub does not send it today)
	ProblemBurned                        // pairing_code_burned: five bad attempts
	ProblemSlugTaken                     // slug_taken: the machine name typed in the hub is in use
	ProblemNotPending                    // pairing_not_pending
	ProblemRate                          // 429: too many attempts from this address
	ProblemTimeout                       // the host did not choose Replace key or a new name in time
	ProblemNetwork                       // the hub could not be reached
	ProblemHub                           // an answer the agent does not understand
	ProblemStore                         // the key could not be saved on this PC
)

// Error is a failed pairing.
type Error struct {
	Problem    Problem
	RetryAfter time.Duration // with ProblemRate, when the hub said how long to wait
	cause      error
}

func (e *Error) Error() string { return "pairing: " + e.Message() }
func (e *Error) Unwrap() error { return e.cause }

// Message is the text shown to the host. It never contains text from the hub (ruling B14).
func (e *Error) Message() string {
	switch e.Problem {
	case ProblemInput:
		return "That doesn't look like a pairing code. Copy the code from the hub's \"Add a machine\" page (it looks like K7M2-9QWD-X3)."
	case ProblemHost:
		return "That pairing link points to a hub this app doesn't work with. Copy the code from your hub's \"Add a machine\" page."
	case ProblemInvalid:
		return "The hub doesn't accept that code. It may be mistyped, older than 10 minutes, or already used. Check it against the hub page, or get a new code there."
	case ProblemExpired:
		return "That code has expired (codes last 10 minutes). Get a new one in the hub."
	case ProblemUsed:
		return "That code was already used to pair a PC. Get a new one in the hub."
	case ProblemBurned:
		return "That code was tried too often and no longer works. Get a new one in the hub."
	case ProblemSlugTaken:
		return "The machine name chosen in the hub is already in use. In the hub, remove the old machine or pick another name, then get a new code."
	case ProblemNotPending:
		return "The hub has nothing waiting for that code. Get a new one in the hub."
	case ProblemRate:
		if e.RetryAfter > 0 {
			return fmt.Sprintf("Too many attempts. Wait %s, then try again.", waitText(e.RetryAfter))
		}
		return "Too many attempts. Wait a minute, then try again."
	case ProblemTimeout:
		return "The hub is still waiting for you to choose \"Replace key\" or a new name. Get a new code in the hub and try again."
	case ProblemNetwork:
		return "Couldn't reach the hub. Check your internet connection and try again."
	case ProblemStore:
		return "Paired, but the key could not be saved on this PC. Try again."
	default:
		return "The hub gave an answer this app doesn't understand. You may need a newer version of the app."
	}
}

func waitText(d time.Duration) string {
	if d < 90*time.Second {
		return fmt.Sprintf("%d seconds", int(d.Round(time.Second)/time.Second))
	}
	return fmt.Sprintf("%d minutes", int((d+59*time.Second)/time.Minute))
}

func fail(p Problem, cause error) *Error { return &Error{Problem: p, cause: cause} }

var (
	reCode      = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{10}$`)
	reMachineID = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	reSlug      = regexp.MustCompile(`^[a-z0-9-]{2,20}$`)
	reKey       = regexp.MustCompile(`^crm_[A-Za-z2-7]{8}_[A-Za-z2-7]{52}$`)
)

// ValidMachineID checks a hub-minted machine id (ULID).
func ValidMachineID(s string) bool { return reMachineID.MatchString(s) }

// ValidKey checks a machine key against the hub's own pattern: crm_<8 base32>_<52 base32>.
func ValidKey(k string) bool { return reKey.MatchString(k) }

// NormalizeCode maps what a person types to the canonical code: any case, dashes and spaces
// ignored, O→0 and I/L→1 (Crockford base32, the same rule as the hub).
func NormalizeCode(in string) (string, bool) {
	if len(in) > 24 {
		return "", false
	}
	s := strings.ToUpper(in)
	s = strings.NewReplacer(" ", "", "-", "", "\t", "", "O", "0", "I", "1", "L", "1").Replace(s)
	return s, reCode.MatchString(s)
}

// ParseInput accepts a bare code (paired with defaultHub) or the full pairing string
// https://<hub host>/agent#<code>. It returns the validated hub WebSocket URL and the canonical code.
func ParseInput(in, defaultHub string) (hub, code string, err error) {
	in = strings.TrimSpace(in)
	if len(in) > 200 {
		return "", "", fail(ProblemInput, nil)
	}
	if i := strings.IndexByte(in, '#'); i >= 0 {
		hub, err = link.FromPairingLink(in[:i])
		if err != nil {
			return "", "", fail(ProblemHost, err)
		}
		in = in[i+1:]
	} else if strings.Contains(in, "://") {
		return "", "", fail(ProblemInput, nil)
	} else {
		if hub, err = link.ValidateHubURL(defaultHub); err != nil {
			return "", "", fail(ProblemHost, err)
		}
	}
	code, ok := NormalizeCode(in)
	if !ok {
		return "", "", fail(ProblemInput, nil)
	}
	return hub, code, nil
}

// FormatCode shows a canonical code as XXXX-XXXX-XX.
func FormatCode(c string) string {
	if len(c) != 10 {
		return c
	}
	return c[:4] + "-" + c[4:8] + "-" + c[8:]
}

const b32 = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567" // RFC 4648

func randBase32(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = b32[b[i]&31] // 256 % 32 == 0, so this is uniform
	}
	return string(b)
}

// MintKey makes a machine key on this PC from crypto/rand: an id of exactly 8 and a secret of exactly
// 52 RFC 4648 base32 characters (260 bits), in the hub's format crm_<key_id>_<secret>.
func MintKey() (keyID, secret, key string) {
	keyID = randBase32(8)
	secret = randBase32(52)
	return keyID, secret, "crm_" + keyID + "_" + secret
}

// Result is a finished pairing.
type Result struct {
	Hub       string // validated WebSocket URL
	MachineID string
	Slug      string
	KeyID     string
	Key       string // crm_<key_id>_<secret>; never logged
}

// Client talks to POST /agent/pair.
type Client struct {
	HTTP      *http.Client  // default: 15 s timeout; redirects are always refused
	PollEvery time.Duration // 3 s
	PollFor   time.Duration // 10 min
	Sleep     func(ctx context.Context, d time.Duration) error
}

func (c *Client) defaults() {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if c.PollEvery <= 0 {
		c.PollEvery = 3 * time.Second
	}
	if c.PollFor <= 0 {
		c.PollFor = 10 * time.Minute
	}
	if c.Sleep == nil {
		c.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

type pairRequest struct {
	Code           string `json:"code"`
	KeyID          string `json:"key_id"`
	KeySecret      string `json:"key_secret"`
	PriorMachineID string `json:"prior_machine_id,omitempty"`
}

type pairResponse struct {
	MachineID string `json:"machine_id"`
	Slug      string `json:"slug"`
	Pending   bool   `json:"pending"`
	Error     *struct {
		Code string `json:"code"`
	} `json:"error"`
}

// errKeyIDTaken is the hub's key_id_taken: the 40-bit id this PC minted is in use. A new key fixes it.
var errKeyIDTaken = errors.New("key id taken")

// Pair runs the exchange. priorMachineID is sent when this PC was paired before, so the hub can offer
// "Replace key"; the hub then answers 202 and waiting is called once while the host chooses in the
// browser. While the answer is 202 the same request, presenting the same key, is repeated every 3 s
// (the hub does not rate-limit such polls) until the hub says paired, burned or invalid, for at most 10 min.
func (c *Client) Pair(ctx context.Context, hub, code, priorMachineID string, waiting func()) (*Result, error) {
	c.defaults()
	endpoint, err := link.PairEndpoint(hub)
	if err != nil {
		return nil, fail(ProblemHost, err)
	}
	deadline := time.Now().Add(c.PollFor)
	for mint := 0; ; mint++ {
		keyID, secret, key := MintKey()
		body, _ := json.Marshal(pairRequest{Code: code, KeyID: keyID, KeySecret: secret, PriorMachineID: priorMachineID})
		res, err := c.exchange(ctx, endpoint, body, deadline, waiting)
		if errors.Is(err, errKeyIDTaken) {
			// Each refusal counts against the code (five burn it), so a new key is tried twice at most.
			if mint >= 2 {
				return nil, fail(ProblemHub, err)
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		res.Hub, res.KeyID, res.Key = hub, keyID, key
		return res, nil
	}
}

// exchange posts one key until the hub gives a final answer.
func (c *Client) exchange(ctx context.Context, endpoint string, body []byte, deadline time.Time, waiting func()) (*Result, error) {
	polling := false
	for {
		res, pending, err := c.post(ctx, endpoint, body)
		wait := c.PollEvery
		var pe *Error
		switch {
		case polling && errors.As(err, &pe) && pe.Problem == ProblemRate:
			// Polls are exempt from the hub's limit; if one is refused anyway, wait as told and go on.
			wait = min(max(pe.RetryAfter, c.PollEvery), time.Minute)
		case err != nil:
			return nil, err
		case !pending:
			return res, nil
		default:
			if !polling && waiting != nil {
				waiting()
			}
			polling = true
		}
		if time.Now().Add(wait).After(deadline) {
			return nil, fail(ProblemTimeout, nil)
		}
		if err := c.Sleep(ctx, wait); err != nil {
			return nil, fail(ProblemNetwork, err)
		}
	}
}

func (c *Client) post(ctx context.Context, endpoint string, body []byte) (*Result, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, fail(ProblemHost, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	hc := *c.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse } // the key never follows a redirect
	resp, err := hc.Do(req)
	if err != nil {
		return nil, false, fail(ProblemNetwork, errors.New("request failed")) // the cause may quote the URL; keep it short
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var pr pairResponse
	_ = json.Unmarshal(raw, &pr) // an unreadable body is judged by the status alone

	switch resp.StatusCode {
	case http.StatusOK:
		if !reMachineID.MatchString(pr.MachineID) || !reSlug.MatchString(pr.Slug) {
			return nil, false, fail(ProblemHub, errors.New("bad machine id or slug"))
		}
		return &Result{MachineID: pr.MachineID, Slug: pr.Slug}, false, nil
	case http.StatusAccepted:
		return nil, true, nil
	}
	if pr.Error != nil && pr.Error.Code == "key_id_taken" {
		return nil, false, errKeyIDTaken
	}
	e := fail(problemFor(resp.StatusCode, pr), fmt.Errorf("HTTP %d", resp.StatusCode))
	if e.Problem == ProblemRate {
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 && n <= 3600 {
			e.RetryAfter = time.Duration(n) * time.Second
		}
	}
	return nil, false, e
}

// problemFor reads the hub's error code by name (hub/src/web/http.ts) and falls back to the HTTP status.
func problemFor(status int, pr pairResponse) Problem {
	if pr.Error != nil {
		switch pr.Error.Code {
		case "pairing_code_invalid":
			return ProblemInvalid
		case "pairing_code_burned":
			return ProblemBurned
		case "slug_taken":
			return ProblemSlugTaken
		case "pairing_not_pending":
			return ProblemNotPending
		case "rate_limit_exceeded":
			return ProblemRate
		case "pairing_code_expired":
			return ProblemExpired
		case "pairing_code_used":
			return ProblemUsed
		}
	}
	switch status {
	case http.StatusNotFound:
		return ProblemInvalid
	case http.StatusGone:
		return ProblemBurned
	case http.StatusTooManyRequests:
		return ProblemRate
	}
	return ProblemHub
}
