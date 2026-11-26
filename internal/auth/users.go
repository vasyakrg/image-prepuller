package auth

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Environment variables understood by UsersFromEnv.
const (
	EnvUsers      = "PREPULLER_USERS"
	EnvLegacyUser = "PREPULLER_AUTH_USER"
	EnvLegacyPass = "PREPULLER_AUTH_PASS"
)

// User is one configured account. Passwords are kept in memory only and are
// never logged or returned by any API.
type User struct {
	Name     string
	Password string
	Role     Role
}

// ParseUsers parses "name:password:role,..." entries. Passwords may contain
// colons: the role is taken after the last colon, the name before the first.
func ParseUsers(raw string) ([]User, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var users []User
	seen := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, rest, ok := strings.Cut(item, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid %s entry %q: expected name:password:role", EnvUsers, redactUserEntry(item))
		}
		idx := strings.LastIndex(rest, ":")
		if idx < 0 {
			return nil, fmt.Errorf("invalid %s entry %q: role is required (name:password:role)", EnvUsers, redactUserEntry(item))
		}
		password, roleStr := rest[:idx], rest[idx+1:]
		if password == "" {
			return nil, fmt.Errorf("invalid %s entry %q: password must not be empty", EnvUsers, redactUserEntry(item))
		}
		role, err := ParseRole(roleStr)
		if err != nil {
			return nil, fmt.Errorf("invalid %s entry %q: %w", EnvUsers, redactUserEntry(item), err)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("duplicate user %q in %s", name, EnvUsers)
		}
		seen[name] = struct{}{}
		users = append(users, User{Name: name, Password: password, Role: role})
	}
	return users, nil
}

// redactUserEntry returns an error-safe rendering of a malformed entry: the
// password part is never echoed back.
func redactUserEntry(item string) string {
	name, _, ok := strings.Cut(item, ":")
	name = strings.TrimSpace(name)
	if !ok || name == "" {
		return "<redacted>"
	}
	return name + ":***"
}

// UsersFromEnv builds the user list from PREPULLER_USERS plus the legacy
// PREPULLER_AUTH_USER/PREPULLER_AUTH_PASS pair (operator role).
func UsersFromEnv() ([]User, error) {
	return ParseUsersFromEnv(os.Getenv)
}

// ParseUsersFromEnv is UsersFromEnv with an injectable lookup for tests.
func ParseUsersFromEnv(getenv func(string) string) ([]User, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	users, err := ParseUsers(getenv(EnvUsers))
	if err != nil {
		return nil, err
	}
	legacyUser := strings.TrimSpace(getenv(EnvLegacyUser))
	legacyPass := getenv(EnvLegacyPass)
	switch {
	case legacyUser != "" && legacyPass == "":
		return nil, errors.New("PREPULLER_AUTH_USER is set but PREPULLER_AUTH_PASS is empty")
	case legacyUser == "" && legacyPass != "":
		return nil, errors.New("PREPULLER_AUTH_PASS is set but PREPULLER_AUTH_USER is empty")
	case legacyUser != "":
		if !hasUser(users, legacyUser) {
			users = append(users, User{Name: legacyUser, Password: legacyPass, Role: RoleOperator})
		}
	}
	if len(users) == 0 {
		return nil, errors.New("no users configured: set PREPULLER_USERS or PREPULLER_AUTH_USER/PREPULLER_AUTH_PASS")
	}
	return users, nil
}

func hasUser(users []User, name string) bool {
	for _, u := range users {
		if u.Name == name {
			return true
		}
	}
	return false
}
