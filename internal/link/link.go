// Package link keeps the agent's one outbound WebSocket to the hub (§10): Bearer auth on the upgrade,
// hello first, pong for ping, 60 s without frames = dead, full-jitter reconnect, close codes
// 4000–4003, one writer goroutine per socket, and every local request cancelled on socket loss.
package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/cheetahrunout/cr-agent/internal/protocol"
)

// Close codes (§10.4).
const (
	CloseBadKey          = 4000
	CloseUpgradeRequired = 4001
	CloseReplaced        = 4002
	CloseRevoked         = 4003
)

// Phase is where the link stands, for the tray's status line.
type Phase string

// Phases.
const (
	PhaseConnecting     Phase = "connecting"
	PhaseConnected      Phase = "connected"       // welcome received
	PhaseDown           Phase = "down"            // lost; retrying with backoff
	PhaseKeyRejected    Phase = "key_rejected"    // 4000 or HTTP 401/403 on the upgrade; retrying hourly
	PhaseUpdateRequired Phase = "update_required" // 4001; retrying hourly
	PhaseReplaced       Phase = "replaced"        // 4002; retrying hourly
	PhaseRemoved        Phase = "removed"         // 4003; stopped for good
)

// MaxPayload is the hard frame limit on both sides (§10.1).
const MaxPayload = 1 << 20

// ErrRevoked ends Run for good: the machine was removed from the hub (4003).
var ErrRevoked = errors.New("this PC was removed from the hub (4003)")

// Handler is the agent side of the link.
type Handler interface {
	// Hello returns the full state snapshot sent first on every connection.
	Hello() *protocol.Hello
	// OnWelcome is called with the hub's welcome (values already clamped).
	OnWelcome(s *Session, w *protocol.Welcome)
	// OnFrame receives cmd, req, req.body and cancel frames. It must not block.
	OnFrame(s *Session, f protocol.Frame)
	// OnDisconnect is called after the session context is cancelled.
	OnDisconnect(reason string)
}

// Config configures the client. Zero durations take the protocol defaults.
type Config struct {
	URL        string // already checked by ValidateHubURL
	Key        string // crm_<key_id>_<secret>
	Handler    Handler
	Log        *slog.Logger
	HTTPClient *http.Client

	DeadAfter    time.Duration // 60 s
	BaseDelay    time.Duration // 1 s
	MaxDelay     time.Duration // 30 s
	HealthyAfter time.Duration // 60 s: a session this long resets the backoff
	GoingAway    time.Duration // 3 s: max delay after 1001/1012
	SlowRetry    time.Duration // 1 h: after 4000, 4001, 4002 or an HTTP 401/403 on the upgrade

	// OnPhase is told every change of phase. It must not block.
	OnPhase func(Phase)

	// Sleep waits between attempts; tests replace it. Returns ctx.Err() when cancelled.
	Sleep func(ctx context.Context, d time.Duration) error
	// Rand returns a uniform value in [0, n); tests replace it.
	Rand func(n int64) int64
}

// Client runs the link.
type Client struct {
	cfg Config
	log *slog.Logger
}

// New fills defaults.
func New(cfg Config) *Client {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&cfg.DeadAfter, 60*time.Second)
	def(&cfg.BaseDelay, time.Second)
	def(&cfg.MaxDelay, 30*time.Second)
	def(&cfg.HealthyAfter, 60*time.Second)
	def(&cfg.GoingAway, 3*time.Second)
	def(&cfg.SlowRetry, time.Hour)
	if cfg.Sleep == nil {
		cfg.Sleep = sleepCtx
	}
	if cfg.Rand == nil {
		cfg.Rand = func(n int64) int64 {
			if n <= 0 {
				return 0
			}
			return rand.Int64N(n)
		}
	}
	if cfg.OnPhase == nil {
		cfg.OnPhase = func(Phase) {}
	}
	// The upgrade carries the machine key and goes to the validated host: a redirect is never followed.
	hc := http.Client{}
	if cfg.HTTPClient != nil {
		hc = *cfg.HTTPClient
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cfg.HTTPClient = &hc
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Client{cfg: cfg, log: log}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// outcome is how one connection ended.
type outcome struct {
	code     websocket.StatusCode // -1 when there was no close frame
	httpCode int                  // status of a refused upgrade
	lasted   time.Duration        // time from upgrade to loss, 0 when the dial failed
	retry    time.Duration        // Retry-After of a refused upgrade, 0 when absent
	reason   string
}

// Backoff is the full-jitter delay for attempt n: random(0, min(max, base×2^n)).
func Backoff(n int, base, max time.Duration, rnd func(int64) int64) time.Duration {
	ceil := max
	if n < 30 {
		if d := base << n; d > 0 && d < max {
			ceil = d
		}
	}
	return time.Duration(rnd(int64(ceil) + 1))
}

// Run connects and reconnects until ctx ends or the machine is revoked.
func (c *Client) Run(ctx context.Context) error {
	n := 0
	for {
		c.cfg.OnPhase(PhaseConnecting)
		o := c.connectOnce(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if o.lasted >= c.cfg.HealthyAfter {
			n = 0
		}
		var delay time.Duration
		switch {
		case o.code == CloseRevoked:
			c.log.Error("link stopped: this PC was removed from the hub", "code", int(o.code))
			c.cfg.OnPhase(PhaseRemoved)
			return ErrRevoked
		case o.code == CloseBadKey || o.httpCode == http.StatusUnauthorized || o.httpCode == http.StatusForbidden:
			c.log.Error("key not accepted; retrying hourly", "code", int(o.code), "http", o.httpCode)
			c.cfg.OnPhase(PhaseKeyRejected)
			delay = c.cfg.SlowRetry
		case o.code == CloseUpgradeRequired:
			c.log.Error("update required: the hub speaks a newer protocol major", "code", int(o.code))
			c.cfg.OnPhase(PhaseUpdateRequired)
			delay = c.cfg.SlowRetry
		case o.code == CloseReplaced:
			c.log.Warn("replaced by another connection with this machine's key; retrying hourly", "code", int(o.code))
			c.cfg.OnPhase(PhaseReplaced)
			delay = c.cfg.SlowRetry
		case o.httpCode == http.StatusTooManyRequests:
			// The hub is slowing this address or this machine down (a block after bad keys, or reconnects
			// that came too fast). It says how long; wait that, plus a little jitter, and try again.
			// This is not a key problem and not a removal.
			c.cfg.OnPhase(PhaseDown)
			wait := o.retry
			if wait <= 0 {
				wait = time.Minute
			}
			delay = min(wait, c.cfg.SlowRetry) + time.Duration(c.cfg.Rand(int64(c.cfg.GoingAway)+1))
			c.log.Warn("the hub asked this PC to wait before reconnecting", "wait_s", int(delay/time.Second))
		case o.code == websocket.StatusServiceRestart || o.code == websocket.StatusGoingAway:
			c.cfg.OnPhase(PhaseDown)
			delay = time.Duration(c.cfg.Rand(int64(c.cfg.GoingAway) + 1))
		default:
			c.cfg.OnPhase(PhaseDown)
			delay = Backoff(n, c.cfg.BaseDelay, c.cfg.MaxDelay, c.cfg.Rand)
			n++
		}
		c.log.Info("link down", "reason", o.reason, "retry_ms", delay.Milliseconds())
		if err := c.cfg.Sleep(ctx, delay); err != nil {
			return err
		}
	}
}

func (c *Client) connectOnce(ctx context.Context) outcome {
	dctx, dcancel := context.WithTimeout(ctx, 20*time.Second)
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+c.cfg.Key)
	conn, resp, err := websocket.Dial(dctx, c.cfg.URL, &websocket.DialOptions{
		HTTPHeader:      hdr,
		HTTPClient:      c.cfg.HTTPClient,
		CompressionMode: websocket.CompressionDisabled,
	})
	dcancel()
	if err != nil {
		o := outcome{code: -1, reason: "dial failed"}
		if resp != nil {
			o.httpCode = resp.StatusCode
			o.reason = fmt.Sprintf("upgrade refused: HTTP %d", resp.StatusCode)
			if n, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && n > 0 {
				o.retry = time.Duration(n) * time.Second
			}
		}
		c.log.Warn("dial failed", "err", err.Error(), "http", o.httpCode)
		return o
	}
	conn.SetReadLimit(MaxPayload)
	start := time.Now()
	c.log.Info("link up")

	sctx, cancel := context.WithCancel(ctx)
	s := &Session{ctx: sctx, out: make(chan outMsg, 64), log: c.log}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.writer(conn, cancel) }()

	o := outcome{code: -1}
	if err := s.Send(c.cfg.Handler.Hello()); err != nil {
		o.reason = "hello not sent"
	} else {
		o = c.readLoop(sctx, conn, s)
	}
	cancel() // cancels every local request in flight on this socket
	wg.Wait()
	conn.CloseNow()
	o.lasted = time.Since(start)
	c.cfg.Handler.OnDisconnect(o.reason)
	return o
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn, s *Session) outcome {
	for {
		rctx, rcancel := context.WithTimeout(ctx, c.cfg.DeadAfter)
		typ, data, err := conn.Read(rctx)
		timedOut := rctx.Err() == context.DeadlineExceeded && ctx.Err() == nil
		rcancel()
		if err != nil {
			code := websocket.CloseStatus(err)
			switch {
			case code != -1:
				return outcome{code: code, reason: fmt.Sprintf("closed %d", int(code))}
			case timedOut:
				return outcome{code: -1, reason: "no frame for 60 s"}
			case ctx.Err() != nil:
				return outcome{code: -1, reason: "write failed"}
			default:
				return outcome{code: -1, reason: "connection lost"}
			}
		}
		if typ != websocket.MessageText {
			continue
		}
		f, err := protocol.Decode(data)
		if err != nil {
			c.decodeError(s, err)
			continue
		}
		switch f := f.(type) {
		case *protocol.Ping:
			_ = s.Send(&protocol.Pong{TS: f.TS})
		case *protocol.Welcome:
			clampWelcome(f)
			c.log.Info("welcome", "proto", Clip(f.Proto), "mode", f.Mode, "ping_interval_s", f.PingIntervalS)
			c.cfg.Handler.OnWelcome(s, f)
			c.cfg.OnPhase(PhaseConnected)
		case *protocol.Reject:
			c.log.Warn("hub rejected the connection", "code", f.Code) // hub text is never shown locally
		case *protocol.Cmd, *protocol.Req, *protocol.ReqBody, *protocol.Cancel:
			c.cfg.Handler.OnFrame(s, f)
		default:
			c.log.Debug("frame ignored", "t", f.Type())
		}
	}
}

func (c *Client) decodeError(s *Session, err error) {
	var ut *protocol.UnknownTypeError
	var ce *protocol.CmdError
	var re *protocol.ReqError
	switch {
	case errors.As(err, &ut):
		c.log.Info("unknown frame type dropped", "t", Clip(ut.T))
	case errors.As(err, &ce):
		c.log.Warn("command refused", "id", ce.ID, "op", Clip(ce.Op), "code", ce.Code, "field", Clip(ce.Field))
		if ce.ID != "" {
			_ = s.Send(&protocol.CmdErr{ID: ce.ID, Code: ce.Code, Field: ce.Field, Message: ce.Msg})
		}
	case errors.As(err, &re):
		c.log.Warn("request refused", "id", re.ID, "why", Clip(re.Msg))
		if re.ID != "" {
			_ = s.Send(&protocol.ResHead{ID: re.ID, Status: http.StatusBadRequest, CType: "application/json"})
			_ = s.Send(&protocol.ResEnd{ID: re.ID, Reason: protocol.ReasonError, Error: "invalid_request"})
		}
	default:
		c.log.Warn("malformed frame dropped", "err", Clip(err.Error()))
	}
}

// Clip shortens a string that came from the hub before it goes to the log (finding L4).
func Clip(s string) string {
	const max = 120
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 { // not in the middle of a UTF-8 character
		cut--
	}
	return s[:cut] + "…"
}

// clampWelcome keeps hub values inside agent-side bounds (ruling B14).
func clampWelcome(w *protocol.Welcome) {
	if w.PingIntervalS < 5 {
		w.PingIntervalS = 5
	}
	if w.PingIntervalS > 60 {
		w.PingIntervalS = 60
	}
	if w.Limits.MaxFrame <= 0 || w.Limits.MaxFrame > MaxPayload {
		w.Limits.MaxFrame = 256 * 1024
	}
	if w.Limits.MaxBody <= 0 || w.Limits.MaxBody > 256*1024 {
		w.Limits.MaxBody = 256 * 1024
	}
	if w.Mode != "update_required" {
		w.Mode = "normal"
	}
}

// Session is one live socket. Its context ends when the socket is lost.
type Session struct {
	ctx context.Context
	out chan outMsg
	log *slog.Logger
}

// outMsg is a frame for the writer, or a request to close the socket after the frames before it.
type outMsg struct {
	data   []byte
	close  bool
	code   websocket.StatusCode
	reason string
}

// Close sends a close frame with code after every frame already queued (1001 before the PC sleeps).
func (s *Session) Close(code int, reason string) {
	select {
	case s.out <- outMsg{close: true, code: websocket.StatusCode(code), reason: reason}:
	case <-s.ctx.Done():
	}
}

// Context ends when the socket is lost.
func (s *Session) Context() context.Context { return s.ctx }

// Send encodes a frame and queues it for the writer; it blocks while the queue is full
// (backpressure) and fails once the socket is gone.
func (s *Session) Send(f protocol.Frame) error {
	b, err := protocol.Encode(f)
	if err != nil {
		return err
	}
	if len(b) > MaxPayload {
		return fmt.Errorf("frame %s too large (%d bytes)", f.Type(), len(b))
	}
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	default:
	}
	select {
	case s.out <- outMsg{data: b}:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

// writer is the only goroutine that writes to the socket.
func (s *Session) writer(conn *websocket.Conn, fail context.CancelFunc) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case m := <-s.out:
			if m.close {
				// The close frame leaves at once; the peer's answer is awaited only briefly, so a
				// PC going to sleep is never held up by a slow hub.
				closed := make(chan struct{})
				go func() { conn.Close(m.code, m.reason); close(closed) }()
				select {
				case <-closed:
				case <-time.After(500 * time.Millisecond):
				}
				fail()
				return
			}
			wctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
			err := conn.Write(wctx, websocket.MessageText, m.data)
			cancel()
			if err != nil {
				if s.ctx.Err() == nil {
					s.log.Warn("write failed", "err", err.Error())
				}
				fail()
				return
			}
		}
	}
}
