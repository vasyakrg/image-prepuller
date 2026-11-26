package auth

import (
	"strings"
	"testing"
)

func TestParseUsers(t *testing.T) {
	users, err := ParseUsers(" admin:pass:operator, view:pw2:viewer ")
	if err != nil {
		t.Fatalf("ParseUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
	if users[0].Name != "admin" || users[0].Password != "pass" || users[0].Role != RoleOperator {
		t.Fatalf("unexpected first user: %+v", users[0])
	}
	if users[1].Name != "view" || users[1].Role != RoleViewer {
		t.Fatalf("unexpected second user: %+v", users[1])
	}

	// Passwords may contain colons; the role is the suffix after the last colon.
	users, err = ParseUsers("svc:pa:ss:word:operator")
	if err != nil {
		t.Fatalf("ParseUsers with colon password: %v", err)
	}
	if users[0].Password != "pa:ss:word" || users[0].Role != RoleOperator {
		t.Fatalf("unexpected colon-password user: %+v", users[0])
	}

	if users, err := ParseUsers("   "); err != nil || users != nil {
		t.Fatalf("empty input must yield no users, got %v/%v", users, err)
	}
}

func TestParseUsersErrorsDoNotLeakPasswords(t *testing.T) {
	const secret = "hunter2-secret"
	tests := map[string]string{
		"missing role":   "admin:" + secret,
		"unknown role":   "admin:" + secret + ":root",
		"empty password": "admin::operator",
		"duplicate user": "admin:" + secret + ":operator,admin:other:viewer",
		"no colon":       secret,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseUsers(raw)
			if err == nil {
				t.Fatal("expected parse error")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked the password: %v", err)
			}
		})
	}
}

func TestParseUsersFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    []User
		wantErr bool
	}{
		{
			name: "users only",
			env:  map[string]string{EnvUsers: "admin:pass:operator,view:pw:viewer"},
			want: []User{{Name: "admin", Password: "pass", Role: RoleOperator}, {Name: "view", Password: "pw", Role: RoleViewer}},
		},
		{
			name: "legacy only maps to operator",
			env:  map[string]string{EnvLegacyUser: "admin", EnvLegacyPass: "pass"},
			want: []User{{Name: "admin", Password: "pass", Role: RoleOperator}},
		},
		{
			name: "users plus legacy",
			env:  map[string]string{EnvUsers: "view:pw:viewer", EnvLegacyUser: "admin", EnvLegacyPass: "pass"},
			want: []User{{Name: "view", Password: "pw", Role: RoleViewer}, {Name: "admin", Password: "pass", Role: RoleOperator}},
		},
		{
			name: "legacy duplicate keeps explicit role",
			env:  map[string]string{EnvUsers: "admin:pw:viewer", EnvLegacyUser: "admin", EnvLegacyPass: "pass"},
			want: []User{{Name: "admin", Password: "pw", Role: RoleViewer}},
		},
		{name: "legacy user without password", env: map[string]string{EnvLegacyUser: "admin"}, wantErr: true},
		{name: "legacy password without user", env: map[string]string{EnvLegacyPass: "pass"}, wantErr: true},
		{name: "nothing configured", env: map[string]string{}, wantErr: true},
		{name: "invalid users", env: map[string]string{EnvUsers: "admin:pass"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string { return tc.env[key] }
			users, err := ParseUsersFromEnv(getenv)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got users %+v", users)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseUsersFromEnv: %v", err)
			}
			if len(users) != len(tc.want) {
				t.Fatalf("expected %d users, got %d: %+v", len(tc.want), len(users), users)
			}
			for i := range users {
				if users[i] != tc.want[i] {
					t.Fatalf("user %d = %+v, want %+v", i, users[i], tc.want[i])
				}
			}
		})
	}
}

func TestUsersFromEnvToManager(t *testing.T) {
	getenv := func(key string) string {
		return map[string]string{
			EnvUsers:      "ops:opspass:operator,auditor:audpass:viewer",
			EnvLegacyUser: "admin",
			EnvLegacyPass: "adminpass",
		}[key]
	}
	users, err := ParseUsersFromEnv(getenv)
	if err != nil {
		t.Fatalf("ParseUsersFromEnv: %v", err)
	}
	m, err := NewManagerFromConfig("secret", users)
	if err != nil {
		t.Fatalf("NewManagerFromConfig: %v", err)
	}
	defer func() { _ = m.Close() }()
	for _, tc := range []struct {
		user string
		pass string
		role Role
	}{
		{"ops", "opspass", RoleOperator},
		{"auditor", "audpass", RoleViewer},
		{"admin", "adminpass", RoleOperator},
	} {
		sess, ok := m.Login(nil, tc.user, tc.pass)
		if !ok || sess.Role != tc.role {
			t.Fatalf("%s: login=%v role=%q, want %q", tc.user, ok, sess.Role, tc.role)
		}
	}
}
