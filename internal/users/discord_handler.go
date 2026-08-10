package users

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tabuhana/bombers-server/internal/auth"
	"github.com/tabuhana/bombers-server/internal/config"
	"github.com/tabuhana/bombers-server/internal/discord"
	"github.com/tabuhana/bombers-server/internal/httpx"
	"github.com/tabuhana/bombers-server/internal/logx"
	"github.com/tabuhana/bombers-server/internal/types"
)

// Signing in with Discord — the only way in.
//
// Four endpoints, none of them authenticated, because they're what you use
// before you have a session:
//
//	GET  /auth/discord/start     begin, from the app or the website
//	GET  /auth/discord/callback  where Discord returns
//	POST /auth/discord/claim     the client trades a handoff code for tokens
//	POST /auth/discord/signup    the website finishes a new account with a name
//
// The shape is unusual in one way worth stating up front: the CALLBACK never
// returns tokens or JSON. It's the target of a browser redirect, so it answers
// with another redirect and hands over a single-use code. Tokens are collected
// separately, over a request the client makes itself.

// Where a login came from, in the query of /auth/discord/start.
const (
	fromApp = "app"
	fromWeb = "web"
)

// What the browser is told, in the query of the redirect back.
const (
	// A refused login from the APP says one thing and explains nothing. The only
	// route to the installer is the website terminal, so a client presenting an
	// identity with no account did not come through it.
	errNotAuthorized = "not_authorized"
	// From the website, where somebody is legitimately trying to sign up.
	errNotAllowed   = "not_allowed"
	errLoginFailed  = "login_failed"
	errSignupClosed = "signup_closed"
)

// DiscordHandler owns the sign-in flow.
type DiscordHandler struct {
	pool    *pgxpool.Pool
	auth    *auth.Service
	client  *discord.Client
	pending *PendingStore

	// signupMode is config.SignupList or config.SignupOpen.
	signupMode string
	// websiteURL is where a web login returns. Required for that half to work.
	websiteURL string
}

func NewDiscordHandler(pool *pgxpool.Pool, authSvc *auth.Service, cfg *config.Config) *DiscordHandler {
	return &DiscordHandler{
		pool: pool,
		auth: authSvc,
		client: &discord.Client{
			ClientID:     cfg.DiscordClientID,
			ClientSecret: cfg.DiscordClientSecret,
			RedirectURL:  cfg.DiscordRedirectURL,
		},
		pending:    NewPendingStore(),
		signupMode: cfg.SignupMode,
		websiteURL: strings.TrimRight(cfg.WebsiteURL, "/"),
	}
}

// Configured reports whether sign-in can work at all, for the startup log.
func (h *DiscordHandler) Configured() bool { return h.client.Configured() }

// Start sends the browser to Discord.
//
//	/auth/discord/start?from=app&port=54321
//	/auth/discord/start?from=web
//
// The app supplies a PORT, not a URL. That's deliberate: the callback ends in a
// redirect, and an endpoint that redirects to a URL a caller handed it is an
// open redirect. A port number can only ever become an address on the machine
// that asked.
func (h *DiscordHandler) Start(w http.ResponseWriter, r *http.Request) {
	if !h.client.Configured() {
		httpx.WriteError(w, http.StatusServiceUnavailable, "this server has no Discord application configured, so nobody can sign in")
		return
	}

	from := r.URL.Query().Get("from")
	var returnTo string
	switch from {
	case fromApp:
		port, err := strconv.Atoi(r.URL.Query().Get("port"))
		if err != nil || port < 1024 || port > 65535 {
			httpx.WriteError(w, http.StatusBadRequest, "port must be a number between 1024 and 65535")
			return
		}
		// 127.0.0.1 rather than "localhost": a name resolves through whatever the
		// machine's hosts file says, and this has to mean the loopback.
		returnTo = fmt.Sprintf("http://127.0.0.1:%d/", port)
	case fromWeb:
		if h.websiteURL == "" {
			httpx.WriteError(w, http.StatusServiceUnavailable, "this server has no website configured")
			return
		}
		returnTo = h.websiteURL + "/"
	default:
		httpx.WriteError(w, http.StatusBadRequest, "from must be app or web")
		return
	}

	state, err := h.pending.StartLogin(returnTo, from == fromApp)
	if err != nil {
		logx.Error("discord: generating state: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not start a login")
		return
	}
	http.Redirect(w, r, h.client.AuthorizeURL(state), http.StatusFound)
}

// Callback is where Discord sends the browser back.
func (h *DiscordHandler) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// Claimed FIRST, and claiming consumes it. Everything after this can fail
	// safely; a state that survived a failure could be replayed.
	pending, ok := h.pending.ClaimLogin(q.Get("state"))
	if !ok {
		// With no valid state there is no verified place to send anybody, so this
		// is the one branch that answers directly instead of redirecting.
		httpx.WriteError(w, http.StatusBadRequest, "that sign-in link has expired — start again")
		return
	}

	// Discord reports a refusal (they hit Cancel) in the query, not by failing.
	if derr := q.Get("error"); derr != "" {
		h.bounce(w, r, pending, errLoginFailed, "")
		return
	}
	code := q.Get("code")
	if code == "" {
		h.bounce(w, r, pending, errLoginFailed, "")
		return
	}

	ctx := r.Context()

	token, err := h.client.Exchange(ctx, code)
	if err != nil {
		logx.Error("discord: code exchange: %v", err)
		h.bounce(w, r, pending, errLoginFailed, "")
		return
	}
	// We only ever needed it to read two things. Handing it back means a stolen
	// database contains no keys to anybody's Discord.
	defer h.client.Revoke(ctx, token)

	identity, err := h.client.Me(ctx, token)
	if err != nil {
		logx.Error("discord: reading the account: %v", err)
		h.bounce(w, r, pending, errLoginFailed, "")
		return
	}

	// Connections are decoration on a person card. Failing to read them is not a
	// failed login.
	var connections []byte
	if conns, cerr := h.client.Connections(ctx, token); cerr != nil {
		logx.Warn("discord: reading connections for %s: %v", identity.ID, cerr)
	} else {
		connections = EncodeConnections(conns)
	}

	profile := DiscordProfile{
		ID:          identity.ID,
		Username:    identity.Username,
		Avatar:      identity.Avatar,
		Connections: connections,
	}

	// Blocked is checked before anything else, including the allowlist, and a
	// query failure counts as blocked — the failure mode of a broken block is
	// nobody getting in, not everybody.
	blocked, err := IsBlocked(ctx, h.pool, identity.ID)
	if err != nil {
		logx.Error("discord: block check for %s: %v", identity.ID, err)
		h.bounce(w, r, pending, errNotAuthorized, "")
		return
	}
	if blocked {
		logx.Warn("discord: refused blocked identity %s (%s)", identity.ID, identity.Username)
		h.bounce(w, r, pending, h.refusalFor(pending), "")
		return
	}

	user, err := GetUserByDiscordID(ctx, h.pool, identity.ID)
	switch {
	case err == nil:
		h.signIn(w, r, pending, user, profile)
	case errors.Is(err, ErrUserNotFound):
		h.noAccount(w, r, pending, profile)
	default:
		logx.Error("discord: lookup by discord id: %v", err)
		h.bounce(w, r, pending, errLoginFailed, "")
	}
}

// signIn completes a login for an account that already exists.
func (h *DiscordHandler) signIn(w http.ResponseWriter, r *http.Request, pending PendingLogin, user *User, profile DiscordProfile) {
	if user.Banned {
		logx.Warn("discord: refused banned user %s", user.Username)
		h.bounce(w, r, pending, h.refusalFor(pending), "")
		return
	}

	// Their handle, picture and linked accounts move on Discord's side, and this
	// is the only moment we hold a token to look. A failure isn't worth refusing
	// a login over.
	if err := RefreshDiscordProfile(r.Context(), h.pool, user.ID, profile); err != nil {
		logx.Warn("discord: refreshing profile for %s: %v", user.Username, err)
	}

	handoffCode, err := h.pending.IssueHandoff(user.ID)
	if err != nil {
		logx.Error("discord: issuing handoff: %v", err)
		h.bounce(w, r, pending, errLoginFailed, "")
		return
	}
	h.bounce(w, r, pending, "", handoffCode)
}

// noAccount handles a verified identity with nothing behind it.
//
// From the app this is the end of the road: the installer is only reachable
// after finishing on the website, so a client asking on behalf of an identity
// with no account did not come through the one path there is. It's recorded and
// refused without explanation.
//
// From the website it's the normal beginning of a signup, and continues if
// they're allowed.
func (h *DiscordHandler) noAccount(w http.ResponseWriter, r *http.Request, pending PendingLogin, profile DiscordProfile) {
	ctx := r.Context()

	attempts, err := RecordSigninAttempt(ctx, h.pool, profile.ID, profile.Username)
	if err != nil {
		logx.Error("discord: recording attempt for %s: %v", profile.ID, err)
	}

	if pending.FromApp {
		logx.Warn("discord: unauthorized client for %s (%s) — attempt %d",
			profile.Username, profile.ID, attempts)
		h.bounce(w, r, pending, errNotAuthorized, "")
		return
	}

	if h.signupMode == config.SignupList {
		allowed, aerr := IsAllowed(ctx, h.pool, profile.ID)
		if aerr != nil {
			logx.Error("discord: allowlist check for %s: %v", profile.ID, aerr)
			h.bounce(w, r, pending, errNotAllowed, "")
			return
		}
		if !allowed {
			logx.Info("discord: %s (%s) is not on the allowlist", profile.Username, profile.ID)
			h.bounce(w, r, pending, errNotAllowed, "")
			return
		}
	}

	ticket, err := h.pending.IssueTicket(profile)
	if err != nil {
		logx.Error("discord: issuing signup ticket: %v", err)
		h.bounce(w, r, pending, errLoginFailed, "")
		return
	}
	h.redirect(w, r, pending.ReturnTo, url.Values{"signup": {ticket}})
}

// refusalFor picks how much a refusal says. The app is told nothing; the
// website, which is where signing up legitimately happens, is told it isn't
// allowed.
func (h *DiscordHandler) refusalFor(pending PendingLogin) string {
	if pending.FromApp {
		return errNotAuthorized
	}
	return errNotAllowed
}

// bounce sends the browser home, carrying either a handoff code or an error.
func (h *DiscordHandler) bounce(w http.ResponseWriter, r *http.Request, pending PendingLogin, errCode, handoffCode string) {
	q := url.Values{}
	if errCode != "" {
		q.Set("error", errCode)
	}
	if handoffCode != "" {
		q.Set("code", handoffCode)
	}
	h.redirect(w, r, pending.ReturnTo, q)
}

// redirect goes to a target this server built itself — a loopback address from
// a port, or the configured website. Never to anything a caller supplied.
func (h *DiscordHandler) redirect(w http.ResponseWriter, r *http.Request, target string, q url.Values) {
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// --- what the client asks for afterwards ---------------------------------

type claimRequest struct {
	Code string `json:"code"`
}

// sessionResponse is the same shape the password login returns, so a client has
// one session type rather than two.
type sessionResponse struct {
	User         types.PublicUser `json:"user"`
	AccessToken  string           `json:"access_token"`
	RefreshToken string           `json:"refresh_token"`
	ExpiresIn    int              `json:"expires_in"`
}

// Claim trades a handoff code for a session. This is the request the client
// makes itself, which is why the tokens are here and not in the redirect.
func (h *DiscordHandler) Claim(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, loginBodyLimit)

	var req claimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	userID, ok := h.pending.ClaimHandoff(req.Code)
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "that code is not valid")
		return
	}
	h.issueSession(w, r, userID)
}

type signupRequest struct {
	Ticket   string `json:"ticket"`
	Username string `json:"username"`
}

// Signup finishes a new account: the website has a verified, allowed identity
// parked under a ticket and a username the person typed.
func (h *DiscordHandler) Signup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, loginBodyLimit)

	var req signupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Peeked, not spent: a taken username has to be retryable at the same
	// prompt rather than sending somebody back through Discord.
	ticket, ok := h.pending.PeekTicket(req.Ticket)
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "that signup has expired — start again")
		return
	}

	user, err := CreateDiscordUser(r.Context(), h.pool, req.Username, ticket.Profile)
	switch {
	case err == nil:
	case errors.Is(err, ErrUsernameTaken):
		httpx.WriteError(w, http.StatusConflict, "username already taken")
		return
	case errors.Is(err, ErrDiscordAlreadyLinked):
		httpx.WriteError(w, http.StatusConflict, "that Discord account already has a Bombers account")
		return
	case errors.Is(err, ErrUsernameEmpty),
		errors.Is(err, ErrUsernameLength),
		errors.Is(err, ErrUsernameWhitespace):
		// The person is at a prompt and can just type another one.
		httpx.WriteError(w, http.StatusBadRequest, err.Error())
		return
	default:
		logx.Error("discord: creating user: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not create the account")
		return
	}

	// Only now, once the account exists.
	h.pending.SpendTicket(req.Ticket)
	// A new account's attempts are no longer interesting.
	if _, cerr := ClearSigninAttempt(r.Context(), h.pool, ticket.Profile.ID); cerr != nil {
		logx.Warn("discord: clearing attempts for %s: %v", ticket.Profile.ID, cerr)
	}

	logx.Info("discord: created %s for %s (%s)", user.Username, ticket.Profile.Username, ticket.Profile.ID)
	h.issueSession(w, r, user.ID)
}

// issueSession mints the token pair and returns it with the user.
func (h *DiscordHandler) issueSession(w http.ResponseWriter, r *http.Request, userID string) {
	user, err := getUserByID(r.Context(), h.pool, userID)
	if err != nil {
		logx.Error("discord: loading user %s: %v", userID, err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not complete the sign-in")
		return
	}

	pair, err := h.auth.IssueInitialPair(r.Context(), user.ID)
	if err != nil {
		logx.Error("discord: issuing tokens: %v", err)
		httpx.WriteError(w, http.StatusInternalServerError, "could not issue tokens")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, sessionResponse{
		User:         user.Public(),
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		ExpiresIn:    pair.ExpiresIn,
	})
}
