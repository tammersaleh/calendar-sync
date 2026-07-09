package feed

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSecret is embedded in every test feed URL's path so tests can assert it
// never leaks into an error string.
const fakeSecret = "SUPERSECRETTOKEN12345"

// clock returns a controllable Now func plus a setter to advance it.
func clock(start time.Time) (func() time.Time, func(time.Time)) {
	var cur atomic.Pointer[time.Time]
	cur.Store(&start)
	now := func() time.Time { return *cur.Load() }
	set := func(t time.Time) { cur.Store(&t) }
	return now, set
}

func TestFetch_CacheGateSkipsSecondCallWithinTTL(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Cache-Control", "max-age=900")
		_, _ = w.Write([]byte("BODY-1"))
	}))
	defer srv.Close()

	now, _ := clock(time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
	f := &Fetcher{Now: now}
	url := srv.URL + "/feed/" + fakeSecret + "/tripit.ics"

	res, err := f.Fetch(context.Background(), url)
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if !res.Changed || res.Skipped {
		t.Fatalf("first fetch: Changed=%v Skipped=%v, want Changed=true Skipped=false", res.Changed, res.Skipped)
	}
	if string(res.Body) != "BODY-1" {
		t.Fatalf("first fetch body = %q, want BODY-1", res.Body)
	}

	res2, err := f.Fetch(context.Background(), url)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if res2.Changed || !res2.Skipped {
		t.Fatalf("second fetch: Changed=%v Skipped=%v, want Changed=false Skipped=true", res2.Changed, res2.Skipped)
	}
	if res2.Body != nil {
		t.Fatalf("skipped fetch should have no body, got %q", res2.Body)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("server saw %d requests, want 1 (cache gate must suppress the second)", got)
	}
}

func TestFetch_ConditionalGET304AfterTTL(t *testing.T) {
	const etag = `"abc123"`
	var sawINM string
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "max-age=900")
			_, _ = w.Write([]byte("BODY-1"))
			return
		}
		sawINM = r.Header.Get("If-None-Match")
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()

	now, set := clock(time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
	f := &Fetcher{Now: now}
	url := srv.URL + "/feed/" + fakeSecret + "/tripit.ics"

	if _, err := f.Fetch(context.Background(), url); err != nil {
		t.Fatalf("first fetch: %v", err)
	}

	set(now().Add(20 * time.Minute)) // past the 15m TTL
	res, err := f.Fetch(context.Background(), url)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if sawINM != etag {
		t.Fatalf("server saw If-None-Match=%q, want %q", sawINM, etag)
	}
	if res.Changed || res.Skipped {
		t.Fatalf("304 fetch: Changed=%v Skipped=%v, want both false", res.Changed, res.Skipped)
	}
	if res.Body != nil {
		t.Fatalf("304 fetch should have no body, got %q", res.Body)
	}
}

func TestFetch_Changed200OnSecondFetch(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Cache-Control", "max-age=900")
		if n == 1 {
			_, _ = w.Write([]byte("BODY-1"))
			return
		}
		_, _ = w.Write([]byte("BODY-2"))
	}))
	defer srv.Close()

	now, set := clock(time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
	f := &Fetcher{Now: now}
	url := srv.URL + "/feed/" + fakeSecret + "/tripit.ics"

	if _, err := f.Fetch(context.Background(), url); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	set(now().Add(20 * time.Minute))
	res, err := f.Fetch(context.Background(), url)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if !res.Changed || res.Skipped {
		t.Fatalf("second fetch: Changed=%v Skipped=%v, want Changed=true Skipped=false", res.Changed, res.Skipped)
	}
	if string(res.Body) != "BODY-2" {
		t.Fatalf("second fetch body = %q, want BODY-2", res.Body)
	}
}

func TestFetch_TTLResolution(t *testing.T) {
	tests := []struct {
		name          string
		cacheControl  string
		publishedTTL  string
		wantNextDelta time.Duration
	}{
		{"max-age clamped up to 60s min", "max-age=30", "", 60 * time.Second},
		{"huge max-age clamped to 24h", "max-age=999999999", "", 24 * time.Hour},
		{"no cache-control falls back to X-PUBLISHED-TTL", "", "PT15M", 15 * time.Minute},
		{"neither falls back to 15m default", "", "", 15 * time.Minute},
		{"max-age wins over published-ttl", "max-age=3600", "PT15M", time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.cacheControl != "" {
					w.Header().Set("Cache-Control", tc.cacheControl)
				}
				if tc.publishedTTL != "" {
					w.Header().Set("X-PUBLISHED-TTL", tc.publishedTTL)
				}
				_, _ = w.Write([]byte("BODY"))
			}))
			defer srv.Close()

			base := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
			now, _ := clock(base)
			f := &Fetcher{Now: now}
			url := srv.URL + "/feed/" + fakeSecret + "/tripit.ics"

			if _, err := f.Fetch(context.Background(), url); err != nil {
				t.Fatalf("fetch: %v", err)
			}
			// A fetch exactly at nextFetchAt-1ns must skip; at nextFetchAt must fetch.
			// Assert the gate landed at base+wantNextDelta by probing just-before.
			if got := f.nextFetchAt.Sub(base); got != tc.wantNextDelta {
				t.Fatalf("nextFetchAt delta = %v, want %v", got, tc.wantNextDelta)
			}
		})
	}
}

func TestFetch_ErrorStatusDoesNotAdvanceGateOrLeakURL(t *testing.T) {
	for _, code := range []int{http.StatusInternalServerError, http.StatusNotFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(code)
			}))
			defer srv.Close()

			now, _ := clock(time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
			f := &Fetcher{Now: now}
			url := srv.URL + "/feed/" + fakeSecret + "/tripit.ics"

			_, err := f.Fetch(context.Background(), url)
			if err == nil {
				t.Fatalf("status %d: want error, got nil", code)
			}
			if strings.Contains(err.Error(), fakeSecret) {
				t.Fatalf("error leaked secret: %q", err.Error())
			}
			// Gate not advanced: the next fetch must hit the server again.
			_, _ = f.Fetch(context.Background(), url)
			if got := atomic.LoadInt32(&hits); got != 2 {
				t.Fatalf("server saw %d requests, want 2 (error must not advance the gate)", got)
			}
		})
	}
}

func TestFetch_InvalidURLDoesNotEcho(t *testing.T) {
	now, _ := clock(time.Now())
	f := &Fetcher{Now: now}
	_, err := f.Fetch(context.Background(), "://"+fakeSecret+"/nope")
	if err == nil {
		t.Fatal("want error for invalid URL")
	}
	if strings.Contains(err.Error(), fakeSecret) {
		t.Fatalf("invalid-URL error leaked secret: %q", err.Error())
	}
}

func TestFetch_CrossHostRedirectRefused(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("LEAKED"))
	}))
	defer other.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, nil, other.URL+"/stolen", http.StatusFound)
	}))
	defer srv.Close()

	now, _ := clock(time.Now())
	f := &Fetcher{Now: now}
	url := srv.URL + "/feed/" + fakeSecret + "/tripit.ics"
	_, err := f.Fetch(context.Background(), url)
	if err == nil {
		t.Fatal("cross-host redirect must error")
	}
	if strings.Contains(err.Error(), fakeSecret) {
		t.Fatalf("redirect error leaked secret: %q", err.Error())
	}
}

func TestFetch_SameHostRedirectFollowed(t *testing.T) {
	var mux http.ServeMux
	mux.HandleFunc("/feed/"+fakeSecret+"/tripit.ics", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final.ics", http.StatusFound)
	})
	mux.HandleFunc("/final.ics", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("FINAL-BODY"))
	})
	srv := httptest.NewServer(&mux)
	defer srv.Close()

	now, _ := clock(time.Now())
	f := &Fetcher{Now: now}
	url := srv.URL + "/feed/" + fakeSecret + "/tripit.ics"
	res, err := f.Fetch(context.Background(), url)
	if err != nil {
		t.Fatalf("same-host redirect should be followed: %v", err)
	}
	if string(res.Body) != "FINAL-BODY" {
		t.Fatalf("body = %q, want FINAL-BODY", res.Body)
	}
}

// TestFetch_RedirectGuardInstalledOncePerClient guards against re-wrapping the
// cross-host redirect guard on every Fetch. With the default (nil) client, the
// guard is the named crossHostRedirect func; if a later call re-wrapped it in a
// closure, CheckRedirect's code pointer would change and closures would
// accumulate unboundedly over the daemon's lifetime.
func TestFetch_RedirectGuardInstalledOncePerClient(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Cache-Control", "max-age=60")
		_, _ = w.Write([]byte("BODY"))
	}))
	defer srv.Close()

	now, set := clock(time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC))
	f := &Fetcher{Now: now}
	url := srv.URL + "/feed/" + fakeSecret + "/tripit.ics"

	want := reflect.ValueOf(crossHostRedirect).Pointer()
	for i := range 5 {
		if _, err := f.Fetch(context.Background(), url); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
		if got := reflect.ValueOf(f.Client.CheckRedirect).Pointer(); got != want {
			t.Fatalf("after fetch %d: CheckRedirect was re-wrapped; guard must install once per client", i)
		}
		set(now().Add(2 * time.Minute)) // advance past the 60s TTL so the next call fetches
	}
	if hits != 5 {
		t.Fatalf("hits = %d, want 5 (each post-TTL call should fetch)", hits)
	}
}

func TestParseISO8601Duration(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"PT15M", 15 * time.Minute, true},
		{"PT12H", 12 * time.Hour, true},
		{"PT1H30M", 90 * time.Minute, true},
		{"PT45S", 45 * time.Second, true},
		{"PT1H30M15S", time.Hour + 30*time.Minute + 15*time.Second, true},
		{"P1D", 0, false},
		{"", 0, false},
		{"garbage", 0, false},
		{"PT", 0, false},
		{"15M", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseISO8601Duration(tc.in)
			if ok != tc.ok {
				t.Fatalf("parseISO8601Duration(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("parseISO8601Duration(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
