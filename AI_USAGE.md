# AI_USAGE.md

## Tools
Claude (chat). That is the only AI tool I used. I also used Google and the Go docs when I wanted to check a standard-library detail (for example how `http.MaxBytesReader` and `bufio` behave).

## How I worked with it
I used Claude like a senior colleague I could ask questions, not like someone who does the assignment for me. The order was always the same: I read the brief and the seed file myself first, wrote down what I thought the problem was, and only then talked to Claude about it.

- **Understanding the problem.** I read the brief and the seed file on my own and wrote down my first reading of the problem. Then I asked Claude things like "what does 'same event' really mean here?" and "what should we do if the same `event_id` shows up with different content?" The answers helped me notice cases I had missed. The decisions in `NOTES.md` (first one wins and report a conflict, answer 200 with a per-event report, count both total opens and unique people) are decisions I made after going back and forth. I can explain every one of them without looking at the notes.
- **Code.** Claude wrote a first draft of the service and the tests. After that I read it line by line, ran it, and asked about everything I did not follow until I could explain it myself: why `Ingest` has two passes, why the log is written before memory is changed, why unique opens use one bit per type per contact, and why the cursor uses arrival order instead of offsets. I changed parts that I wanted different, and I later found and fixed real weaknesses in it (see below).
- **Debugging task.** I looked at the output differences myself first and Claude helped me confirm the reasons. I could not run `go run -race` on my machine (details in `BUGS.md`), so bug 4 was found by reading the code and the comment above `apply`, not from the race detector.
- **Written parts.** I used Claude to help me structure the notes and to challenge my answers (for example, "what else could make Opened go down?"). I then reworked the text so it says what I actually think. Parts 4 and 5 are the places where I did the most rewriting.

## One suggestion I changed
There were two real choices I had to make:

1. **Storage.** SQLite needs a third-party driver, and pure in-memory loses everything on restart. I chose an in-memory store plus an append-only log file that is replayed on startup. It has no dependencies and I can explain it in a minute. `NOTES.md` says what I would use at 100M events a day.
2. **`fsync` while holding the lock.** I observed that `Ingest` calls `fsync` while it holds the write lock, so a stats request can wait behind a disk write. The suggested fix was to restructure the write path. I decided not to. At a few thousand events a day it does not matter, and a restructure would make the code harder to reason about for a small gain. Instead I wrote the trade-off down openly in `NOTES.md` and put it under "If I had another day." I also removed my earlier claim that stats are "O(1)", because that was more than I could really promise. Now it only says stats read a few stored numbers.

## Something AI helped me understand
That "how many opened?" has two honest answers (total open events and distinct people who opened), and that mixing them up is exactly how a dashboard number can go down. I had not thought of it before and I now use it in the angry-marketer answer. It also made me see that without a `message_id` I cannot tell two messages to the same contact apart.

## Something I had to debug or fix after the first draft
The first draft worked on the happy path, but when I reviewed it for failure cases I found real weaknesses, and I fixed them myself:

- **Crash replay bug.** If the service crashed halfway through writing a line, the next event was glued onto that broken line when it restarted. On startup I now cut off a half-written last line. I added a test that fails without the fix and passes with it.
- **Long log lines.** The line scanner had a 1 MB limit and silently stopped the replay when it hit a longer line. I replaced it with a reader that has no line length limit, and added a test.
- **Shutdown.** `log.Fatal` meant `defer store.Close()` never ran. Ctrl+C now shuts the server down cleanly and closes the log. I also added a `WriteTimeout`.
- **Time check.** The service accepted any timestamp. Events more than 24 hours in the future are now rejected, with a test. Old events are still accepted, because late events are normal.

I also had two small Windows problems. `>` in PowerShell writes UTF-16, so my first output diff against `expected_output.txt` looked wrong even though the numbers matched (I used `Compare-Object` after that). And `-race` does not work on my machine because of my 32-bit gcc.

## What is mine and what is not
Claude did a lot of the typing. I did the reading, the decisions, the questions, the review, the fixes after review, and the checks I could run. If the follow-up interview asks me to walk through any file, I can.
