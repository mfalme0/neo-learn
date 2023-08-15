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

| Layer | Choice |
| --- | --- |
| Frontend | React / Next.js, TypeScript, PWA capabilities, IndexedDB for offline state |
| Backend | Go or Python, REST API |
| Data | PostgreSQL, Redis for queues and transient state |
| Messaging | SMS gateway, SIM Toolkit integration, message queue for async delivery |
| Infrastructure | Docker, CI/CD, Linux, cloud or self-hosted deployment |

---

## Roadmap

The initial versions focus on proving the core architecture rather than shipping a
complete LMS.

**Phase 1 — Core Platform**
Authentication, student accounts, courses, lessons, assessments, progress tracking,
basic teacher dashboard.

**Phase 2 — SMS**
Lesson delivery, SMS assessments, response processing, delivery tracking, message
queue, retry handling.

**Phase 3 — Synchronization**
Offline application, local state, sync queue, conflict handling, cross-channel
progress synchronization.

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

**Under active design. Experimental / side project.**

Neo Learn is a practical exploration of resilient EdTech infrastructure with
particular attention to low-connectivity environments and multi-channel learning
experiences. There is no working implementation yet.

## License

To be decided.