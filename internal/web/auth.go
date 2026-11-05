package web

import (
	"crypto/subtle"
	"html/template"
	"net/http"
	"strings"
	"time"
)

const tokenCookie = "codedeep_token"

var loginPage = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>CodeDeep · sign in</title>
<link rel="stylesheet" href="/static/styles.css">
<style>
  body { display: grid; place-items: center; min-height: 100vh; margin: 0; background: var(--bg, #16181d); color: var(--fg, #e6e8ee); font-family: ui-sans-serif, system-ui, sans-serif; }
  form { width: min(360px, 90vw); padding: 28px; border: 1px solid #2b2f39; border-radius: 12px; background: #1c1f27; }
  h1 { font-size: 18px; margin: 0 0 6px; }
  p { font-size: 13px; color: #9aa1af; margin: 0 0 18px; }
  input { width: 100%; box-sizing: border-box; padding: 10px 12px; border-radius: 8px; border: 1px solid #343a46; background: #14161c; color: inherit; }
  button { margin-top: 14px; width: 100%; padding: 10px 12px; border: 0; border-radius: 8px; background: #5b8cff; color: white; font-weight: 600; cursor: pointer; }
  .error { color: #ff8b8b; margin-top: 12px; font-size: 13px; }
</style>
</head>
<body>
<form method="post" action="/login">
  <h1>CodeDeep</h1>
  <p>Enter the access token to open this workspace.</p>
  <input type="password" name="token" autocomplete="current-password" autofocus required aria-label="Access token">
  <button type="submit">Sign in</button>
  {{if .Error}}<div class="error">{{.Error}}</div>{{end}}
</form>
</body>
</html>`))

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	token := s.App.Cfg.WebToken
	if r.Method == http.MethodGet {
		if token == "" || validCookie(r, token) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		renderLogin(w, http.StatusOK, "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		renderLogin(w, http.StatusBadRequest, "Invalid request.")
		return
	}
	provided := r.PostFormValue("token")
	if !secureEqual(provided, token) {
		renderLogin(w, http.StatusUnauthorized, "That token is not correct.")
		return
	}
	setTokenCookie(w, r, token)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func renderLogin(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = loginPage.Execute(w, map[string]string{"Error": message})
}

func withAuth(next http.Handler, token string) http.Handler {
	token = strings.TrimSpace(token)
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" || strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		if q := r.URL.Query().Get("token"); q != "" && secureEqual(q, token) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				next.ServeHTTP(w, r)
				return
			}
			setTokenCookie(w, r, token)
			redirectWithoutToken(w, r)
			return
		}
		if validRequest(r, token) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("WWW-Authenticate", `Bearer realm="codedeep"`)
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func validRequest(r *http.Request, token string) bool {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if secureEqual(strings.TrimSpace(strings.TrimPrefix(h, "Bearer ")), token) {
			return true
		}
	}
	return validCookie(r, token)
}

func validCookie(r *http.Request, token string) bool {
	c, err := r.Cookie(tokenCookie)
	return err == nil && secureEqual(c.Value, token)
}

func setTokenCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int((30 * 24 * time.Hour).Seconds()),
	})
}

func redirectWithoutToken(w http.ResponseWriter, r *http.Request) {
	u := *r.URL
	q := u.Query()
	q.Del("token")
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
}

func secureEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
