-- Reverse of 000001_init. Dependency-ordered; tables with no dependents
-- are dropped first.

DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS progress;
DROP TABLE IF EXISTS learning_events;
DROP TABLE IF EXISTS course_enrollments;
DROP TABLE IF EXISTS choices;
DROP TABLE IF EXISTS questions;
DROP TABLE IF EXISTS assessments;
DROP TABLE IF EXISTS lessons;
DROP TABLE IF EXISTS modules;
DROP TABLE IF EXISTS courses;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS institutions;

DROP TYPE IF EXISTS event_kind;
DROP TYPE IF EXISTS event_source;
DROP TYPE IF EXISTS question_kind;
DROP TYPE IF EXISTS user_role;

DROP EXTENSION IF EXISTS citext;