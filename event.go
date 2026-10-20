package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The four event types the providers can send. The index is used to keep
// per-type counters in small fixed-size arrays.
var typeIndex = map[string]int{"sent": 0, "delivered": 1, "opened": 2, "clicked": 3}

const (
	tSent = iota
	tDelivered
	tOpened
	tClicked
)

const (
	maxIDLen  = 128
	maxFuture = 24 * time.Hour // events further in the future than this are rejected
)

// Event is the cleaned-up event we store and return.
// Timestamp is always UTC. Seq is our own arrival counter (see NOTES.md).
type Event struct {
	EventID    string         `json:"event_id"`
	CampaignID string         `json:"campaign_id"`
	ContactID  string         `json:"contact_id"`
	Type       string         `json:"type"`
	Timestamp  time.Time      `json:"timestamp"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Seq        int64          `json:"seq"`
}

// incomingEvent is what a provider sends. Everything is decoded loosely so
// that one weird element only fails itself, never the rest of the batch.
type incomingEvent struct {
	EventID    string         `json:"event_id"`
	CampaignID string         `json:"campaign_id"`
	ContactID  string         `json:"contact_id"`
	Type       string         `json:"type"`
	Timestamp  string         `json:"timestamp"`
	Metadata   map[string]any `json:"metadata"`
}

// validate turns an incoming event into a clean Event, or explains why not.
// Rules are deliberately strict: the payload is a fixed contract, so we
// reject instead of guessing (e.g. "OPENED" is rejected, not lower-cased).
func validate(in incomingEvent, now time.Time) (Event, error) {
	ev := Event{
		EventID:    strings.TrimSpace(in.EventID),
		CampaignID: strings.TrimSpace(in.CampaignID),
		ContactID:  strings.TrimSpace(in.ContactID),
		Type:       in.Type,
		Metadata:   in.Metadata,
	}
	for name, v := range map[string]string{
		"event_id": ev.EventID, "campaign_id": ev.CampaignID, "contact_id": ev.ContactID,
	} {
		if v == "" {
			return Event{}, fmt.Errorf("%s is required", name)
		}
		if len(v) > maxIDLen {
			return Event{}, fmt.Errorf("%s is longer than %d characters", name, maxIDLen)
		}
	}
	if _, ok := typeIndex[ev.Type]; !ok {
		return Event{}, fmt.Errorf("type %q is not one of sent, delivered, opened, clicked", ev.Type)
	}
	if in.Timestamp == "" {
		return Event{}, errors.New("timestamp is required")
	}
	ts, err := time.Parse(time.RFC3339, in.Timestamp)
	if err != nil {
		return Event{}, fmt.Errorf("timestamp %q is not RFC 3339 (e.g. 2026-08-10T09:15:04Z)", in.Timestamp)
	}
	if ts.After(now.Add(maxFuture)) {
		return Event{}, fmt.Errorf("timestamp %q is more than 24h in the future", in.Timestamp)
	}
	ev.Timestamp = ts.UTC()
	return ev, nil
}

// sameFact says whether two events with the same event_id describe the same
// thing. Metadata is ignored on purpose: a retry may carry different extras.
func sameFact(a, b Event) bool {
	return a.CampaignID == b.CampaignID && a.ContactID == b.ContactID &&
		a.Type == b.Type && a.Timestamp.Equal(b.Timestamp)
}
