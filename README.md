# Neo Learn

**An offline-capable learning platform built around SMS and SIM Toolkit infrastructure.**

Neo Learn is an experimental EdTech platform. It provides a conventional, modern
learning experience through a web or mobile interface while retaining **SMS / SIM
Toolkit as a fallback communication layer**. A learner can work through lessons,
assessments, and progress in a proper graphical interface when connected, and the
same learning session continues through SMS when connectivity is unavailable.

The goal is *not* to build another SMS learning service. The goal is to explore
what happens when **SMS becomes another transport layer for a modern learning
platform**.

---

## The Problem

A traditional online learning platform assumes connectivity:

```text
Student → Web/Mobile App → Internet → Learning Platform
```

When the internet disappears, the learning experience stops.

Text-message-based learning survives disconnection, but introduces its own
problem: a purely conversational interface is cumbersome for richer course
structures, progress tracking, assessments, and larger amounts of content.

Neo Learn explores the middle ground:

```text
                    ┌── Web / Mobile UI
                    │
Student ────────────┤
                    │
                    └── SMS / STK
                           ↓
                    Learning Platform
```

The interface changes depending on what connectivity is available. The underlying
learning state does not.

---

## Core Idea

Neo Learn treats **SMS and SIM Toolkit as transport mechanisms rather than the
application itself**. Course progress, assessment state, submissions, and account
information live centrally in the platform. The learner then reaches that state
through different interfaces.

### Connected

- Course dashboards
- Lesson navigation
- Rich learning content
- Interactive quizzes
- Progress tracking
- Assessment history
- Notifications
- Teacher communication

### Offline / Low Connectivity

- SMS lessons and SMS questions
- Multiple-choice assessments
- Answer submission
- Progress notifications
- Reminders
- Basic course navigation
- SIM Toolkit interactions where supported

These are **not separate learning systems**. Both operate against the same learner
and course state.

---

## Architecture

```text
                         ┌──────────────────┐
                         │   Web / Mobile   │
                         │       UI         │
                         └────────┬─────────┘
                                  │
                         ┌────────▼─────────┐
                         │   Neo Learn API  │
                         └────────┬─────────┘
                                  │
                   ┌──────────────┼──────────────┐
                   │              │              │
             ┌─────▼─────┐  ┌────▼─────┐  ┌────▼─────┐
             │ Learning  │  │Assessment│  │ Progress |
             │  Engine   │  │  Engine  │  │  Engine  │
             └─────┬─────┘  └────┬─────┘  └────┬─────┘
                   │              │              │
                   └──────────────┼──────────────┘
                                  │
                         ┌────────▼────────┐
                         │ Message / Sync  │
                         │     Layer       │
                         └────────┬────────┘
                                  │
                         ┌────────▼────────┐
                         │  SMS / STK      │
                         │   Gateway       │
                         └─────────────────┘
```

The learning engine never needs to know whether a learner answered through a React
interface or replied to an SMS. It only needs to process the learner's action.

### Transport pipeline

```text
Learning Event
      ↓
Message Queue
      ↓
Transport Router
      ↓
 ┌────┴─────┐
 │          │
SMS        STK
 │          │
 └────┬─────┘
      ↓
    Learner
```

Adding a channel means adding a transport, not rewriting the learning engine.
Candidate transports: Web, Android, SMS, SIM Toolkit, WhatsApp, Email, push.

---

## Learning State

Consistency across interfaces is the central engineering problem. Consider:

```text
Lesson 4
Question 1
Answer: B
Status: Correct
Progress: 42%
```

That state is identical regardless of the submitting interface. A learner can start
a lesson in the app, lose connectivity, receive the next question by SMS, reply
`B`, then reconnect and see updated progress. The application becomes a
synchronization problem as much as an education problem.

---

## Offline-First Design

The client caches course metadata, downloaded lessons, assessments, profile
information, and progress state. Actions taken offline enter a local queue:

```text
User Action
    ↓
Local Store → Sync Queue
    ↓
Internet Available?
    │
   No ──→ Wait
    │
   Yes → Backend → Conflict Resolution → Synced State
```

SMS provides a second path for actions that cannot wait for a connection.

---

## Reliability and Idempotency

SMS is asynchronous, so the messaging layer must be treated as a distributed
system. It has to account for delivery delays, duplicate and out-of-order messages,
failed delivery, retries, expiry, duplicate learner responses, network outages, and
gateway failures.

A learner who sends the same answer twice because no acknowledgement arrived must
not be scored twice:

```text
SMS Response
     ↓
Generate / Extract Message ID
     ↓
Check processed events
     │
     ├── Already processed → Ignore
     │
     └── New event → Process answer → Update progress → Send acknowledgement
```

---

## Student Experience

The primary experience is a modern learning interface, not a collection of SMS menus.
The dashboard shows current courses, current lesson, overall progress, upcoming
assessments, and recent activity. A course view shows modules, lessons, completion
status, assessments, and results.

An assessment in the app:

```text
────────────────────────
     Mathematics
      Algebra 01
────────────────────────

Solve:

2x + 4 = 10

○ A. 2
○ B. 3
○ C. 4
○ D. 5

          [ Submit ]
────────────────────────
```

The same assessment over SMS:

```text
NEO LEARN

Algebra 01
Question 3/10

2x + 4 = 10

A. 2
B. 3
C. 4
D. 5

Reply A, B, C or D.
```

Both interfaces generate the same learner action.

---

## Teacher Platform

A dedicated educator interface for creating courses, lessons, and assessments;
assigning courses; managing classes; monitoring learner progress; reviewing results;
scheduling content; sending announcements; and identifying learners falling behind.

A teacher never needs to know whether a student completed a lesson through the
application or through SMS. The platform handles that abstraction.

---

## Administration

An administrative interface provides institution-level controls: schools and
institutions, teacher and student accounts, classes, course management, messaging
statistics, delivery status, user management, permissions, usage analytics, and
system health. This demonstrates the multi-tenant architecture real-world SaaS
systems require.

---

## Example Flow

A student begins a mathematics course.

**09:00** — Opens Neo Learn; the app downloads the day's lesson.
**09:05** — Internet connectivity drops.
**09:06** — The backend determines the lesson is incomplete and delivers it by SMS.
**09:10** — The learner receives the Fractions lesson and a multiple-choice
question, and replies `A`.
**09:10** — The backend processes the response and records the result.
**14:30** — Connectivity returns. The dashboard now shows:

```text
Mathematics

Fractions
████████████░░ 80%

Last activity:
Completed via SMS

Score:
4 / 5
```

The learning session never broke. The interface simply changed.

---

## Proposed Technology Stack

The stack is deliberately less important than keeping the learning engine
independent of the transport mechanism.

As built so far:

| Layer | Choice |
| --- | --- |
| Frontend | Next.js 15, React 19, TypeScript (strict), Tailwind, Vitest |
| Backend | Go 1.27, `net/http` + chi |
| Data | PostgreSQL 17, sqlc-typed queries, golang-migrate, Redis for sessions |
| Messaging | *planned* — SMS gateway, SIM Toolkit, outbox worker |
| Infrastructure | Docker Compose, Makefile |

---

## Roadmap

The initial versions focus on proving the core architecture rather than shipping a
complete LMS.

**Phase 1 — Core Platform** · *in progress*
Authentication, student accounts, courses, lessons, assessments, and progress
tracking are built and tested. The teacher dashboard is not.

**Phase 1.5 — Synchronization** · *partly built*
The offline queue and the cross-transport event model are in place ahead of
their original position, because the SMS worker depends on both. Native app
packaging and service-worker caching are not.

**Phase 2 — SMS** · *built, no live provider yet*
Lesson delivery, SMS assessments, response processing, delivery tracking, the
outbox drain worker, and retry handling are implemented and tested. The outbound
gateway is a logging stub; a real provider means implementing `sms.Gateway`.
Still missing: delivery-report reconciliation and the scheduler that decides
*when* to open an SMS session.

**Phase 3 — Synchronization** · *superseded by 1.5*
Offline state, the sync queue, and conflict handling shipped with Phase 1.5.

**Phase 4 — SIM Toolkit**
STK interaction, menu-based navigation, assessment interaction, account lookup,
progress retrieval.

**Phase 5 — Analytics**
Learner engagement, completion rates, assessment performance, SMS delivery metrics,
offline activity, teacher dashboards.

---

## The Bigger Idea

Neo Learn is not trying to replace modern learning applications with SMS. It is
exploring the opposite: **what if modern learning applications could use SMS when
they need to?**

Whether a request arrives over Wi-Fi, 4G, 5G, offline cache, SMS, or SIM Toolkit,
the platform should still resolve it to the same record:

```text
Joseph → Mathematics → Algebra → Question 4 → Answer B → Correct → Progress updated
```

That separation between **learning state and communication transport** is the
central idea behind Neo Learn. The question being explored:

> Can a modern learning platform provide a continuous user experience when the
> underlying network changes from broadband internet to SMS?

---

## Why Build It?

Neo Learn is primarily a systems engineering experiment. It brings together problems
that usually exist independently: offline-first applications, distributed state
synchronization, messaging systems, SMS infrastructure, assessment engines,
event-driven architecture, idempotent processing, queue management, authentication,
multi-tenant SaaS design, educational content management, and responsive design.

---

## Status

**Milestones 1 and 2 built. Experimental / side project.**

What runs today:

- **Learning engine** — an append-only event log is the source of truth; progress is a projection of it. A single `UNIQUE (event_id)` constraint is what makes a duplicated action a no-op, whether it came from a double-click, an offline queue replay, or a learner texting the same answer twice.
- **REST API** — auth, courses, lessons, assessments, progress, and `POST /v1/events` as the single write path for learner state.
- **Web client** — sign in, dashboard, lesson view, and an assessment surface that shows the server's verdict. Holds an offline queue in `localStorage`; answers survive a lost connection.
- **SMS delivery** — an outbox drain worker with exponential backoff, GSM-7/UCS-2 chunking, terminal-vs-retryable failure handling, and opt-out. Inbound replies arrive at a shared-secret-authenticated webhook and are resolved to a learner by sender number, then become ordinary learning events.
- **Tests** — integration tests against real Postgres and Redis, plus 54 frontend tests.

What does not exist yet: the SIM Toolkit transport, delivery-report
reconciliation, the SMS delivery *scheduler* (nothing decides when to open an
SMS session), and the teacher and administrator interfaces.

The outbound gateway is a **logging implementation**, not a real provider. It
records what would have been sent and accepts injected failures, so the retry
paths are exercised without paying for messages. Adding a real provider means
implementing `sms.Gateway` and nothing else.

See [`docs/architecture.md`](docs/architecture.md) for why the system is shaped
this way, including the alternatives that were rejected.

---

## Getting Started

Requires Go 1.27, Node 20.11+, and Docker Desktop.

```bash
# 1. Start Postgres (host port 5433) and Redis (6379)
make up

# 2. Apply migrations
make migrate

# 3. Seed a development institution, course, and assessment
go run ./cmd/seed

# 4. Start the API on :8080
make run

# 5. Start the web client on :3000, in another shell
make web-install
make web-dev
```

Sign in at `http://localhost:3000/login` with the credentials printed by the
seed command. `joseph.learner@example.test` has a phone number attached and is
the account to use when testing SMS delivery later.

Run the tests:

```bash
make test          # Go integration tests
make web-install && cd web && npm test
```

Go tests skip rather than fail when Postgres and Redis are unreachable, so
`go test ./...` still works without Docker.

---

## Repository Layout

```text
cmd/api            HTTP server and SMS drain worker
cmd/seed           development fixtures
db/migrations      golang-migrate, up/down pairs
db/queries         SQL, type-checked against the schema by sqlc
internal/db        generated: models and typed queries
internal/learning  the engine — knows nothing about HTTP, SMS, or STK
internal/sms       transport: gateway, rendering, chunking, delivery planning
internal/inbound   inbound replies → learning events
internal/outbox    drain worker, retries, backoff
internal/auth      argon2id hashing, Redis sessions
internal/httpapi   REST transport
internal/store     connection pool
web/               Next.js learner interface
docs/              architecture decisions
```

`internal/learning` is the boundary that matters. It accepts a learner action
and applies it to learner state; it has no transport awareness. Adding SMS meant
calling `Ingest`, not changing anything in there.

## Trying SMS locally

With no provider account, the log gateway records what would have been sent:

```bash
SMS_INBOUND_SECRET=local-dev-secret make run
```

To exercise inbound, point a webhook at `/v1/sms/inbound` with that header set:

```bash
curl -X POST http://localhost:8080/v1/sms/inbound \
  -H 'Content-Type: application/json' \
  -H 'X-Neo-Auth: local-dev-secret' \
  -d '{"from":"+254700000001","body":"B","message_id":"gw-msg-1"}'
```

The reply is logged rather than delivered. Send the same `message_id` twice and
the second is reported as a duplicate and changes nothing.

## License

To be decided.