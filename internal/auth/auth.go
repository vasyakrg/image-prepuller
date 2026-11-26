package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const CookieName = "prepuller_session"

type Config struct {
	Username      string
	Password      string
	SessionSecret []byte
	TTL           time.Duration
}

type Manager struct {
	user   string
	pass   string
	secret []byte
	ttl    time.Duration
}

func NewManager(cfg Config) (*Manager, error) {
	if strings.TrimSpace(cfg.Username) == "" || cfg.Password == "" {
		return nil, errors.New("PREPULLER_AUTH_USER and PREPULLER_AUTH_PASS must be set")
	}
	secret := cfg.SessionSecret
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, fmt.Errorf("generate session secret: %w", err)
		}
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Manager{
		user:   cfg.Username,
		pass:   cfg.Password,
		secret: secret,
		ttl:    ttl,
	}, nil
}

func (m *Manager) ValidateCredentials(username, password string) bool {
	uOK := subtle.ConstantTimeCompare([]byte(username), []byte(m.user)) == 1
	pOK := subtle.ConstantTimeCompare([]byte(password), []byte(m.pass)) == 1
	return uOK && pOK
}

func (m *Manager) IssueCookie(w http.ResponseWriter, r *http.Request, username string) {
	exp := time.Now().Add(m.ttl).Unix()
	payload := fmt.Sprintf("%s|%d", base64.RawURLEncoding.EncodeToString([]byte(username)), exp)
	sig := m.sign(payload)
	token := payload + "." + sig

	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(m.ttl.Seconds()),
	})
}

func (m *Manager) ClearCookie(w http.ResponseWriter, r *http.Request) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   -1,
	})
}

func (m *Manager) AuthenticateRequest(r *http.Request) (string, bool) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 2 {
		return "", false
	}
	payload, gotSig := parts[0], parts[1]
	wantSig := m.sign(payload)
	if subtle.ConstantTimeCompare([]byte(gotSig), []byte(wantSig)) != 1 {
		return "", false
	}
	fields := strings.Split(payload, "|")
	if len(fields) != 2 {
		return "", false
	}
	exp, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	rawUser, err := base64.RawURLEncoding.DecodeString(fields[0])
	if err != nil {
		return "", false
	}
	return string(rawUser), true
}

func (m *Manager) sign(payload string) string {
	mac := hmac.New(sha256.New, m.secret)
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}
