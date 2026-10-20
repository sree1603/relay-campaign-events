package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"
)

const (
	maxBodyBytes = 5 << 20 // 5 MiB
	maxBatch     = 1000    // events per request
	defaultLimit = 50
	maxLimit     = 200
)

type Server struct {
	store *Store
	now   func() time.Time // injectable so tests don't depend on the clock
}

func NewServer(s *Store) *Server { return &Server{store: s, now: time.Now} }

func (srv *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /events", srv.handlePostEvents)
	mux.HandleFunc("GET /campaigns/{campaignID}/stats", srv.handleGetStats)
	mux.HandleFunc("GET /campaigns/{campaignID}/events", srv.handleListEvents)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	return mux
}

// Issue describes one event in a batch that was not counted as new.
type Issue struct {
	Index   int    `json:"index"`
	EventID string `json:"event_id,omitempty"`
	Status  string `json:"status"` // "rejected" or "conflict"
	Reason  string `json:"reason"`
}

type IngestResult struct {
	Received   int     `json:"received"`
	Accepted   int     `json:"accepted"`
	Duplicates int     `json:"duplicates"`
	Conflicts  int     `json:"conflicts"`
	Rejected   int     `json:"rejected"`
	Issues     []Issue `json:"issues"`
}

// handlePostEvents: the provider sends a JSON array. We decode it element by
// element, so one broken event never takes the other 199 down with it.
//
// Status codes:
//
//	200 - the batch was understood. Look at the body for per-event results.
//	      Bad events inside it do NOT cause a 4xx: the provider would just
//	      retry the same bad event forever. Good events are already saved.
//	400 - body is not a JSON array at all (retrying will not help either).
//	413 - body or batch too large.
//	500 - we could not save (disk error). Provider should retry; safe, because
//	      retries are de-duplicated.
func (srv *Server) handlePostEvents(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "body larger than 5 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "could not read body")
		return
	}

	var raws []json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(body), &raws); err != nil {
		writeError(w, http.StatusBadRequest, "body must be a JSON array of events")
		return
	}
	if len(raws) > maxBatch {
		writeError(w, http.StatusRequestEntityTooLarge, "at most 1000 events per request")
		return
	}

	res := IngestResult{Received: len(raws), Issues: []Issue{}}
	var good []Event
	var goodIdx []int // position of each good event in the original array
	for i, raw := range raws {
		var in incomingEvent
		if err := json.Unmarshal(raw, &in); err != nil {
			res.Rejected++
			res.Issues = append(res.Issues, Issue{Index: i, Status: "rejected", Reason: "not a valid event object: " + err.Error()})
			continue
		}
		ev, err := validate(in, srv.now())
		if err != nil {
			res.Rejected++
			res.Issues = append(res.Issues, Issue{Index: i, EventID: in.EventID, Status: "rejected", Reason: err.Error()})
			continue
		}
		good = append(good, ev)
		goodIdx = append(goodIdx, i)
	}

	outcomes, err := srv.store.Ingest(good)
	if err != nil {
		log.Printf("ingest failed: %v", err)
		writeError(w, http.StatusInternalServerError, "could not save events, please retry")
		return
	}
	for j, o := range outcomes {
		switch o {
		case Accepted:
			res.Accepted++
		case Duplicate:
			res.Duplicates++
		case Conflict:
			res.Conflicts++
			res.Issues = append(res.Issues, Issue{
				Index: goodIdx[j], EventID: good[j].EventID, Status: "conflict",
				Reason: "event_id already stored with different campaign/contact/type/timestamp; first one kept",
			})
		}
	}
	log.Printf("POST /events received=%d accepted=%d dup=%d conflict=%d rejected=%d",
		res.Received, res.Accepted, res.Duplicates, res.Conflicts, res.Rejected)
	writeJSON(w, http.StatusOK, res)
}

func (srv *Server) handleGetStats(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("campaignID")
	st, ok := srv.store.Stats(id, srv.now())
	if !ok {
		writeError(w, http.StatusNotFound, "no events seen for campaign "+id)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// handleListEvents: ?limit=50&before=<seq>&type=opened
// Newest arrival first. "before" is the cursor returned as next_before.
func (srv *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("campaignID")
	q := r.URL.Query()

	limit := defaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			writeError(w, http.StatusBadRequest, "limit must be a number from 1 to 200")
			return
		}
		limit = n
	}
	var before int64
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "before must be a positive number")
			return
		}
		before = n
	}
	typ := q.Get("type")
	if typ != "" {
		if _, ok := typeIndex[typ]; !ok {
			writeError(w, http.StatusBadRequest, "unknown type filter")
			return
		}
	}

	events, next, ok := srv.store.List(id, typ, before, limit)
	if !ok {
		writeError(w, http.StatusNotFound, "no events seen for campaign "+id)
		return
	}
	resp := map[string]any{"campaign_id": id, "events": events}
	if next > 0 {
		resp["next_before"] = next
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Printf("writeJSON: %v", err)
	}
}
