package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/neolearn/neolearn/internal/auth"
	"github.com/neolearn/neolearn/internal/config"
	"github.com/neolearn/neolearn/internal/db"
	"github.com/neolearn/neolearn/internal/learning"
)

// Server holds the dependencies every handler needs.
type Server struct {
	cfg      *config.Config
	queries  *db.Queries
	sessions *auth.SessionStore
	learning *learning.Service
	hasher   auth.HashParams
}

// NewServer wires a Server.
func NewServer(
	cfg *config.Config,
	queries *db.Queries,
	sessions *auth.SessionStore,
	learningService *learning.Service,
) *Server {
	return &Server{
		cfg:      cfg,
		queries:  queries,
		sessions: sessions,
		learning: learningService,
		hasher:   auth.DefaultHashParams(cfg.Argon2.Time, cfg.Argon2.Memory, cfg.Argon2.Threads),
	}
}

// Router builds the HTTP handler.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(RequestLogger)
	r.Use(securityHeaders)
	r.Use(AllowWebOrigin(s.cfg.WebOrigin))

	r.Get("/healthz", s.handleHealth)

	r.Route("/v1", func(r chi.Router) {
		// Public.
		r.Post("/auth/login", s.handleLogin)

		// Authenticated.
		r.Group(func(r chi.Router) {
			r.Use(RequireSession(s.sessions))

			r.Post("/auth/logout", s.handleLogout)
			r.Get("/me", s.handleMe)

			r.Get("/courses", s.handleListCourses)
			r.Get("/courses/{courseID}", s.handleGetCourse)
			r.Get("/courses/{courseID}/lessons", s.handleListLessons)
			r.Get("/lessons/{lessonID}", s.handleGetLesson)
			r.Get("/courses/{courseID}/assessments", s.handleListAssessments)
			r.Get("/assessments/{assessmentID}", s.handleGetAssessment)

			// The single write path for learner state, shared by every
			// transport.
			r.Post("/events", s.handleIngestEvent)

			r.Get("/progress", s.handleGetProgress)
		})
	})

	return r
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

type loginRequest struct {
	Institution string `json:"institution"`
	Email       string `json:"email"`
	Password    string `json:"password"`
}

type userResponse struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Institution string `json:"institution_slug"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeRequestError(w, err)
		return
	}

	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	req.Institution = strings.ToLower(strings.TrimSpace(req.Institution))

	if req.Email == "" || req.Password == "" {
		writeErrorDetails(w, http.StatusBadRequest, "missing_credentials",
			"institution, email, and password are required",
			map[string]string{
				"email":       requiredIfBlank(req.Email),
				"password":    requiredIfBlank(req.Password),
				"institution": requiredIfBlank(req.Institution),
			})
		return
	}

	inst, err := s.queries.GetInstitutionBySlug(r.Context(), req.Institution)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Same response as a wrong password: a distinct error would let
			// someone enumerate which institutions exist.
			writeError(w, http.StatusUnauthorized, "invalid_credentials", "institution, email, or password is incorrect")
			return
		}
		internalError(w, r, "auth.lookup_institution", err)
		return
	}

	user, err := s.queries.GetUserByEmail(r.Context(), db.GetUserByEmailParams{
		InstitutionID: inst.ID,
		Email:         req.Email,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusUnauthorized, "invalid_credentials", "institution, email, or password is incorrect")
			return
		}
		internalError(w, r, "auth.lookup_user", err)
		return
	}

	if user.PasswordHash == nil || *user.PasswordHash == "" {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "this account cannot sign in with a password")
		return
	}

	storedHash := *user.PasswordHash

	ok, err := s.hasher.Verify(req.Password, storedHash)
	if err != nil {
		internalError(w, r, "auth.verify_password", err)
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "institution, email, or password is incorrect")
		return
	}

	// Opportunistic upgrade: the cost parameters may have been raised since
	// this password was set.
	if s.hasher.NeedsRehash(storedHash) {
		if upgraded, err := s.hasher.Hash(req.Password); err == nil {
			_ = s.queries.UpdatePasswordHash(r.Context(), db.UpdatePasswordHashParams{
				InstitutionID: inst.ID,
				ID:            user.ID,
				PasswordHash:  &upgraded,
			})
		}
	}

	expiresAt := time.Now().Add(s.cfg.Session.TTL)
	token, err := s.sessions.Create(r.Context(), auth.Session{
		UserID:        user.ID,
		InstitutionID: inst.ID,
		Role:          string(user.Role),
		Email:         user.Email,
		DisplayName:   user.DisplayName,
		IssuedAt:      time.Now().UTC(),
		ExpiresAt:     expiresAt,
	})
	if err != nil {
		internalError(w, r, "auth.create_session", err)
		return
	}

	s.setSessionCookie(w, token, expiresAt)
	// Best-effort audit trail; a failure here must not fail the login.
	_ = s.queries.TouchSession(r.Context(), auth.HashToken(token))

	writeJSON(w, http.StatusOK, userResponse{
		ID:          user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		Role:        string(user.Role),
		Institution: inst.Slug,
	})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" {
		if err := s.sessions.Delete(r.Context(), cookie.Value); err != nil {
			internalError(w, r, "auth.delete_session", err)
			return
		}
		_ = s.queries.RevokeSession(r.Context(), auth.HashToken(cookie.Value))
	}
	expireSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	sess, ok := sessionFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "sign in to continue")
		return
	}

	inst, err := s.queries.GetInstitutionByID(r.Context(), sess.InstitutionID)
	slug := ""
	if err == nil {
		slug = inst.Slug
	}

	writeJSON(w, http.StatusOK, userResponse{
		ID:          sess.UserID,
		Email:       sess.Email,
		DisplayName: sess.DisplayName,
		Role:        sess.Role,
		Institution: slug,
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, expiresAt time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		MaxAge:   int(s.cfg.Session.TTL.Seconds()),
		HttpOnly: true,
		Secure:   s.cfg.Session.CookieSecure,
		Domain:   s.cfg.Session.CookieDomain,
		// Lax rather than Strict: a learner following a link back from an
		// email should still be signed in. The cookie carries no authority
		// of its own, so the CSRF exposure is limited to top-level GET
		// navigation, all of which are read-only.
		SameSite: http.SameSiteLaxMode,
	})
}

func requiredIfBlank(v string) string {
	if strings.TrimSpace(v) == "" {
		return "required"
	}
	return ""
}

// ---------------------------------------------------------------------------
// Learning
// ---------------------------------------------------------------------------

type ingestEventRequest struct {
	// EventID is required and must be generated by the client. It is the
	// idempotency key: retrying an event with the same ID is a no-op.
	EventID      string `json:"event_id"`
	Kind         string `json:"kind"`
	Source       string `json:"source"`
	LessonID     *int64 `json:"lesson_id,omitempty"`
	AssessmentID *int64 `json:"assessment_id,omitempty"`
	QuestionID   *int64 `json:"question_id,omitempty"`
	ChoiceLabel  string `json:"choice_label,omitempty"`
	// OccurredAt is when the learner acted, RFC3339. Omitted means now.
	OccurredAt *time.Time `json:"occurred_at,omitempty"`
}

type ingestEventResponse struct {
	EventID   string         `json:"event_id"`
	Duplicate bool           `json:"duplicate"`
	Graded    *gradedPayload `json:"graded,omitempty"`
}

type gradedPayload struct {
	QuestionID   int64  `json:"question_id"`
	ChoiceLabel  string `json:"choice_label"`
	Correct      bool   `json:"correct"`
	Marks        int32  `json:"marks"`
	MarksAwarded int32  `json:"marks_awarded"`
	MarksTotal   int32  `json:"marks_total"`
	Percent      int32  `json:"percent"`
}

func (s *Server) handleIngestEvent(w http.ResponseWriter, r *http.Request) {
	identity, ok := requireIdentity(w, r)
	if !ok {
		return
	}

	var req ingestEventRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeRequestError(w, err)
		return
	}

	eventID, err := uuid.Parse(strings.TrimSpace(req.EventID))
	if err != nil {
		writeErrorDetails(w, http.StatusBadRequest, "invalid_event_id",
			"event_id must be a client-generated UUID",
			map[string]string{"event_id": "must be a valid UUID"})
		return
	}

	source := learning.Source(strings.TrimSpace(req.Source))
	if source == "" {
		source = learning.SourceWeb
	}

	payload := []byte("{}")
	if req.ChoiceLabel != "" {
		payload = mustJSON(learning.AnswerPayload{ChoiceLabel: strings.ToUpper(strings.TrimSpace(req.ChoiceLabel))})
	}

	ev := learning.Event{
		EventID:       eventID,
		InstitutionID: identity.InstitutionID,
		// The learner is always the authenticated user. A client cannot
		// submit events on someone else's behalf through this endpoint.
		LearnerID:    identity.UserID,
		LessonID:     req.LessonID,
		AssessmentID: req.AssessmentID,
		QuestionID:   req.QuestionID,
		Kind:         learning.Kind(strings.TrimSpace(req.Kind)),
		Source:       source,
		Payload:      payload,
	}
	if req.OccurredAt != nil {
		ev.OccurredAt = req.OccurredAt.UTC()
	}

	result, err := s.learning.Ingest(r.Context(), ev)
	if err != nil {
		writeLearningError(w, r, err)
		return
	}

	resp := ingestEventResponse{
		EventID:   result.EventID.String(),
		Duplicate: result.Duplicate,
	}
	if result.Graded != nil {
		resp.Graded = &gradedPayload{
			QuestionID:   result.Graded.QuestionID,
			ChoiceLabel:  result.Graded.ChoiceLabel,
			Correct:      result.Graded.Correct,
			Marks:        result.Graded.Marks,
			MarksAwarded: result.Graded.MarksAwarded,
			MarksTotal:   result.Graded.MarksTotal,
			Percent:      result.Graded.Percent,
		}
	}

	// A duplicate is a success, not a conflict: the learner's intent was
	// already recorded. Returning 200 keeps client retry logic simple.
	writeJSON(w, http.StatusOK, resp)
}

func writeRequestError(w http.ResponseWriter, err error) {
	var reqErr *RequestError
	if errors.As(err, &reqErr) {
		status := http.StatusBadRequest
		if reqErr.Code == "body_too_large" {
			status = http.StatusRequestEntityTooLarge
		}
		writeErrorDetails(w, status, reqErr.Code, reqErr.Message, reqErr.Details)
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request", "could not process the request")
}

// writeLearningError maps domain errors to status codes.
func writeLearningError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, learning.ErrEventIDRequired),
		errors.Is(err, learning.ErrEventIDInvalid),
		errors.Is(err, learning.ErrKindRequired),
		errors.Is(err, learning.ErrKindInvalid),
		errors.Is(err, learning.ErrSourceInvalid),
		errors.Is(err, learning.ErrLearnerRequired),
		errors.Is(err, learning.ErrChoiceRequired),
		errors.Is(err, learning.ErrQuestionRequired),
		errors.Is(err, learning.ErrLessonRequired):
		writeError(w, http.StatusBadRequest, "invalid_event", err.Error())

	case errors.Is(err, learning.ErrChoiceNotFound):
		writeError(w, http.StatusNotFound, "choice_not_found", "no such choice for that question")

	case errors.Is(err, learning.ErrQuestionNotFound):
		writeError(w, http.StatusNotFound, "question_not_found", "no such question")

	default:
		internalError(w, r, "learning.ingest", err)
	}
}

func mustJSON(v any) []byte {
	b, err := jsonMarshal(v)
	if err != nil {
		// Only reachable if a struct literal here gains an unmarshalable
		// field, which is a compile-time-review concern, not a runtime one.
		panic("httpapi: encode payload: " + err.Error())
	}
	return b
}
