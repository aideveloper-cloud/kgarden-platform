package main

// Who is at the floor tablet. People and their access are managed in ERPNext (the core): only users
// granted the MES_FLOOR app there can sign in, with the role given there (operator, supervisor;
// super admins get every app). Two ways in, both checked by ERPNext:
//
//	POST /auth/pin            employee code + PIN on a shared tablet (kgarden.access.api.verify_pin,
//	                          called with the MES service account)
//	GET  /auth/oidc/start     "เข้าสู่ระบบด้วย K Garden": OpenID Connect against ERPNext;
//	POST /auth/oidc/callback  the tablet hands back ?code&state, the MES exchanges it server-side
//
// Either way the MES issues its own session (HttpOnly cookie, stored hashed). Sessions end on logout,
// expiry, or when ERPNext reports the person lost MES access (polled from access_changes).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aideveloper-cloud/kgarden-platform/internal/erpnext"
	"github.com/aideveloper-cloud/kgarden-platform/internal/pgstore"
	"github.com/jackc/pgx/v5"
)

const (
	appCode       = "MES_FLOOR"
	sessionCookie = "kg_mes_session"
)

type OIDCConfig struct {
	ClientID, ClientSecret, RedirectURI string
	PublicBase                          string // ERPNext as the tablet's browser reaches it (sign-in page)
}

type Auth struct {
	store  *pgstore.Store
	access *erpnext.Client // MES service account (role KG Access Service)
	oidc   OIDCConfig
	ttl    time.Duration
	http   *http.Client
}

type Session struct {
	ID           string    `json:"-"`
	User         string    `json:"user"`
	FullName     string    `json:"full_name"`
	EmployeeCode string    `json:"employee_code"`
	Role         string    `json:"role"`
	Method       string    `json:"method"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// Label is what the MES records as the operator of an action.
func (s *Session) Label() string {
	if s.EmployeeCode != "" {
		return s.FullName + " (" + s.EmployeeCode + ")"
	}
	return s.FullName + " <" + s.User + ">"
}

type ctxKey struct{}

func sessionFrom(r *http.Request) *Session {
	s, _ := r.Context().Value(ctxKey{}).(*Session)
	return s
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// Require wraps a handler: a live session is needed, with one of roles (superadmin always passes).
func (a *Auth) Require(h http.HandlerFunc, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, err := a.lookup(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		if s == nil {
			writeErr(w, fail(401, "กรุณาเข้าสู่ระบบ"))
			return
		}
		if len(roles) > 0 && s.Role != "superadmin" && !contains(roles, s.Role) {
			writeErr(w, fail(403, "บทบาท %s ทำรายการนี้ไม่ได้", s.Role))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, s)))
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (a *Auth) lookup(r *http.Request) (*Session, error) {
	token := ""
	if c, err := r.Cookie(sessionCookie); err == nil {
		token = c.Value
	} else if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token = strings.TrimPrefix(h, "Bearer ")
	}
	if token == "" {
		return nil, nil
	}
	var s Session
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		return tx.QueryRow(r.Context(), `SELECT id::text, user_id, full_name, employee_code, role, method, expires_at
			FROM mes.session WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()`, hashToken(token)).
			Scan(&s.ID, &s.User, &s.FullName, &s.EmployeeCode, &s.Role, &s.Method, &s.ExpiresAt)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &s, err
}

func (a *Auth) issue(w http.ResponseWriter, r *http.Request, s *Session) error {
	token := randomToken()
	s.ExpiresAt = time.Now().Add(a.ttl)
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		if err := tx.QueryRow(r.Context(), `INSERT INTO mes.session (org_id, token_hash, user_id, full_name, employee_code, role, method, expires_at)
			VALUES (mes.current_org(), $1, $2, $3, $4, $5, $6, $7) RETURNING id::text`,
			hashToken(token), s.User, s.FullName, s.EmployeeCode, s.Role, s.Method, s.ExpiresAt).Scan(&s.ID); err != nil {
			return err
		}
		return audit(r.Context(), tx, s.Label(), "session.start", "session", s.ID, map[string]string{"method": s.Method, "role": s.Role})
	})
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Expires: s.ExpiresAt, Secure: r.TLS != nil})
	return nil
}

func (a *Auth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /auth/pin", a.pinLogin)
	mux.HandleFunc("GET /auth/oidc/start", a.oidcStart)
	mux.HandleFunc("POST /auth/oidc/callback", a.oidcCallback)
	mux.HandleFunc("GET /auth/me", a.Require(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, sessionFrom(r))
	}))
	mux.HandleFunc("POST /auth/logout", a.logout)
}

type accessAnswer struct {
	OK           bool     `json:"ok"`
	Granted      bool     `json:"granted"`
	User         string   `json:"user"`
	FullName     string   `json:"full_name"`
	EmployeeCode string   `json:"employee_code"`
	Role         string   `json:"role"`
	Reason       string   `json:"reason"`
	ChangedAt    string   `json:"changed_at"`
	Scopes       []string `json:"scopes"`
}

func (a *Auth) pinLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EmployeeCode string `json:"employee_code"`
		PIN          string `json:"pin"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	var ans accessAnswer
	err := a.access.PostMethod(r.Context(), "kgarden.access.api.verify_pin",
		map[string]any{"app_code": appCode, "employee_code": req.EmployeeCode, "pin": req.PIN}, &ans)
	var ee *erpnext.Error
	if errors.As(err, &ee) && ee.Status == http.StatusTooManyRequests {
		writeErr(w, fail(429, "ลองผิดหลายครั้งเกินไป รอสักครู่แล้วลองใหม่"))
		return
	}
	if err != nil {
		slog.Error("verify pin", "err", err)
		writeErr(w, fail(502, "ตรวจสอบกับ ERPNext ไม่ได้ ลองใหม่อีกครั้ง"))
		return
	}
	if !ans.OK {
		writeErr(w, fail(401, "%s", ans.Reason))
		return
	}
	s := &Session{User: ans.User, FullName: ans.FullName, EmployeeCode: ans.EmployeeCode, Role: ans.Role, Method: "pin"}
	if err := a.issue(w, r, s); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s)
}

func (a *Auth) oidcStart(w http.ResponseWriter, r *http.Request) {
	state := randomToken()
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		if _, err := tx.Exec(r.Context(), `DELETE FROM mes.oidc_state WHERE created_at < now() - interval '15 minutes'`); err != nil {
			return err
		}
		_, err := tx.Exec(r.Context(), `INSERT INTO mes.oidc_state (org_id, state) VALUES (mes.current_org(), $1)`, state)
		return err
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	q := url.Values{"client_id": {a.oidc.ClientID}, "response_type": {"code"}, "scope": {"openid all"},
		"redirect_uri": {a.oidc.RedirectURI}, "state": {state}}
	writeJSON(w, 200, map[string]string{"url": a.oidc.PublicBase + "/api/method/frappe.integrations.oauth2.authorize?" + q.Encode()})
}

func (a *Auth) oidcCallback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	var found bool
	err := a.store.Tx(r.Context(), func(tx pgx.Tx) error {
		tag, err := tx.Exec(r.Context(), `DELETE FROM mes.oidc_state WHERE state = $1 AND created_at > now() - interval '15 minutes'`, req.State)
		found = tag.RowsAffected() == 1
		return err
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	if !found || req.Code == "" {
		writeErr(w, fail(400, "การเข้าสู่ระบบหมดอายุหรือไม่ถูกต้อง ลองใหม่อีกครั้ง"))
		return
	}
	token, err := a.exchangeCode(r.Context(), req.Code)
	if err != nil {
		slog.Warn("oidc code exchange", "err", err)
		writeErr(w, fail(401, "ERPNext ไม่ยืนยันการเข้าสู่ระบบ ลองใหม่อีกครั้ง"))
		return
	}
	info, err := a.userinfo(r.Context(), token)
	if err != nil {
		slog.Warn("oidc userinfo", "err", err)
		writeErr(w, fail(401, "อ่านข้อมูลผู้ใช้จาก ERPNext ไม่ได้"))
		return
	}
	if info.App != appCode || !info.Access || info.Role == "" {
		writeErr(w, fail(403, "บัญชีนี้ไม่มีสิทธิ์ใช้แท็บเล็ตหน้างานผลิต"))
		return
	}
	s := &Session{User: info.Email, FullName: info.Name, EmployeeCode: info.EmployeeCode, Role: info.Role, Method: "sso"}
	if err := a.issue(w, r, s); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, s)
}

func (a *Auth) exchangeCode(ctx context.Context, code string) (string, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {a.oidc.RedirectURI},
		"client_id": {a.oidc.ClientID}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		a.access.Base()+"/api/method/frappe.integrations.oauth2.get_token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(a.oidc.ClientID, a.oidc.ClientSecret)
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 || json.Unmarshal(body, &out) != nil || out.AccessToken == "" {
		return "", fmt.Errorf("token endpoint %d: %.200s", resp.StatusCode, body)
	}
	return out.AccessToken, nil
}

type userinfo struct {
	Email        string `json:"email"`
	Name         string `json:"name"`
	App          string `json:"kg_app"`
	Access       bool   `json:"kg_access"`
	Role         string `json:"kg_role"`
	EmployeeCode string `json:"kg_employee_code"`
}

func (a *Auth) userinfo(ctx context.Context, token string) (*userinfo, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		a.access.Base()+"/api/method/frappe.integrations.oauth2.openid_profile", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var u userinfo
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 || json.Unmarshal(body, &u) != nil || u.Email == "" {
		return nil, fmt.Errorf("userinfo %d: %.200s", resp.StatusCode, body)
	}
	return &u, nil
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if s, err := a.lookup(r); err == nil && s != nil {
		_ = a.store.Tx(r.Context(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(r.Context(), `UPDATE mes.session SET revoked_at = now(), revoke_reason = 'logout' WHERE id = $1`, s.ID); err != nil {
				return err
			}
			return audit(r.Context(), tx, s.Label(), "session.end", "session", s.ID, map[string]string{"reason": "logout"})
		})
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// WatchAccess ends sessions of people whose MES access changed in ERPNext: lost it, disabled, or a
// different role (they sign in again and get the new one).
func (a *Auth) WatchAccess(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := a.checkAccess(ctx); err != nil {
			slog.Warn("access changes", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *Auth) checkAccess(ctx context.Context) error {
	var since string
	err := a.store.Tx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT checked_since FROM mes.access_cursor`).Scan(&since)
		if errors.Is(err, pgx.ErrNoRows) {
			// First run: everything changed in the last day (ERPNext local time).
			since = time.Now().In(bangkok).Add(-24 * time.Hour).Format(time.DateTime)
			_, err = tx.Exec(ctx, `INSERT INTO mes.access_cursor (org_id, checked_since) VALUES (mes.current_org(), $1)`, since)
		}
		return err
	})
	if err != nil {
		return err
	}
	var changes []accessAnswer
	if err := a.access.GetMethod(ctx, "kgarden.access.api.access_changes",
		url.Values{"app_code": {appCode}, "since": {since}}, &changes); err != nil {
		return err
	}
	if len(changes) == 0 {
		return nil
	}
	latest := since
	return a.store.Tx(ctx, func(tx pgx.Tx) error {
		for _, c := range changes {
			if c.ChangedAt > latest {
				latest = c.ChangedAt[:min(len(c.ChangedAt), 19)]
			}
			var tag interface{ RowsAffected() int64 }
			var err error
			if !c.Granted {
				tag, err = tx.Exec(ctx, `UPDATE mes.session SET revoked_at = now(), revoke_reason = 'access removed in ERPNext'
					WHERE user_id = $1 AND revoked_at IS NULL`, c.User)
			} else {
				tag, err = tx.Exec(ctx, `UPDATE mes.session SET revoked_at = now(), revoke_reason = 'role changed in ERPNext'
					WHERE user_id = $1 AND revoked_at IS NULL AND role <> $2`, c.User, c.Role)
			}
			if err != nil {
				return err
			}
			if n := tag.RowsAffected(); n > 0 {
				slog.Info("sessions ended by ERPNext access change", "user", c.User, "granted", c.Granted, "sessions", n)
				if err := audit(ctx, tx, "erpnext", "session.revoked", "user", c.User,
					map[string]any{"granted": c.Granted, "role": c.Role, "sessions": n}); err != nil {
					return err
				}
			}
		}
		// Keep the cursor on the latest change seen: the next read repeats that second, harmlessly.
		_, err := tx.Exec(ctx, `UPDATE mes.access_cursor SET checked_since = $1`, latest)
		return err
	})
}

var bangkok = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Bangkok"); err == nil {
		return loc
	}
	return time.FixedZone("ICT", 7*3600)
}()
