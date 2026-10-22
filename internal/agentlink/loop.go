// This file holds the machine agent loop (spec §7.1): hello, snapshot when
// asked, then flush queued events, long-poll requests, apply them, post
// results and heartbeat, reconnecting with backoff when the server is away.
package agentlink

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"time"
)

const (
	// MaxBatch is the most events per POST /events and results per POST
	// /results the server accepts.
	MaxBatch = 500

	defaultPollWait   = 30 * time.Second
	defaultHeartbeat  = 30 * time.Second
	defaultBackoffMin = time.Second
	defaultBackoffMax = 60 * time.Second
)

// LoopConfig configures Run. TownRoot, ServerURL, MachineID, Token, Collect
// and Apply are required; zero durations take the defaults (30 s long-poll
// and heartbeat, 1 s to 60 s backoff).
type LoopConfig struct {
	TownRoot, ServerURL, MachineID, Token string
	GtbVersion                            string

	// Collect builds a full snapshot of the machine. The loop overwrites its
	// Cursor (see Run).
	Collect func() (Snapshot, error)
	// Apply applies requests that are neither expired nor already applied.
	Apply Applier
	// Scan, when set, runs before each heartbeat; production uses it to
	// queue artifact events for artifacts whose sha changed.
	Scan func() error
	// Now is the machine clock used for request expiry; nil means time.Now.
	Now func() time.Time
	// Logf receives log lines; nil discards them. The token is never logged.
	Logf func(format string, args ...any)
	// HTTPClient overrides the HTTP client (per-call timeouts still apply).
	HTTPClient *http.Client

	PollWait, HeartbeatEvery, BackoffMin, BackoffMax time.Duration
}

// Run runs the agent loop until ctx is cancelled (it then returns nil) or the
// server rejects the token (it logs "agentlink: token rejected; run gtb join
// again" and returns ErrTokenRejected).
//
// Session:
//  1. POST hello. If the server answers resync, send a snapshot.
//  2. Loop: flush pending events in batches of up to 500 and Ack the cursor
//     the server returns; long-poll GET requests; apply each request and post
//     all results in one batch; heartbeat every HeartbeatEvery.
//
// A 409 resync from POST /events sends a snapshot and carries on. Network
// errors and 5xx answers end the session; Run reconnects after an
// exponential backoff (BackoffMin doubling to BackoffMax, with jitter).
// Nothing queued is dropped: events stay in the outbox until acked.
//
// Snapshot cursor: a snapshot is the machine's whole state, so it supersedes
// every queued state event. The loop sets Snapshot.Cursor to the queue's last
// ID, posts it, re-queues the pending events that are not state (evidence;
// they get new IDs above that cursor), then acks the cursor. If the machine
// stops between the post and the ack, the next POST /events gets 409 and
// resyncs again, so at worst an evidence event is sent twice.
//
// Requests: every request delivered gets a Result posted (R33), including
// ones that are dropped. A request whose ExpiresAt is not after Now answers
// {ok:false, error:"expired"} without calling Apply. A key found in the
// applied set (<townRoot>/.runtime/agentlink/applied.jsonl, last 10k keys)
// gets its stored result re-posted without calling Apply. Every applied
// result is stored before it is posted. A successful "snapshot" request
// makes the loop resync.
func Run(ctx context.Context, cfg LoopConfig) error {
	if cfg.TownRoot == "" || cfg.ServerURL == "" || cfg.MachineID == "" || cfg.Token == "" || cfg.Collect == nil || cfg.Apply == nil {
		return errors.New("agentlink: Run: TownRoot, ServerURL, MachineID, Token, Collect and Apply are required")
	}
	l, err := newLoop(cfg)
	if err != nil {
		return err
	}
	backoff := l.cfg.BackoffMin
	for {
		progressed, err := l.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrTokenRejected) {
			l.logf("agentlink: token rejected; run gtb join again")
			return ErrTokenRejected
		}
		if progressed {
			backoff = l.cfg.BackoffMin
		}
		wait := backoff
		if !transient(err) {
			// A refusal that retrying cannot fix quickly (protocol mismatch,
			// forbidden, bad request): keep trying, but slowly.
			wait = l.cfg.BackoffMax
		}
		l.logf("agentlink: disconnected: %v; retrying in %s", err, wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(jitter(wait)):
		}
		backoff = min(backoff*2, l.cfg.BackoffMax)
	}
}

// jitter returns a duration in [d/2, d].
func jitter(d time.Duration) time.Duration {
	if d <= 1 {
		return d
	}
	return d/2 + rand.N(d/2+1)
}

type loop struct {
	cfg     LoopConfig
	c       *client
	q       *Queue
	applied *AppliedSet

	resync        bool // send a snapshot before the next flush
	lastHeartbeat time.Time
}

func newLoop(cfg LoopConfig) (*loop, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.PollWait <= 0 {
		cfg.PollWait = defaultPollWait
	}
	if cfg.HeartbeatEvery <= 0 {
		cfg.HeartbeatEvery = defaultHeartbeat
	}
	if cfg.BackoffMin <= 0 {
		cfg.BackoffMin = defaultBackoffMin
	}
	if cfg.BackoffMax < cfg.BackoffMin {
		cfg.BackoffMax = max(defaultBackoffMax, cfg.BackoffMin)
	}
	applied, err := OpenAppliedSet(cfg.TownRoot, DefaultAppliedKeys)
	if err != nil {
		return nil, err
	}
	return &loop{
		cfg:     cfg,
		c:       newClient(cfg.ServerURL, cfg.Token, cfg.HTTPClient),
		q:       OpenQueue(cfg.TownRoot),
		applied: applied,
	}, nil
}

func (l *loop) logf(format string, args ...any) { l.cfg.Logf(format, args...) }

// session runs one connection until an error. progressed reports whether
// hello succeeded (so the caller resets its backoff).
func (l *loop) session(ctx context.Context) (progressed bool, err error) {
	host, _ := os.Hostname()
	resync, err := l.c.hello(ctx, Hello{
		MachineID: l.cfg.MachineID, Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH,
		GtbVersion: l.cfg.GtbVersion, Protocol: ProtocolVersion,
	})
	if err != nil {
		return false, err
	}
	l.logf("agentlink: connected to %s", l.cfg.ServerURL)
	l.resync = l.resync || resync
	for {
		if l.resync {
			if err := l.sendSnapshot(ctx); err != nil {
				return true, err
			}
		}
		if err := l.flush(ctx); err != nil {
			return true, err
		}
		if l.resync {
			continue // a 409 during flush: snapshot, then flush again
		}
		if time.Since(l.lastHeartbeat) >= l.cfg.HeartbeatEvery { // wall clock; Now is for expiry
			if l.cfg.Scan != nil {
				if err := l.cfg.Scan(); err != nil {
					l.logf("agentlink: scan: %v", err)
				}
			}
			if err := l.c.heartbeat(ctx); err != nil {
				return true, err
			}
			l.lastHeartbeat = time.Now()
			if err := l.flush(ctx); err != nil { // events the scan queued
				return true, err
			}
			if l.resync {
				continue
			}
		}
		reqs, err := l.c.requests(ctx, l.cfg.PollWait)
		if err != nil {
			return true, err
		}
		if err := l.handle(ctx, reqs); err != nil {
			return true, err
		}
	}
}

// flush sends every pending event in batches of MaxBatch, acking the cursor
// the server returns after each batch. A 409 sets l.resync and returns nil.
func (l *loop) flush(ctx context.Context) error {
	for {
		after, err := l.q.Cursor()
		if err != nil {
			return err
		}
		evs, err := l.q.Pending(MaxBatch)
		if err != nil {
			return err
		}
		if len(evs) == 0 {
			return nil
		}
		cursor, err := l.c.events(ctx, after, evs)
		if errors.Is(err, errResync) {
			l.resync = true
			return nil
		}
		if err != nil {
			return err
		}
		if err := l.q.Ack(cursor); err != nil {
			return err
		}
		if len(evs) < MaxBatch {
			return nil
		}
	}
}

// sendSnapshot collects and posts a snapshot (see Run for the cursor rule).
func (l *loop) sendSnapshot(ctx context.Context) error {
	last, err := l.q.LastID()
	if err != nil {
		return err
	}
	acked, err := l.q.Cursor()
	if err != nil {
		return err
	}
	var keep []Event
	if last > acked {
		pend, err := l.q.Pending(int(^uint(0) >> 1))
		if err != nil {
			return err
		}
		for _, e := range pend {
			if e.ID <= last && e.Type == EventEvidence {
				keep = append(keep, e)
			}
		}
	}
	snap, err := l.cfg.Collect()
	if err != nil {
		return fmt.Errorf("agentlink: collect snapshot: %w", err)
	}
	snap.Cursor = last
	if err := l.c.snapshot(ctx, snap); err != nil {
		return err
	}
	for _, e := range keep {
		e.Data = append(e.Data[:0:0], e.Data...)
		if err := l.q.Append(e); err != nil {
			return err
		}
	}
	if err := l.q.Ack(last); err != nil {
		return err
	}
	l.resync = false
	l.logf("agentlink: sent snapshot (%d items, cursor %s)", len(snap.Items), last)
	return nil
}

// handle applies reqs and posts their results.
func (l *loop) handle(ctx context.Context, reqs []Request) error {
	if len(reqs) == 0 {
		return nil
	}
	results := make([]Result, 0, len(reqs))
	for _, r := range reqs {
		results = append(results, l.apply(ctx, r))
	}
	for len(results) > 0 {
		n := min(len(results), MaxBatch)
		if err := l.c.results(ctx, results[:n]); err != nil {
			return err
		}
		results = results[n:]
	}
	return nil
}

func (l *loop) apply(ctx context.Context, r Request) Result {
	if !r.ExpiresAt.After(l.cfg.Now()) {
		l.logf("agentlink: request %s (%s) expired; dropped", r.Key, r.Type)
		return Fail(r.Key, ErrCodeExpired)
	}
	if res, ok := l.applied.Get(r.Key); ok {
		l.logf("agentlink: request %s already applied; re-posting its result", r.Key)
		res.Key = r.Key
		return res
	}
	res := l.safeApply(ctx, r)
	res.Key = r.Key
	if err := l.applied.Put(res); err != nil {
		l.logf("agentlink: record applied request %s: %v", r.Key, err)
	}
	if r.Type == "snapshot" && res.OK {
		l.resync = true
	}
	return res
}

func (l *loop) safeApply(ctx context.Context, r Request) (res Result) {
	defer func() {
		if p := recover(); p != nil {
			l.logf("agentlink: request %s (%s) panicked: %v", r.Key, r.Type, p)
			res = Fail(r.Key, ErrCodeFailed)
		}
	}()
	return l.cfg.Apply.Apply(ctx, r)
}
