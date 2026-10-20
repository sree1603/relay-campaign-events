# BUGS.md — Part 3 (debugging/)

## How I checked
I ran the program, compared its output with `expected_output.txt`, and then read the code for the causes of each difference. The fixes are small (about 16 lines added, 6 removed, the rest of the file is untouched) and each one is marked with a `FIX` comment in `debugging/main.go`.

**Important limit:** I could not run `go run -race` on my machine. The race detector needs cgo and a 64-bit C compiler, and the gcc on my Windows laptop is 32-bit. So I never saw the detector's report. Bug 4 was found by reading the code, and I checked the fix by running the program many times and comparing the output, plus the reasoning below. I would still run `go run -race . events.jsonl` on Linux or WSL before trusting it, and I have not done that.

On Windows I compared the output with PowerShell's `Compare-Object` instead of `diff`, because `>` in PowerShell writes UTF-16, which makes a plain file comparison fail even when the numbers match.

Before the fixes, `cmp_A` printed `sent=2025 ... unique_opens=549`. The correct line is `sent=1987 ... unique_opens=942`.

## Bug 1 — Duplicates were only removed inside one batch

- **What:** Duplicate `event_id`s were still counted when the two copies were far apart in the file.
- **Why:** `processBatch` created `seen := make(map[string]bool)` on every call, so the "already seen" memory was thrown away every 200 lines. A retry that lands in a different batch looks new. The file has 600 duplicate lines, so the numbers were too high (`sent=2025` instead of `1987`).
- **Fix:** Move the map to package level (`seenIDs`) so it lives for the whole file. It is only used on the main goroutine, so it needs no lock.
- **Verified:** `sent` and the other totals now match `expected_output.txt`. I also counted the duplicate lines separately and got 600.

## Bug 2 — `unique_opens` was global, not per campaign

- **What:** A contact who opened in two campaigns was counted only in the first one.
- **Why:** `openedBy` was keyed by `ContactID` alone. The spec says the same contact opening in two campaigns counts once *in each* campaign. 1,098 contacts open in more than one campaign, so the numbers were too low (`549` instead of `942` for `cmp_A`).
- **Fix:** Key the map by `CampaignID + "|" + ContactID`.
- **Verified:** `unique_opens` matches the expected output for every campaign.

## Bug 3 — Daily buckets used the machine's local time zone

- **What:** `delivered` per day moved between days depending on where the program was run.
- **Why:** `ev.Timestamp.Local()` converts to the computer's time zone. The spec says to use the UTC date. On a machine that is already in UTC the bug stays hidden, so it is easy to miss. For a machine in India (UTC+5:30), any delivery after 18:30 UTC lands on the next day. In this file that is 1,381 deliveries.
- **Fix:** `.Local()` becomes `.UTC()`.
- **Verified:** The daily lines match `expected_output.txt`. The fix does not depend on the machine's zone, because `.UTC()` always gives the same result.

## Bug 4 — Data race on the counters

- **What:** `Sent++`, `Delivered++` and the others were run by 8 worker goroutines on the same struct at the same time.
- **Why:** `x++` is read, add, write, which is not atomic. Two goroutines can read the same value, so one update is lost. The comment above `apply` says "the increments themselves are cheap", which is the trap: cheap does not mean safe. The result can change from run to run (requirement 4).
- **Fix:** Add a `sync.Mutex` and lock it in `apply`. I kept the worker pool so the diff stays small.
- **Verified:** I ran the fixed program several times and every run matched `expected_output.txt`. This is weaker evidence than the race detector, because a race can be hidden on any single run, so I am not claiming "no race warnings." I did not run `-race` (see the limit above).
- **Alternative:** I could use `atomic.Int64` counters, or drop the worker pool, because the work per event is trivial. A mutex was the smallest change.

## Final check I would run on Linux or WSL
```bash
cd debugging
for i in 1 2 3 4 5; do go run -race . events.jsonl 2>/dev/null | diff - expected_output.txt && echo "run $i identical"; done
```

On Windows PowerShell, without `-race`:
```powershell
cd debugging
go run . events.jsonl | Out-File -Encoding ascii actual.txt
Compare-Object (Get-Content actual.txt) (Get-Content expected_output.txt)   # no output = identical
```
