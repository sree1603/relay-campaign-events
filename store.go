package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"sync"
	"time"
)

// Outcome tells the caller what happened to one event in a batch.
type Outcome int

const (
	Accepted  Outcome = iota // new event, now counted
	Duplicate                // same event_id and same facts: a retry, ignored
	Conflict                 // same event_id but different facts: first one wins
)

// campaign holds one campaign's events and its running numbers. Numbers are
// updated when an event is accepted, so reading stats just reads a few stored numbers.
type campaign struct {
	events    []*Event         // in arrival order (Seq increasing)
	total     [4]int           // accepted events per type
	contacts  map[string]uint8 // contact_id -> bit mask of types seen
	unique    [4]int           // distinct contacts per type
	anomalies int              // contacts with opened/clicked but no delivered
	first     time.Time        // earliest event timestamp
	last      time.Time        // latest event timestamp
}

// Store is the whole "database": a map of event_id for dedupe plus the
// per-campaign data. One RWMutex guards everything (see NOTES.md for why).
type Store struct {
	mu        sync.RWMutex
	byID      map[string]*Event
	campaigns map[string]*campaign
	nextSeq   int64
	log       *os.File // append-only file of accepted events, nil = memory only
}

func NewStore() *Store {
	return &Store{byID: map[string]*Event{}, campaigns: map[string]*campaign{}, nextSeq: 1}
}

// OpenStore creates a store backed by an append-only JSONL file. Existing
// lines are replayed through the same code path as live events.
//
// Crash safety: if the process died in the middle of a write, the last line
// has no "\n" at the end. That event was never acknowledged (we answer 200 only
// after the write and fsync finished), so we cut the half line off before
// appending anything new. Otherwise the next event would be glued onto it.
func OpenStore(path string) (*Store, error) {
	s := NewStore()
	if f, err := os.Open(path); err == nil {
		r := bufio.NewReader(f) // no 1 MB line limit like bufio.Scanner has
		var goodEnd int64       // file offset just after the last complete line
		n := 0
		for {
			line, err := r.ReadBytes('\n')
			if err != nil { // io.EOF (maybe with a partial last line) or a real read error
				f.Close()
				if err != io.EOF {
					return nil, err
				}
				if len(line) > 0 {
					log.Printf("replay: dropping half-written last line (%d bytes)", len(line))
					if err := os.Truncate(path, goodEnd); err != nil {
						return nil, err
					}
				}
				break
			}
			goodEnd += int64(len(line))
			var ev Event
			if err := json.Unmarshal(line, &ev); err != nil {
				log.Printf("replay: skipping unreadable line: %v", err)
				continue
			}
			s.apply(&ev)
			n++
		}
		log.Printf("replayed %d events from %s", n, path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	s.log = f
	return s, nil
}

func (s *Store) Close() error {
	if s.log != nil {
		return s.log.Close()
	}
	return nil
}

// Ingest stores a batch. It returns one Outcome per input event, in order.
// The whole batch runs under one lock, so two requests carrying the same
// event_id can never both be counted.
func (s *Store) Ingest(batch []Event) ([]Outcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Outcome, len(batch))
	var fresh []*Event
	pending := map[string]*Event{} // accepted earlier in this same batch

	// Pass 1: decide what is new. Nothing is changed yet.
	for i := range batch {
		ev := batch[i]
		prev, seen := s.byID[ev.EventID]
		if !seen {
			prev, seen = pending[ev.EventID]
		}
		switch {
		case !seen:
			ev.Seq = s.nextSeq + int64(len(fresh))
			p := &ev
			pending[ev.EventID] = p
			fresh = append(fresh, p)
			out[i] = Accepted
		case sameFact(*prev, ev):
			out[i] = Duplicate
		default:
			out[i] = Conflict
		}
	}

	// Write to disk BEFORE changing memory (and before we answer 2xx), so a
	// failed write leaves us unchanged and the provider simply retries.
	if err := s.appendLog(fresh); err != nil {
		return nil, err
	}

	// Pass 2: apply.
	for _, ev := range fresh {
		s.apply(ev)
	}
	return out, nil
}

func (s *Store) appendLog(events []*Event) error {
	if s.log == nil || len(events) == 0 {
		return nil
	}
	w := bufio.NewWriter(s.log)
	enc := json.NewEncoder(w)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	return s.log.Sync()
}

// apply puts one accepted event into memory. Caller holds the write lock
// (or is the single-threaded startup replay).
func (s *Store) apply(ev *Event) {
	s.byID[ev.EventID] = ev
	if ev.Seq >= s.nextSeq {
		s.nextSeq = ev.Seq + 1
	}
	c := s.campaigns[ev.CampaignID]
	if c == nil {
		c = &campaign{contacts: map[string]uint8{}}
		s.campaigns[ev.CampaignID] = c
	}
	c.events = append(c.events, ev)

	t := typeIndex[ev.Type]
	c.total[t]++

	old := c.contacts[ev.ContactID]
	nu := old | 1<<t
	if nu != old {
		c.unique[t]++
		if isAnomaly(old) {
			c.anomalies--
		}
		if isAnomaly(nu) {
			c.anomalies++
		}
		c.contacts[ev.ContactID] = nu
	}

	if c.first.IsZero() || ev.Timestamp.Before(c.first) {
		c.first = ev.Timestamp
	}
	if ev.Timestamp.After(c.last) {
		c.last = ev.Timestamp
	}
}

// isAnomaly: the contact opened or clicked, but we have no delivered for them.
// Usually just a late/out-of-order delivered; worth showing, not hiding.
func isAnomaly(mask uint8) bool {
	return mask&(1<<tDelivered) == 0 && mask&(1<<tOpened|1<<tClicked) != 0
}

// Stats is the dashboard answer for one campaign.
type Stats struct {
	CampaignID     string         `json:"campaign_id"`
	AsOf           time.Time      `json:"as_of"`
	Totals         map[string]int `json:"totals"`          // distinct events per type
	UniqueContacts map[string]int `json:"unique_contacts"` // distinct contacts per type
	Rates          Rates          `json:"rates"`
	FirstEventAt   time.Time      `json:"first_event_at"`
	LastEventAt    time.Time      `json:"last_event_at"`
	DataQuality    DataQuality    `json:"data_quality"`
}

// Rates are based on unique contacts. A rate is null when its denominator is 0.
type Rates struct {
	Delivery *float64 `json:"delivery_rate"` // delivered / sent
	Open     *float64 `json:"open_rate"`     // opened   / delivered
	Click    *float64 `json:"click_rate"`    // clicked  / delivered
}

type DataQuality struct {
	OpenedOrClickedWithoutDelivered int `json:"opened_or_clicked_without_delivered"`
}

func ratio(a, b int) *float64 {
	if b == 0 {
		return nil
	}
	r := float64(a) / float64(b)
	r = float64(int(r*10000+0.5)) / 10000 // 4 decimals
	return &r
}

// Stats returns ok=false when we have never seen an event for the campaign.
func (s *Store) Stats(id string, now time.Time) (Stats, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.campaigns[id]
	if c == nil {
		return Stats{}, false
	}
	names := []string{"sent", "delivered", "opened", "clicked"}
	st := Stats{
		CampaignID:     id,
		AsOf:           now.UTC(),
		Totals:         map[string]int{},
		UniqueContacts: map[string]int{},
		FirstEventAt:   c.first,
		LastEventAt:    c.last,
		DataQuality:    DataQuality{OpenedOrClickedWithoutDelivered: c.anomalies},
	}
	for i, n := range names {
		st.Totals[n] = c.total[i]
		st.UniqueContacts[n] = c.unique[i]
	}
	st.Rates = Rates{
		Delivery: ratio(c.unique[tDelivered], c.unique[tSent]),
		Open:     ratio(c.unique[tOpened], c.unique[tDelivered]),
		Click:    ratio(c.unique[tClicked], c.unique[tDelivered]),
	}
	return st, true
}

// List returns up to limit events, newest arrival first, that arrived before
// the given seq (0 = from the newest). typ may be "" for all types.
// next is the cursor for the following page (0 = no more pages).
func (s *Store) List(id, typ string, before int64, limit int) (events []Event, next int64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.campaigns[id]
	if c == nil {
		return nil, 0, false
	}
	end := len(c.events)
	if before > 0 {
		end = sort.Search(len(c.events), func(i int) bool { return c.events[i].Seq >= before })
	}
	events = []Event{}
	i := end - 1
	for ; i >= 0 && len(events) < limit; i-- {
		if typ == "" || c.events[i].Type == typ {
			events = append(events, *c.events[i])
		}
	}
	if i >= 0 && len(events) > 0 {
		next = events[len(events)-1].Seq
	}
	return events, next, true
}

func (s *Store) String() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fmt.Sprintf("store(events=%d campaigns=%d)", len(s.byID), len(s.campaigns))
}
