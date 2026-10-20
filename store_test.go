package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)

func ev(id, camp, contact, typ string, minutes int) Event {
	return Event{EventID: id, CampaignID: camp, ContactID: contact, Type: typ, Timestamp: t0.Add(time.Duration(minutes) * time.Minute)}
}

func mustStats(t *testing.T, s *Store, camp string) Stats {
	t.Helper()
	st, ok := s.Stats(camp, t0)
	if !ok {
		t.Fatalf("no stats for %s", camp)
	}
	return st
}

// The retry case: same event many times must be counted once.
func TestDuplicateIsCountedOnce(t *testing.T) {
	s := NewStore()
	e := ev("e1", "c1", "ct1", "delivered", 0)
	out, _ := s.Ingest([]Event{e, e, e}) // same batch
	out2, _ := s.Ingest([]Event{e})      // later batch
	if out[0] != Accepted || out[1] != Duplicate || out[2] != Duplicate || out2[0] != Duplicate {
		t.Fatalf("unexpected outcomes %v %v", out, out2)
	}
	if got := mustStats(t, s, "c1").Totals["delivered"]; got != 1 {
		t.Fatalf("delivered = %d, want 1", got)
	}
}

// Same event_id but different facts: first wins and it is reported as a conflict.
func TestConflictKeepsFirst(t *testing.T) {
	s := NewStore()
	out, _ := s.Ingest([]Event{ev("e1", "c1", "ct1", "sent", 0), ev("e1", "c1", "ct1", "clicked", 0)})
	if out[1] != Conflict {
		t.Fatalf("want Conflict, got %v", out[1])
	}
	st := mustStats(t, s, "c1")
	if st.Totals["sent"] != 1 || st.Totals["clicked"] != 0 {
		t.Fatalf("first event should win, got %+v", st.Totals)
	}
}

// "Opened" means two things: raw opens and distinct people who opened.
func TestOpenedTotalVsUnique(t *testing.T) {
	s := NewStore()
	s.Ingest([]Event{
		ev("e1", "c1", "ct1", "opened", 1),
		ev("e2", "c1", "ct1", "opened", 2), // same person opens again
		ev("e3", "c1", "ct2", "opened", 3),
	})
	st := mustStats(t, s, "c1")
	if st.Totals["opened"] != 3 || st.UniqueContacts["opened"] != 2 {
		t.Fatalf("totals=%v unique=%v", st.Totals, st.UniqueContacts)
	}
}

// The same contact in two campaigns is counted once in each (the same trap
// as bug 2 in debugging/).
func TestUniqueIsPerCampaign(t *testing.T) {
	s := NewStore()
	s.Ingest([]Event{ev("e1", "c1", "ct1", "opened", 1), ev("e2", "c2", "ct1", "opened", 1)})
	if mustStats(t, s, "c1").UniqueContacts["opened"] != 1 || mustStats(t, s, "c2").UniqueContacts["opened"] != 1 {
		t.Fatal("contact should count once per campaign")
	}
}

// Arrival order must not change the numbers; an early "opened" is flagged
// until its "delivered" shows up, then the flag clears.
func TestOutOfOrderArrival(t *testing.T) {
	s := NewStore()
	s.Ingest([]Event{ev("e2", "c1", "ct1", "opened", 5)})
	if got := mustStats(t, s, "c1").DataQuality.OpenedOrClickedWithoutDelivered; got != 1 {
		t.Fatalf("anomaly = %d, want 1", got)
	}
	s.Ingest([]Event{ev("e1", "c1", "ct1", "delivered", 1)})
	st := mustStats(t, s, "c1")
	if st.DataQuality.OpenedOrClickedWithoutDelivered != 0 || st.Totals["opened"] != 1 || st.Totals["delivered"] != 1 {
		t.Fatalf("after late delivered: %+v", st)
	}
	if !st.FirstEventAt.Equal(t0.Add(time.Minute)) || !st.LastEventAt.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("first/last use event time, got %v %v", st.FirstEventAt, st.LastEventAt)
	}
}

// Two requests at once with overlapping events: every event_id counted once.
// Run with -race.
func TestConcurrentIngest(t *testing.T) {
	s := NewStore()
	var all []Event
	for i := 0; i < 200; i++ {
		all = append(all, ev(fmt.Sprintf("e%d", i), "c1", fmt.Sprintf("ct%d", i%50), "delivered", i))
	}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Ingest(all)
			s.Stats("c1", t0) // readers at the same time
		}()
	}
	wg.Wait()
	st := mustStats(t, s, "c1")
	if st.Totals["delivered"] != 200 || st.UniqueContacts["delivered"] != 50 {
		t.Fatalf("got %+v %+v", st.Totals, st.UniqueContacts)
	}
}

// Restart: the log replay must give back exactly the same numbers.
func TestReplayAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	s1, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s1.Ingest([]Event{ev("e1", "c1", "ct1", "sent", 0), ev("e2", "c1", "ct1", "opened", 3), ev("e1", "c1", "ct1", "sent", 0)})
	before := mustStats(t, s1, "c1")
	s1.Close()

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if after := mustStats(t, s2, "c1"); !reflect.DeepEqual(before, after) {
		t.Fatalf("stats changed after restart:\n%+v\n%+v", before, after)
	}
	// and the retry of an old event after restart is still a duplicate
	if out, _ := s2.Ingest([]Event{ev("e2", "c1", "ct1", "opened", 3)}); out[0] != Duplicate {
		t.Fatalf("want Duplicate after restart, got %v", out[0])
	}
}

// Paging walks every event exactly once, newest arrival first.
func TestListPagination(t *testing.T) {
	s := NewStore()
	var batch []Event
	for i := 0; i < 7; i++ {
		batch = append(batch, ev(fmt.Sprintf("e%d", i), "c1", "ct1", "sent", i))
	}
	s.Ingest(batch)
	var seen []string
	var cursor int64
	for pages := 0; pages < 10; pages++ {
		evs, next, _ := s.List("c1", "", cursor, 3)
		for _, e := range evs {
			seen = append(seen, e.EventID)
		}
		if next == 0 {
			break
		}
		cursor = next
	}
	want := []string{"e6", "e5", "e4", "e3", "e2", "e1", "e0"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("got %v want %v", seen, want)
	}
}

// Crash in the middle of a write: the last line is only half there. After
// restart the half line must be dropped, and the next event must NOT be glued
// onto it (that would corrupt a good event).
func TestReplayWithHalfWrittenLastLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	s1, _ := OpenStore(path)
	s1.Ingest([]Event{ev("e1", "c1", "ct1", "sent", 0)})
	s1.Close()

	// simulate the crash: a partial JSON line with no newline
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(`{"event_id":"e2","campaign_id":"c1","contact_id":"ct`)
	f.Close()

	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s2.Ingest([]Event{ev("e3", "c1", "ct2", "sent", 5)}) // first write after the crash
	s2.Close()

	s3, err := OpenStore(path) // restart again: e3 must have survived
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close()
	if got := mustStats(t, s3, "c1").Totals["sent"]; got != 2 { // e1 and e3
		t.Fatalf("sent = %d, want 2 (e1 and e3)", got)
	}
}

// A line longer than 1 MB must not silently stop the replay.
func TestReplayWithVeryLongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	s1, _ := OpenStore(path)
	big := ev("e1", "c1", "ct1", "sent", 0)
	big.Metadata = map[string]any{"blob": string(make([]byte, 2<<20))}
	s1.Ingest([]Event{big, ev("e2", "c1", "ct2", "sent", 1)})
	s1.Close()

	s2, _ := OpenStore(path)
	defer s2.Close()
	if got := mustStats(t, s2, "c1").Totals["sent"]; got != 2 {
		t.Fatalf("sent = %d, want 2", got)
	}
}
