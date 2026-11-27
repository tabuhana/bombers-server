// Package deadline puts a time limit on every HTTP request.
//
// Without one, a stalled request (a database call, an object-storage call, or
// a client that stopped sending) never ends on either side. There are three
// classes of request:
//
//   - Quick is the default for EVERY request, including 404s and CORS
//     preflights: 30 seconds, on the request context (so pgx and object-storage
//     calls made with r.Context() are cancelled) and on the connection's read
//     and write deadlines. A new route is safe by default; a longer budget has
//     to be DECLARED where the route is registered.
//   - Transfer is for routes that move bytes: a ceiling of ten minutes in
//     total, and a stall limit of one minute. The connection is cut when no
//     body bytes arrive for a minute while the handler is reading the body, or
//     no response bytes are written for a minute while it is writing.
//   - Live is for the socket routes: no limit at all. The context deadline is
//     removed and the connection deadlines are cleared, because the socket
//     handlers run their loops on r.Context() and have their own ping/pong
//     liveness.
//
// Middleware gives every request the Quick class. It cannot just use
// context.WithTimeout: a derived context can only SHORTEN a deadline, never
// move it later, and a route-level Transfer or Live has to be able to do that.
// So the budget is a timer that cancels the context when it fires, and Transfer
// and Live reach it through the context to reset or stop it.
//
// The connection half of Quick is not set here. The http.Server's ReadTimeout
// and WriteTimeout (set to Default.Quick in main) already give every request
// those deadlines, and the server resets them for each request on a keep-alive
// connection. Transfer and Live only move them per route.
//
// One net/http detail shapes Transfer. Once a handler has consumed the request
// body, the server starts a background read on the connection to notice a
// client that hung up, and if THAT read hits the read deadline the server
// cancels the request context. So the read deadline must never sit at a short
// value while the handler is still working: it is only pulled in to the stall
// limit for the duration of a body read, and put back to the ceiling after.
package deadline

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// HeaderTimeout and IdleTimeout are for the http.Server itself.
const (
	HeaderTimeout = 10 * time.Second
	IdleTimeout   = 2 * time.Minute
)

// ErrTimedOut is the cause on a request context that ran out of time.
var ErrTimedOut = errors.New("request took too long")

// Limits are the three durations the classes use.
type Limits struct {
	// Quick is the whole budget of an ordinary request.
	Quick time.Duration
	// TransferTotal is the ceiling on a Transfer request.
	TransferTotal time.Duration
	// TransferStall is how long a Transfer request may go without a body byte
	// arriving or a response byte leaving.
	TransferStall time.Duration
}

// Default is what the server runs with.
var Default = Limits{Quick: 30 * time.Second, TransferTotal: 10 * time.Minute, TransferStall: time.Minute}

// Deadlines holds the limits and builds the middlewares.
type Deadlines struct {
	l Limits
}

// New returns the middlewares for the given limits.
func New(l Limits) *Deadlines { return &Deadlines{l: l} }

type budgetKey struct{}

// budget is the per-request timer that cancels the context.
type budget struct {
	timer *time.Timer
}

// Middleware gives the request the Quick class. Apply it to the whole router.
func (d *Deadlines) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancelCause(r.Context())
		b := &budget{timer: time.AfterFunc(d.l.Quick, func() { cancel(ErrTimedOut) })}
		defer func() {
			b.timer.Stop()
			cancel(nil)
		}()
		ctx = context.WithValue(ctx, budgetKey{}, b)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Transfer moves a route to the Transfer class. It must run after Middleware.
func (d *Deadlines) Transfer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := r.Context().Value(budgetKey{}).(*budget)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		b.timer.Reset(d.l.TransferTotal)

		ceiling := time.Now().Add(d.l.TransferTotal)
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(ceiling)
		_ = rc.SetWriteDeadline(ceiling)

		r2 := r.WithContext(r.Context())
		if r.Body != nil && r.Body != http.NoBody {
			r2.Body = &stallReader{rc: rc, body: r.Body, stall: d.l.TransferStall, ceiling: ceiling}
		}
		next.ServeHTTP(&stallWriter{ResponseWriter: w, rc: rc, stall: d.l.TransferStall, ceiling: ceiling}, r2)
	})
}

// Live moves a route to the Live class. It must run after Middleware.
func (d *Deadlines) Live(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b, ok := r.Context().Value(budgetKey{}).(*budget); ok {
			b.timer.Stop()
		}
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Time{})
		_ = rc.SetWriteDeadline(time.Time{})
		next.ServeHTTP(w, r)
	})
}

// soonest is the earlier of now+stall and the ceiling.
func soonest(stall time.Duration, ceiling time.Time) time.Time {
	t := time.Now().Add(stall)
	if t.After(ceiling) {
		return ceiling
	}
	return t
}

// stallReader pulls the read deadline in to the stall limit for each body
// read, and puts it back to the ceiling afterwards.
type stallReader struct {
	rc      *http.ResponseController
	body    io.ReadCloser
	stall   time.Duration
	ceiling time.Time
}

func (s *stallReader) Read(p []byte) (int, error) {
	_ = s.rc.SetReadDeadline(soonest(s.stall, s.ceiling))
	n, err := s.body.Read(p)
	if err != nil {
		_ = s.rc.SetReadDeadline(s.ceiling)
	}
	return n, err
}

func (s *stallReader) Close() error { return s.body.Close() }

// stallWriter pulls the write deadline in to the stall limit for each write.
type stallWriter struct {
	http.ResponseWriter
	rc      *http.ResponseController
	stall   time.Duration
	ceiling time.Time
}

func (s *stallWriter) Write(p []byte) (int, error) {
	_ = s.rc.SetWriteDeadline(soonest(s.stall, s.ceiling))
	return s.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the real writer through this one.
func (s *stallWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *stallWriter) Flush() {
	_ = http.NewResponseController(s.ResponseWriter).Flush()
}
