package deadline

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

var testLimits = Limits{Quick: 150 * time.Millisecond, TransferTotal: 2 * time.Second, TransferStall: 200 * time.Millisecond}

// serve runs h behind the middlewares on a real server whose connection
// timeouts come from l, the way main sets them.
func serve(t *testing.T, l Limits, build func(d *Deadlines) http.Handler) *httptest.Server {
	t.Helper()
	d := New(l)
	srv := httptest.NewUnstartedServer(d.Middleware(build(d)))
	srv.Config.ReadTimeout = l.Quick
	srv.Config.WriteTimeout = l.Quick
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func transfer(h http.HandlerFunc) func(d *Deadlines) http.Handler {
	return func(d *Deadlines) http.Handler { return d.Transfer(h) }
}

func TestQuickCancelsContext(t *testing.T) {
	type result struct {
		cause   error
		elapsed time.Duration
	}
	got := make(chan result, 1)
	srv := serve(t, testLimits, func(d *Deadlines) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			got <- result{context.Cause(r.Context()), time.Since(start)}
		})
	})
	resp, err := http.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
	}
	res := <-got
	if !errors.Is(res.cause, ErrTimedOut) {
		t.Fatalf("cause = %v, want ErrTimedOut", res.cause)
	}
	if res.elapsed > 2*time.Second {
		t.Fatalf("cancelled after %v, want about %v", res.elapsed, testLimits.Quick)
	}
}

func TestQuickFastHandlerUnaffected(t *testing.T) {
	srv := serve(t, testLimits, func(d *Deadlines) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") })
	})
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestTransferOutlivesQuick(t *testing.T) {
	srv := serve(t, testLimits, transfer(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		if r.Context().Err() != nil {
			w.WriteHeader(500)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestTransferSlowUploadWithSteadyProgress(t *testing.T) {
	srv := serve(t, testLimits, transfer(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil || r.Context().Err() != nil {
			w.WriteHeader(500)
			return
		}
		_, _ = io.WriteString(w, strings.Repeat("x", len(b)))
	}))
	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < 6; i++ {
			time.Sleep(100 * time.Millisecond) // under the stall limit, 600ms in all
			_, _ = pw.Write([]byte("abcd"))
		}
		pw.Close()
	}()
	resp, err := http.Post(srv.URL, "application/octet-stream", pr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || len(body) != 24 {
		t.Fatalf("status %d, read %d bytes, want 200 and 24", resp.StatusCode, len(body))
	}
}

func TestTransferStallCutsBodyRead(t *testing.T) {
	readErr := make(chan error, 1)
	srv := serve(t, testLimits, transfer(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		readErr <- err
	}))
	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	go func() {
		_, _ = pw.Write([]byte("a"))
		// then nothing, for much longer than the stall limit
	}()
	go func() {
		resp, err := http.Post(srv.URL, "application/octet-stream", pr)
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case err := <-readErr:
		var ne net.Error
		if err == nil || !(errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())) {
			t.Fatalf("read error = %v, want a timeout", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stalled body was never cut")
	}
}

func TestTransferCeiling(t *testing.T) {
	l := testLimits
	l.TransferTotal = 400 * time.Millisecond
	l.TransferStall = 300 * time.Millisecond
	cause := make(chan error, 1)
	srv := serve(t, l, transfer(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
		cause <- context.Cause(r.Context())
	}))
	go func() {
		resp, err := http.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case c := <-cause:
		// The read deadline sits at the same instant as the budget timer, so the
		// server's own cancel (a plain Canceled) can win the race. Either way the
		// handler was cut at the ceiling.
		if !errors.Is(c, ErrTimedOut) && !errors.Is(c, context.Canceled) {
			t.Fatalf("cause = %v, want ErrTimedOut or Canceled", c)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("handler was never cancelled")
	}
}

func TestTransferContextLiveAfterBodyConsumed(t *testing.T) {
	srv := serve(t, testLimits, transfer(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(500)
			return
		}
		time.Sleep(450 * time.Millisecond) // past the stall limit, under the ceiling
		if r.Context().Err() != nil {
			w.WriteHeader(500)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	resp, err := http.Post(srv.URL, "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: context was cancelled after the body was read", resp.StatusCode)
	}
}

func TestLiveHasNoLimit(t *testing.T) {
	srv := serve(t, testLimits, func(d *Deadlines) http.Handler {
		return d.Live(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(400 * time.Millisecond)
			if r.Context().Err() != nil {
				w.WriteHeader(500)
				return
			}
			_, _ = io.WriteString(w, "alive")
		}))
	})
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 || string(body) != "alive" {
		t.Fatalf("status %d body %q err %v", resp.StatusCode, body, err)
	}
}
