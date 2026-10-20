package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func newTestServer() (*Server, http.Handler) {
	srv := NewServer(NewStore())
	srv.now = func() time.Time { return time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC) }
	return srv, srv.Routes()
}

func post(h http.Handler, body string) (int, IngestResult) {
	req := httptest.NewRequest("POST", "/events", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var res IngestResult
	json.Unmarshal(rec.Body.Bytes(), &res)
	return rec.Code, res
}

func get(h http.Handler, url string, into any) int {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
	if into != nil {
		json.Unmarshal(rec.Body.Bytes(), into)
	}
	return rec.Code
}

// The "other 199 events" case: one broken element must not hurt the rest.
func TestBadEventDoesNotSinkTheBatch(t *testing.T) {
	_, h := newTestServer()
	body := `[
	 {"event_id":"a","campaign_id":"c1","contact_id":"x","type":"sent","timestamp":"2026-08-10T09:00:00Z"},
	 {"event_id":"b","campaign_id":"c1","contact_id":"x","type":"delivered","timestamp":"not-a-time"},
	 {"event_id":123,"campaign_id":"c1","contact_id":"x","type":"sent","timestamp":"2026-08-10T09:00:00Z"},
	 "just a string",
	 {"event_id":"c","campaign_id":"c1","contact_id":"x","type":"OPENED","timestamp":"2026-08-10T09:00:00Z"},
	 {"event_id":"d","campaign_id":"c1","contact_id":"x","type":"delivered","timestamp":"2026-08-10T09:01:00Z"}
	]`
	code, res := post(h, body)
	if code != 200 || res.Received != 6 || res.Accepted != 2 || res.Rejected != 4 {
		t.Fatalf("code=%d res=%+v", code, res)
	}
	var st Stats
	get(h, "/campaigns/c1/stats", &st)
	if st.Totals["sent"] != 1 || st.Totals["delivered"] != 1 {
		t.Fatalf("stats %+v", st.Totals)
	}
}

func TestNotAnArrayIs400(t *testing.T) {
	_, h := newTestServer()
	for _, b := range []string{`{"event_id":"a"}`, `not json`, ``} {
		if code, _ := post(h, b); code != 400 {
			t.Fatalf("body %q: code %d, want 400", b, code)
		}
	}
}

func TestUnknownCampaignIs404(t *testing.T) {
	_, h := newTestServer()
	if code := get(h, "/campaigns/nope/stats", nil); code != 404 {
		t.Fatalf("code %d", code)
	}
}

func TestRatesAreNullWithoutDenominator(t *testing.T) {
	_, h := newTestServer()
	post(h, `[{"event_id":"a","campaign_id":"c1","contact_id":"x","type":"opened","timestamp":"2026-08-10T09:00:00Z"}]`)
	var st Stats
	get(h, "/campaigns/c1/stats", &st)
	if st.Rates.Delivery != nil || st.Rates.Open != nil {
		t.Fatalf("rates should be null, got %+v", st.Rates)
	}
}

// End-to-end with the real seed file from the assignment.
func TestSeedFile(t *testing.T) {
	seed, err := os.ReadFile("seed/events.json")
	if err != nil {
		t.Skip("seed file not found")
	}
	_, h := newTestServer()
	code, res := post(h, string(seed))
	if code != 200 {
		t.Fatalf("code %d", code)
	}
	// 235 in the file = 210 new + 15 identical retries + 3 conflicts + 7 invalid
	if res.Received != 235 || res.Accepted != 210 || res.Duplicates != 15 || res.Conflicts != 3 || res.Rejected != 7 {
		t.Fatalf("unexpected result %+v", res)
	}
	var before Stats
	get(h, "/campaigns/cmp_summer_sale/stats", &before)

	// Provider replays the whole file: nothing may change.
	_, again := post(h, string(seed))
	if again.Accepted != 0 {
		t.Fatalf("replay accepted %d events", again.Accepted)
	}
	var after Stats
	get(h, "/campaigns/cmp_summer_sale/stats", &after)
	b1, _ := json.Marshal(before)
	b2, _ := json.Marshal(after)
	if string(b1) != string(b2) {
		t.Fatalf("stats changed after replay:\n%s\n%s", b1, b2)
	}
}

func TestListEventsEndpoint(t *testing.T) {
	_, h := newTestServer()
	post(h, `[
	 {"event_id":"a","campaign_id":"c1","contact_id":"x","type":"sent","timestamp":"2026-08-10T09:00:00Z"},
	 {"event_id":"b","campaign_id":"c1","contact_id":"x","type":"opened","timestamp":"2026-08-10T09:05:00Z"}]`)
	var resp struct {
		Events []Event `json:"events"`
	}
	if code := get(h, "/campaigns/c1/events?type=opened", &resp); code != 200 || len(resp.Events) != 1 || resp.Events[0].EventID != "b" {
		t.Fatalf("code=%d resp=%+v", code, resp)
	}
	if code := get(h, "/campaigns/c1/events?limit=0", nil); code != 400 {
		t.Fatalf("limit=0 should be 400, got %d", code)
	}
}

// Loose sanity check: a clock in the far future is rejected, old events are not.
func TestFutureTimestampRejected(t *testing.T) {
	_, h := newTestServer() // fake "now" is 2026-08-11
	code, res := post(h, `[
	 {"event_id":"a","campaign_id":"c1","contact_id":"x","type":"sent","timestamp":"2099-01-01T00:00:00Z"},
	 {"event_id":"b","campaign_id":"c1","contact_id":"x","type":"sent","timestamp":"2020-01-01T00:00:00Z"}]`)
	if code != 200 || res.Accepted != 1 || res.Rejected != 1 {
		t.Fatalf("code=%d res=%+v", code, res)
	}
}
