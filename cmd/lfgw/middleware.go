package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/rs/zerolog/hlog"
)

// nonProxiedEndpointsMiddleware is a workaround to support healthz and metrics endpoints while forwarding everything else to an upstream.
func (app *application) nonProxiedEndpointsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/healthz") {
			w.WriteHeader(http.StatusOK)
			return
		}

		if strings.HasPrefix(r.URL.Path, "/metrics") {
			metrics.WritePrometheus(w, true)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// logMiddleware populates zerolog context with additional fields that are used in log entries generated by other middlewares. Also, it optionally generates access logs.
func (app *application) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next = hlog.RequestIDHandler("req_id", "Request-Id")(next)

		if app.Debug {
			err := r.ParseForm()
			if err != nil {
				app.clientError(w, http.StatusBadRequest)
				return
			}

			// Once r.ParseForm() is called, we need to update ContentLength, otherwise the request will fail. r.PostForm contains data for PATCH, POST, and PUT requests.
			postForm := r.PostForm.Encode()
			newBody := strings.NewReader(postForm)
			r.ContentLength = newBody.Size()
			r.Body = io.NopCloser(newBody)

			// If any of those are empty, they won't get logged
			app.enrichDebugLogContext(r, "get_params", app.unescapedURLQuery(r.URL.Query().Encode()))
			app.enrichDebugLogContext(r, "post_params", app.unescapedURLQuery(postForm))
		}

		if app.LogRequests || app.Debug {
			next = hlog.AccessHandler(func(r *http.Request, status, size int, duration time.Duration) {
				// TODO: optionally change to debug?
				hlog.FromRequest(r).Info().
					Str("method", r.Method).
					Stringer("url", r.URL).
					Int("status", status).
					Int("size", size).
					Dur("duration", duration).
					Msg("")
			})(next)
		}
		next.ServeHTTP(w, r)
	})
}

// safeModeMiddleware forbids access to some HTTP methods and APIs if safe mode is enabled
func (app *application) safeModeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.SafeMode {
			// TODO: allow OPTIONS?
			if r.Method != http.MethodGet && r.Method != http.MethodPost {
				w.Header().Set("Allow", fmt.Sprintf("%s, %s", http.MethodGet, http.MethodPost))
				app.clientError(w, http.StatusMethodNotAllowed)
				return
			}

			// TODO: more unsafe paths?
			if app.isUnsafePath(r.URL.Path) {
				hlog.FromRequest(r).Error().Caller().
					Msgf("Blocked a request to %s", r.URL.Path)
				app.clientError(w, http.StatusForbidden)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// proxyHeadersMiddleware sets proxy headers.
func (app *application) proxyHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.SetProxyHeaders {
			r.Header.Set("X-Forwarded-For", r.RemoteAddr)
			r.Header.Set("X-Forwarded-Proto", r.URL.Scheme)
			r.Header.Set("X-Forwarded-Host", r.Header.Get("Host"))
		}
		next.ServeHTTP(w, r)
	})
}

// oidcModeMiddleware verifies a jwt token, and, if valid and authorized,
// adds a respective label filter to the request context.
func (app *application) oidcModeMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawAccessToken, err := app.getRawAccessToken(r)
		if err != nil {
			app.clientErrorMessage(w, http.StatusUnauthorized, err)
			return
		}

		ctx := r.Context()
		accessToken, err := app.verifier.Verify(ctx, rawAccessToken)
		if err != nil {
			// Better to log to see token verification errors
			hlog.FromRequest(r).Error().Caller().
				Err(err).Msg("")
			app.clientErrorMessage(w, http.StatusUnauthorized, err)
			return
		}

		// Extract custom claims
		var claims struct {
			Roles []string `json:"roles"`
			Email string   `json:"email"`
			// Username string   `json:"preferred_username"`
		}
		if err := accessToken.Claims(&claims); err != nil {
			// Claims not set, bad token
			hlog.FromRequest(r).Error().Caller().
				Err(err).Msg("")
			app.clientErrorMessage(w, http.StatusUnauthorized, err)
			return
		}

		app.enrichLogContext(r, "email", claims.Email)
		// NOTE: The field will contain all roles present in the token, not only those that are considered during ACL generation process
		app.enrichDebugLogContext(r, "roles", strings.Join(claims.Roles, ", "))

		acl, err := app.getACL(claims.Roles)
		if err != nil {
			hlog.FromRequest(r).Error().Caller().
				Err(err).Msg("")
			app.clientErrorMessage(w, http.StatusUnauthorized, err)
			return
		}

		app.enrichDebugLogContext(r, "label_filter", string(acl.LabelFilter.AppendString(nil)))

		ctx = context.WithValue(ctx, contextKeyACL, acl)
		r = r.WithContext(ctx)

		next.ServeHTTP(w, r)
	})
}

// rewriteRequestMiddleware rewrites a request before forwarding it to the upstream.
func (app *application) rewriteRequestMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Rewrite request destination
		r.Host = app.UpstreamURL.Host

		if app.isNotAPIRequest(r.URL.Path) {
			hlog.FromRequest(r).Debug().Caller().
				Msg("Not an API request, request is not modified")
			next.ServeHTTP(w, r)
			return
		}

		acl, ok := r.Context().Value(contextKeyACL).(ACL)
		if !ok {
			// Should never happen. It means OIDC middleware hasn't done it's job
			app.serverError(w, r, fmt.Errorf("ACL is not set in the context"))
			return
		}

		if acl.Fullaccess {
			hlog.FromRequest(r).Debug().Caller().
				Msg("User has full access, request is not modified")
			next.ServeHTTP(w, r)
			return
		}

		err := r.ParseForm()
		if err != nil {
			app.clientError(w, http.StatusBadRequest)
			return
		}

		// Adjust GET params
		getParams := r.URL.Query()
		newGetParams, err := app.prepareQueryParams(&getParams, acl)
		if err != nil {
			hlog.FromRequest(r).Error().Caller().
				Err(err).Msg("")
			app.clientError(w, http.StatusBadRequest)
			return
		}
		r.URL.RawQuery = newGetParams
		app.enrichDebugLogContext(r, "new_get_params", app.unescapedURLQuery(newGetParams))

		// Adjust POST params
		if r.Method == http.MethodPost {
			newPostParams, err := app.prepareQueryParams(&r.PostForm, acl)
			if err != nil {
				hlog.FromRequest(r).Error().Caller().
					Err(err).Msg("")
				app.clientError(w, http.StatusBadRequest)
				return
			}
			newBody := strings.NewReader(newPostParams)
			r.ContentLength = newBody.Size()
			r.Body = io.NopCloser(newBody)
			app.enrichDebugLogContext(r, "new_post_params", app.unescapedURLQuery(newPostParams))
		}

		next.ServeHTTP(w, r)
	})
}
