# Campaign events service (Go, stdlib only)

Receives delivery events from providers (`sent`, `delivered`, `opened`, `clicked`) and serves per-campaign stats for a dashboard.
See **NOTES.md** for decisions, scale memo and the "angry marketer" answer, **BUGS.md** for the debugging task, **AI_USAGE.md** for AI disclosure.

## Requirements
Go 1.22 or newer (`go version`). No third-party dependencies.

## Run
```bash
go run .                       # listens on :8080, keeps events in data/events.jsonl
go run . -addr :9090           # different port
go run . -data ""              # memory only, nothing written to disk
```
Events are written to `data/events.jsonl` before the service answers 200, and replayed when it starts (a half-written last line from a crash is cut off). Ctrl+C stops it cleanly. Delete `data/` to start clean.

Windows PowerShell: use `curl.exe` (plain `curl` is a different command), e.g. `curl.exe -s -X POST localhost:8080/events -H "Content-Type: application/json" --data-binary "@seed/events.json"`.

## Try it (second terminal)
```bash
# load the one-day seed file (235 events: duplicates, conflicts and bad records included)
curl -s -X POST localhost:8080/events -H 'Content-Type: application/json' --data-binary @seed/events.json

# stats for a campaign
curl -s localhost:8080/campaigns/cmp_summer_sale/stats

# recent activity, newest first (optional: &type=opened, &limit=20)
curl -s "localhost:8080/campaigns/cmp_summer_sale/events?limit=5"
# next page: use "next_before" from the previous response
curl -s "localhost:8080/campaigns/cmp_summer_sale/events?limit=5&before=<next_before>"

# one good + one bad event in the same batch: the good one is still saved
curl -s -X POST localhost:8080/events -H 'Content-Type: application/json' -d '[
  {"event_id":"evt_x1","campaign_id":"cmp_demo","contact_id":"ct_1","type":"sent","timestamp":"2026-08-10T09:00:00Z"},
  {"event_id":"evt_x2","campaign_id":"cmp_demo","contact_id":"ct_1","type":"opened","timestamp":"yesterday"}
]'
# send the same batch again: duplicates are ignored, numbers don't change
```
Seed result: `received 235, accepted 210, duplicates 15, conflicts 3, rejected 7`.

## API in one minute
| Endpoint | Meaning |
|---|---|
| `POST /events` | JSON array of events. **200** + per-event report (`accepted`, `duplicates`, `conflicts`, `rejected`, `issues[]`); **400** not an array; **413** too big (5 MiB / 1000 events); **500** couldn't save, retry is safe |
| `GET /campaigns/{id}/stats` | `totals` (distinct events per type), `unique_contacts`, `rates` (null if no denominator), `first_event_at`, `last_event_at`, `as_of`, `data_quality`. **404** if the campaign has no events |
| `GET /campaigns/{id}/events` | `limit` (1–200, default 50), `before` (cursor), `type` filter. Newest arrival first |
| `GET /healthz` | liveness |

## Tests
```bash
go test -race -count=1 ./...        # service tests (include the seed file and a concurrency test)
go test -race -count=1 -v ./...     # same, with test names
go vet ./...
# Windows without a 64-bit gcc: -race won't build, so leave it out
go test -count=1 ./...
```

## Debugging exercise (Part 3)
```bash
cd debugging
go run -race . events.jsonl > actual.txt
diff actual.txt expected_output.txt && echo "identical"

# PowerShell (no diff, and ">" saves UTF-16, so compare directly):
Compare-Object (go run . events.jsonl) (Get-Content expected_output.txt)   # prints nothing = identical
```

## Layout
```
main.go        flags + server start
event.go       event types, validation, "what is the same event"
store.go       in-memory store, counters, append-only log, replay
handlers.go    HTTP handlers and responses
*_test.go      tests
seed/          provider traffic from the assignment
debugging/     the buggy program, fixed
```
