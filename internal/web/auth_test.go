package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"codedeep/internal/app"
	"codedeep/internal/config"
)

func authTestHandler(token string) http.Handler {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return withAuth(next, token)
}

func TestAuthDisabledWithoutToken(t *testing.T) {
	h := authTestHandler("")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestAuthRejectsAPICalls(t *testing.T) {
	h := authTestHandler("secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "Bearer") {
		t.Fatalf("WWW-Authenticate = %q", rec.Header().Get("WWW-Authenticate"))
	}
}

func TestAuthRedirectsPagesToLogin(t *testing.T) {
	h := authTestHandler("secret")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("status = %d, location = %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestAuthAcceptsBearerAndCookie(t *testing.T) {
	h := authTestHandler("secret")
	cases := []struct {
		name string
		req  *http.Request
	}{
		{
			name: "bearer",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
				r.Header.Set("Authorization", "Bearer secret")
				return r
			}(),
		},
		{
			name: "cookie",
			req: func() *http.Request {
				r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
				r.AddCookie(&http.Cookie{Name: tokenCookie, Value: "secret"})
				return r
			}(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, tc.req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
		})
	}
}

func TestAuthRejectsWrongCredentials(t *testing.T) {
	h := authTestHandler("secret")
	for _, header := range []string{"Bearer nope", "Basic secret"} {
		req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		req.Header.Set("Authorization", header)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%q: status = %d", header, rec.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	req.AddCookie(&http.Cookie{Name: tokenCookie, Value: "nope"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong cookie: status = %d", rec.Code)
	}
}

func TestAuthQueryToken(t *testing.T) {
	h := authTestHandler("secret")
	// API calls accept the query token directly.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health?token=secret", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("api status = %d", rec.Code)
	}

	// Page loads trade the query token for a cookie and strip it from the URL.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/?token=secret&x=1", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("page status = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "token=") || !strings.HasPrefix(loc, "/?") {
		t.Fatalf("location = %q", loc)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), tokenCookie+"=secret") {
		t.Fatalf("cookies = %q", rec.Header().Get("Set-Cookie"))
	}
}

func TestAuthExemptsStaticAndLogin(t *testing.T) {
	h := authTestHandler("secret")
	for _, path := range []string{"/static/app.js", "/login"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d", path, rec.Code)
		}
	}
}

func newLoginServer(token string) *Server {
	cfg := &config.Config{WebToken: token}
	return &Server{App: &app.App{Cfg: cfg}, hub: newApprovalHub(), active: map[string]bool{}}
}

func TestLoginFormAndSubmit(t *testing.T) {
	s := newLoginServer("secret")

	rec := httptest.NewRecorder()
	s.handleLogin(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `name="token"`) {
		t.Fatalf("form: status = %d body = %q", rec.Code, rec.Body.String())
	}

	form := "token=wrong"
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.handleLogin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token status = %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("token=secret"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.handleLogin(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("login status = %d location = %q", rec.Code, rec.Header().Get("Location"))
	}
	cookie := rec.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Name != tokenCookie || cookie[0].Value != "secret" || !cookie[0].HttpOnly {
		t.Fatalf("cookies = %+v", cookie)
	}
}

func TestLoginRedirectsWhenDisabledOrSignedIn(t *testing.T) {
	s := newLoginServer("")
	rec := httptest.NewRecorder()
	s.handleLogin(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("disabled token status = %d", rec.Code)
	}

	s = newLoginServer("secret")
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.AddCookie(&http.Cookie{Name: tokenCookie, Value: "secret"})
	rec = httptest.NewRecorder()
	s.handleLogin(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("signed-in status = %d", rec.Code)
	}
}

func editorCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd", []string{"/c", "exit"}
	}
	return "true", nil
}

func TestAuthenticatedHandlerRoutes(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	command, args := editorCommand()
	cfg := &config.Config{
		WorkDir:     work,
		SessionsDir: t.TempDir(),
		TracesDir:   t.TempDir(),
		Home:        t.TempDir(),
		WebToken:    "secret",
		Editor:      &config.Editor{Command: command, Args: args},
	}
	a, err := app.New(cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	defer a.Close()
	h := New(a).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bootstrap without token: %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d", rec.Code)
	}
	var boot map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &boot); err != nil {
		t.Fatalf("bootstrap JSON: %v", err)
	}
	if _, ok := boot["skills"]; !ok {
		t.Fatal("bootstrap is missing skills metadata")
	}

	req = httptest.NewRequest(http.MethodGet, "/api/skills", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("skills = %d %q", rec.Code, rec.Body.String())
	}

	body := strings.NewReader(`{"path":"main.go","line":3}`)
	req = httptest.NewRequest(http.MethodPost, "/api/open", body)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("open status = %d body = %q", rec.Code, rec.Body.String())
	}
	var opened map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &opened); err != nil {
		t.Fatalf("open JSON: %v", err)
	}
	if opened["line"].(float64) != 3 {
		t.Fatalf("open = %+v", opened)
	}

	body = strings.NewReader(`{"path":"../escape.txt"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/open", body)
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("outside path status = %d", rec.Code)
	}
}
