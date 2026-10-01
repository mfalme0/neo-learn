# Neo Learn Architecture

This document records *why* the system is shaped the way it is. The README
describes what it does; this explains the decisions that are not obvious from
the code and would be easy to undo by accident.

---

## The one decision everything else follows from

**Learner state is an append-only event log. Progress is a projection of that
log.**

Every state change — starting a lesson, answering a question — is an immutable
row in `learning_events`. Nothing ever mutates a row, and nothing updates
progress directly. The `progress` table exists purely so a dashboard read is
one indexed lookup instead of an aggregate over the entire history.

```text
Action (web / SMS / STK)
   ↓  client-generated event_id (UUID)
POST /v1/events
   ↓
INSERT ... ON CONFLICT (event_id) DO NOTHING   ← the idempotency gate
   ↓
progress projection updated (same transaction)
   ↓
outbox row written (same transaction)
```

Three problems from the README dissolve into this one shape:

| Problem | How the log answers it |
| --- | --- |
| A learner texts the same answer twice | Same `event_id`, second insert is a no-op |
| An offline queue replays out of order | Order changes which event *wins*, never correctness |
| SMS and web disagree about state | Both are events; both hit one endpoint |

### Alternatives that were rejected

**Per-field last-write-wins.** Each client sends a timestamp per field, the
server keeps the newest. Simpler to reason about, and it loses legitimate work:
a learner who answers on SMS and then the web would have the web timestamp win
even if the SMS answer was genuinely later. Worse, it cannot express "this
submission already happened" — so the double-texted answer still needs a
separate dedupe mechanism. The log gets both properties from one constraint.

**Optimistic concurrency with manual conflict resolution.** Server rejects stale
writes with a version mismatch; client refetches and retries. This pushes
complexity onto every caller. An SMS gateway cannot refetch and retry — it can
only send a message — so this would have made the fallback path the hard one.
Explicit rejection is honest for a collaborative editor and wrong here.

---

## Idempotency is a schema property, not application logic

The guarantee comes from one `UNIQUE` constraint:

```sql
CONSTRAINT learning_events_event_id_key UNIQUE (event_id)
```

with `ON CONFLICT DO NOTHING` on insert. A duplicate surfaces as pgx's
`ErrNoRows`, not as an error — and the service returns early, leaving the
projection untouched and enqueuing no second notification.

Three consequences worth stating explicitly:

- **The id must be client-generated.** A server-generated id cannot deduplicate
  anything: the client would have no way to recognise its own retry. This is
  why `newEventId()` exists on the client and why the browser uses
  `crypto.randomUUID()` rather than a counter. The constraint is global, so a
  predictable id would let one learner's action be swallowed as another's
  duplicate.
- **A duplicate is not an error, and not a conflict.** It returns 200 with
  `duplicate: true`. A learner who pressed send twice has succeeded at what they
  intended.
- **Idempotency extends to notifications.** A duplicate does not enqueue a
  second outbox row, so it does not produce a second SMS acknowledgement. The
  README's example of a learner re-sending because they saw no confirmation is
  exactly the case this handles.

---

## Ordering: `occurred_at`, not `received_at`

When a question is answered more than once, which answer counts?

The projection ranks a re-answered question by **`occurred_at` — the learner's
clock** — with `id` breaking ties:

```sql
SELECT DISTINCT ON (ev.question_id) ev.question_id, ev.payload ->> 'choice_label'
FROM learning_events ev
...
ORDER BY ev.question_id, ev.occurred_at DESC, ev.id DESC
```

This was originally written against `received_at` and the tests caught it.
Ranking by arrival time lets a stale replayed event overwrite a newer answer:
the offline queue delivers out of order, so the event that arrives last is
frequently not the event that happened last.

`received_at` is still recorded, because the difference between the two is
offline lag and should stay measurable — it is exactly the metric that says
whether the SMS fallback is working.

Both clocks are stored rather than trusting either. Client clocks are wrong;
server clocks hide how long a learner was actually disconnected.

---

## Replay is recomputed, not incremented

`ApplyAnswerToProgress` re-derives totals from the log on every write rather
than doing `marks_awarded = marks_awarded + 1`.

Incrementing would be faster and would be wrong. A learner who answers the
same question twice has two events; incrementing counts two marks for one
question. Recomputing `COUNT(DISTINCT ...)` over the deduped set is
self-correcting, and the event log is small enough per assessment that the cost
is irrelevant.

Correctness comes from the choice the learner actually picked:

```sql
JOIN choices ch ON ch.question_id = ev.question_id AND ch.label = latest.label
```

Not from the question row. A re-answer changes which label applies, and reading
correctness from the question would score the stale answer.

---

## Completion is sticky

`UpsertLessonProgress` will not move a `completed` row back to `in_progress`,
regardless of what arrives:

```sql
status = CASE WHEN progress.status = 'completed' THEN 'completed' ELSE EXCLUDED.status END
```

Without this, an out-of-order replay makes finished work look unfinished. A
learner who completed a lesson offline, then had an earlier `lesson_started`
event replay, would watch their progress drop.

---

## Grading is never client-side

`is_correct` is dropped in the HTTP layer before the assessment payload is
serialised. A test asserts the string `is_correct` does not appear in the
response.

The consequence is that the UI cannot show a result without asking the server.
`POST /v1/events` returns the authoritative verdict, and the assessment page
renders that. An SMS learner has no way to be trusted to grade themselves, and
requiring the web path to grade itself would mean the two transports could
disagree — which is the entire failure mode this project exists to avoid.

---

## Transactions span log, projection, and notification

One transaction in `Service.Ingest` covers all three writes. If the projection
succeeded but the outbox row did not, a learner's work would be recorded and
their acknowledgement never sent — a silent divergence between state and
communication.

`outbox` currently has no reader. It is written now so that adding the SMS
worker in milestone 2 does not require rewriting the ingest path.

---

## Tenancy from the first migration

Every table carries `institution_id`, and no query in `db/queries/` accepts it
from an unauthenticated caller. Retrofitting tenancy means rewriting every
query and backfilling every table, so it is cheaper to carry the column while
there is one institution.

Two rules follow from it:

- **A missing row is a 404, not a 403.** `IsEnrolled` failing closed returns
  "no such course", so a learner cannot probe which course ids exist.
- **Login errors are indistinguishable.** Unknown institution, unknown email,
  and wrong password all return the same message and code. A different message
  would make the endpoint an enumeration oracle for which schools use the
  platform.

---

## Sessions

Opaque tokens in Redis, keyed by SHA-256. The raw token is generated once,
placed in an HttpOnly cookie, and never persisted — a Redis dump does not yield
usable sessions.

Password hashes embed their argon2id parameters:

```text
$argon2id$v=19$m=65536,t=3,p=2$<salt>$<key>
```

Verification uses the *stored* parameters, so raising `ARGON2_MEMORY_KIB` does
not lock out every existing user. A successful login with weak parameters
transparently re-hashes at the current cost.

The cookie is `HttpOnly`, `SameSite=Lax`, `Secure` off only for local http.
There is no token in a header or query string: those get written to access logs
and proxy logs.

---

## What the frontend is responsible for

The web client holds a queue in `localStorage`. Every submission is persisted
**before** the network is attempted, then sent; failures stay queued and retry
with the original `event_id`.

This is only safe because of the server contract. The queue can retry as
aggressively as it likes — a duplicate event is a no-op.

`localStorage` rather than IndexedDB is a deliberate trade: the queue is a short
list of small objects that must be readable *synchronously* at startup. An async
store would mean a render where queued answers briefly appear missing, and a
learner would re-enter work they had already done.

Rejected entries are retained with their reason and surfaced in the UI rather
than dropped. Silently discarding a failed submission is the one behaviour that
would make a learner distrust the offline story.

---

## Testing approach

Integration tests run against real Postgres and Redis, not fakes. The
behaviour under test *is* database behaviour: `ON CONFLICT` semantics,
transaction boundaries, `DISTINCT ON` ordering, constraint enforcement. A mock
would only test the mock.

Tests skip cleanly when the services are unavailable, so `go test ./...` remains
usable without Docker while still running the full suite in CI.

Frontend tests cover the API client, the offline queue, and the accessibility
semantics of shared components — that a progress bar exposes `aria-valuenow`,
that status is carried in text and not only in colour.

---

## Known gaps and deliberate omissions

- **No SMS or STK transport yet.** `outbox` is written but nothing drains it.
  That is milestone 2.
- **No teacher or admin interface.** The schema supports them; the authoring
  queries exist; there are no endpoints. Educators landing in the web app get
  an explicit "not built yet" page rather than an empty dashboard.
- **No rebuild tool for the projection.** Because the log is authoritative,
  `RebuildProgress` is straightforward to add and is deliberately absent — it
  would be untested code pretending to be a safety net.
- **Choice labels are single uppercase letters.** A `CHAR(1)` constraint,
  because the label has to survive being the entire contents of a text message.
  This forecloses more than four choices, deliberately.
- **Lesson bodies are stored as markdown but rendered as plain text.** No
  sanitiser exists yet, so the client escapes by construction rather than
  trusting server-supplied HTML. A markdown renderer with raw HTML disabled can
  replace this.