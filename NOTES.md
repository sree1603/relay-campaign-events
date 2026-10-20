# NOTES.md

# Part 1: Reading the brief first

## What I think the problem is
Providers (email, SMS, push) send us events about messages: sent, delivered, opened, clicked. We store them and show per-campaign numbers on a dashboard. Relay never delivers anything itself.

The code is easy. The hard part is that the data is messy: providers send the same event many times, events come late and in the wrong order, and some are just broken. The numbers still have to be right.

## Assumptions
- `event_id` is unique across all providers. The payload has no provider field, so I can't do better.
- Same `event_id` + same campaign, contact, type and timestamp = a retry. I ignore metadata when comparing.
- Same `event_id` but different content = the provider made a mistake. I keep the first one, don't count the second, and tell the provider (a "conflict").
- Timestamps are RFC 3339. I store them in UTC and always use event time, not arrival time.
- The dashboard polls every few seconds. It doesn't need live push.
- A campaign exists once I've seen an event for it. Unknown campaign gives 404.
- The four event types are exactly lowercase `sent`, `delivered`, `opened`, `clicked`. I reject anything else (`OPENED`, `spam_report`) instead of guessing.

## Things the brief doesn't say
- **"How many opened?"** Total open events, or different people who opened? Very different numbers.
- **No `message_id`.** If one contact gets two messages in a campaign, I can't tell which message an event belongs to. So I can only count per contact.
- **What is "the same event"?** Only `event_id`? What if it's reused with different content? (The seed file does exactly this.)
- **All-time or a time window?** By event time or arrival time?
- **Is an `opened` before its `delivered` valid?** I treat it as valid but early.
- **How late is too late?** The brief says "hours". I don't reject old events.
- **What should the provider get back for a bad event?** A 4xx means they retry for ever. A 200 means they never find out.
- **How fresh must the dashboard be?** Seconds? Minutes?

## Questions for the PM
1. Is "Opened" total opens or unique people? Can we show both?
2. Can one contact get several messages in one campaign? Can providers add a `message_id`?
3. Is `event_id` unique across providers, or only within one? Can we get a `provider` field?
4. If an `event_id` is reused with different content, which one is right: first or last?
5. Do marketers want all-time numbers, or last 24 hours and graphs?
6. Do we ignore machine opens (e.g. mail apps that pre-load images)?
7. How fresh do the numbers need to be?
8. How fast will traffic grow? That decides whether memory is OK.

## Priorities
1. Count correctly: dedupe by `event_id`, and stay correct when two requests arrive at the same time.
2. One bad event must not break the rest of the batch, and the provider should learn what happened to each event.
3. A stats response a dashboard can use directly.
4. Tests for the things I'd hate to break.
5. The debugging exercise, then the written parts.
6. The optional events list, because it was small.

## Decisions I made
- **Same event = same `event_id`.** Same id and same content is a duplicate (fine, counted as success). Same id with different content is a conflict (first one wins, reported).
- **POST /events answers 200** with `received / accepted / duplicates / conflicts / rejected` and a list of problems (position in the array, event_id, reason). It only returns 400 if the body isn't a JSON array, 413 if it's too big (5 MiB or more than 1000 events), and 500 if saving to disk failed (the provider retries, which is safe because retries are deduped).
  I'm fairly sure about 200-for-bad-events, because a 4xx would just make the provider send the same bad event again and again. But I know some teams would pick 207 or 422, so I'd ask.
- **Decoding:** I decode the array as a list of raw JSON pieces and then each piece on its own. So `"event_id": 123` or just `"hello"` inside the array only rejects that one element.
- **What I reject:** missing `event_id`, `campaign_id` or `contact_id`; unknown type (including wrong case); missing or non-RFC-3339 timestamp; ids longer than 128 chars; a timestamp more than 24 hours in the future. Old timestamps are fine. On the seed file: 235 events in, 210 accepted, 15 retries ignored, 3 conflicts, 7 rejected.
- **"Opened" is both:** `totals.opened` counts different events, `unique_contacts.opened` counts different people (per campaign). The rates use unique people: `open_rate = unique opened / unique delivered`.
- **Out of order is fine.** Every update is just "add this event to a set", so arrival order doesn't change the totals. An `opened` with no `delivered` yet is counted and shown in `data_quality`. It disappears from that count when the `delivered` arrives.
- **Storage:** everything in memory (a map by `event_id` for dedupe, plus the events and counters per campaign), and also an append-only file. I write and `fsync` the file before answering 200, and read it again when the server starts. I chose this because it needs no extra library and I can explain it quickly. Counters are updated when an event is saved, so reading stats just reads a few numbers.
  On restart, if the last line of the file is half-written (crash in the middle of a write), I cut it off. Otherwise the next event would be glued to it. That event was never acknowledged, so the provider will resend it.
- **Concurrency:** one `RWMutex` for the whole store. A batch is saved while holding the write lock, so two requests with the same `event_id` can't both count. Stats reads use the read lock.
  Known weakness: the `fsync` happens while holding that lock, so a stats read can wait behind a disk write. At a few thousand events a day nobody would notice. At high volume it would matter (see Part 4).
- **Events list:** newest *arrival* first, and the cursor is my own arrival number (`before=<seq>`). I didn't use event time because late events would shift the pages while someone is scrolling.

## Tests: what and why
Duplicates in one batch and across batches; conflict keeps the first; total vs unique opens; unique is per campaign; out-of-order arrival; 16 goroutines posting overlapping batches; restart gives the same numbers; a half-written last line after a crash; a very long log line; pagination; a bad event doesn't sink the batch; non-array body is 400; future timestamp rejected; the real seed file, posted twice.

---

# Part 4: Scale memo (100 million events a day)

That's about 1,200 events a second on average, maybe 5 to 10 times that at peak. These are my own reasoning, I haven't load-tested anything.

**What breaks first?** Memory. I keep every event forever, a few hundred bytes each, so it's tens of GB per day, and reading the log at startup would take very long. After that: one lock and one `fsync` per batch means all writers wait in line, and one process means no backup if it dies.

**What I'd change, in order:**
1. Use a real database with a unique key on `event_id` (insert, ignore if it exists). Then the database does the dedupe and not a map in one process. Per-campaign counters become rollup rows (per campaign per hour) updated in the same transaction as the insert.
2. Run several copies of the service behind a load balancer. This only works after step 1, because the state can't live in the process any more.
3. Add a queue between the webhook and the database writer, so a slow database doesn't make us miss the provider's timeout.
4. Cache stats for a second or two and read from the rollups.

**Does the API change?** Mostly no. `POST /events` could answer 202 ("accepted, processing"), and then I can't tell the provider whether each event was a duplicate in the same response. The stats shape stays the same, but `as_of` matters more because numbers lag a bit. The storage design changes for sure.

**A queue?** Yes, it takes bursts and slow databases off our back. New problems: messages can be delivered more than once, the dashboard is behind by the queue lag, bad messages need a dead-letter queue, ordering only exists inside a partition, and someone has to watch the lag.

**Where do duplicates sneak in now?** The queue redelivers after a consumer crashes before saving its position; the producer retries a publish that actually worked; two consumers get the same message after a rebalance; and a provider retry arrives after we dropped old ids from a fast cache. The defence is the same for all: a write keyed on `event_id` that does nothing the second time.

**Keeping counters trustworthy:** raw events are the truth and are never edited, and counters are derived from them. I'd update the rollup in the same transaction as the event insert so they can't disagree, and run a background check that recomputes some campaign-hours from raw events and compares. If a counter is wrong, rebuild it.

**What I'd monitor:** requests per second and error rate for `POST /events`, p99 latency, queue lag and the age of the oldest message, size of the dead-letter queue, duplicate / reject / conflict ratio per provider, how late events are (arrival minus event time), database write latency, and any difference found by the recount.

**What I'd not solve yet:** exactly-once delivery end to end (idempotent writes are enough), multi-region, per-contact timelines, archiving and deletion requests, filtering machine opens, per-provider auth and rate limits, push updates. I also wouldn't choose the final analytics database yet. I'd first load-test Postgres with rollups, and only move to something like ClickHouse if that isn't enough. I don't know where the limit is, and that is how I'd find out.

---

# Part 5: The angry marketer

10:00: Sent 1,000,000 / Delivered 970,000 / Opened 250,000 / Clicked 20,000
10:30: Sent 1,000,000 / Delivered 975,000 / Opened 248,000 / Clicked 20,000

## Possible reasons, before blaming the code
**Why opened could go down:**
1. The meaning changed: total opens at 10:00, unique opens at 10:30 (for example after a deploy).
2. A dedupe fix removed double-counted events, so the old number was too high.
3. The provider took events back, for example it decided some opens were bots or mail pre-loading and sent corrections.
4. A conflict was resolved: an `event_id` that used to count was replaced.
5. A contact unsubscribed or was deleted (privacy request) and their events were removed.
6. The two numbers came from different places: a stale cache, or a server instance that restarted and has less data.
7. The dashboard filter is a moving window (like "last 24h") and old opens dropped out, or there is a time zone or day boundary difference.
8. A backfill or reprocessing job changed the rollups.
9. Data was merged, split or moved between campaigns.
10. A real bug: lost updates from a race (like bug 4 in the debugging task), a counter that goes down somewhere, or data lost after a crash or restore.

**Why delivered went up:**
11. Late delivery events. Providers keep retrying soft bounces for hours, and some send receipts in batches.
12. A delayed provider queue was flushed.

## Is "delivered went up" suspicious?
Not by itself. Sent stayed at 1,000,000, so the sending is finished, and delivered going from 97.0% to 97.5% looks like late receipts. It would be suspicious if delivered went above sent, if it keeps rising long after the provider's retry window, or if it jumps at the same moment opened drops (that smells like a rewrite or reclassification).

## What I'd check first
1. **Where each number came from:** same server, same cache, same code version? Was there a deploy or restart between 10:00 and 10:30? This is cheap and rules out a lot.
2. **Recompute from raw events** for both times (events received up to 10:00 and up to 10:30, deduped by `event_id`) and compare with what the dashboard showed.
   - If the recount matches the dashboard, the code does what it should, and the *data* changed (corrections, a definition change). That's expected behaviour, and I'd explain it to the marketer.
   - If it doesn't match, it's a bug in the counters or the cache.
3. In my design raw events are append-only, so opened can only go up by itself. If it went down, something outside the normal path happened (a different code path, a restart that lost data, or the view). So the drop itself tells me where to look.
4. If it is expected, make the product clearer: show "unique opens" with a "last updated" time and a note that numbers can change when providers send late or corrected events.

---

# What I completed / what I intentionally skipped

**Completed:** `POST /events` (per-event validation, dedupe, conflicts, partial success), `GET /campaigns/{id}/stats`, the optional paginated `GET /campaigns/{id}/events`, an on-disk log that is replayed on start (including crash handling), graceful shutdown, tests, the four bugs in `debugging/` fixed and explained in `BUGS.md`, these notes, and `AI_USAGE.md`.

**Skipped on purpose:**
- A real database (SQLite): not needed at this size, and it adds a dependency.
- A "too old" limit for events: I need the PM's answer on how late is normal. (I did add a loose 24h future limit.)
- Scoping `event_id` per provider: the payload has no provider field.
- Hourly or daily graphs and date filters: not in the brief.
- Log rotation: the file only grows.
- Moving `fsync` out of the lock: simple code over speed for now (see decisions).
- Auth, rate limits, Docker, metrics: the assignment says to skip them.

# If I had another day
- Ask the PM the questions above, especially total vs unique opens, and `message_id`.
- Add hourly numbers to the stats for a graph.
- Try SQLite or Postgres with a unique index on `event_id` and compare it with the current store.
- Do the `fsync` outside the read lock (or write in groups) and measure with a load test of about 1,000 events per second.
- Add a `/metrics` endpoint with counts of accepted, duplicate, rejected and conflict events.
- Run the `-race` tests on a working 64-bit gcc setup (or WSL) on my own machine.
