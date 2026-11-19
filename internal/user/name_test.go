package user

import (
	"errors"
	osuser "os/user"
	"strings"
	"testing"
)

func TestCurrentNameReturnsCurrentUserErrors(t *testing.T) {
	lookupErr := errors.New("identity service unavailable")
	setCurrentUserLookup(t, func() (*osuser.User, error) {
		return nil, lookupErr
	})

	_, err := CurrentName()
	if !errors.Is(err, lookupErr) {
		t.Fatalf("CurrentName error = %v, want wrapped lookup error", err)
	}
}

func TestCurrentNameReturnsCurrentUserName(t *testing.T) {
	setCurrentUserLookup(t, func() (*osuser.User, error) {
		return &osuser.User{Username: "test-user"}, nil
	})

	name, err := CurrentName()
	if err != nil {
		t.Fatalf("CurrentName error = %v", err)
	}
	if name != "test-user" {
		t.Fatalf("CurrentName = %q, want test-user", name)
	}
}

func TestCurrentNameRejectsEmptyUserName(t *testing.T) {
	setCurrentUserLookup(t, func() (*osuser.User, error) {
		return &osuser.User{}, nil
	})

	if _, err := CurrentName(); err == nil || !strings.Contains(err.Error(), "empty user name") {
		t.Fatalf("CurrentName error = %v, want empty user name", err)
	}
}

func TestNameRetainsStringCompatibilityWithoutPanicking(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		setCurrentUserLookup(t, func() (*osuser.User, error) {
			return &osuser.User{Username: "test-user"}, nil
		})
		if got := Name(); got != "test-user" {
			t.Fatalf("Name = %q, want test-user", got)
		}
	})

	t.Run("lookup failure", func(t *testing.T) {
		setCurrentUserLookup(t, func() (*osuser.User, error) {
			return nil, errors.New("identity service unavailable")
		})
		if got := Name(); got != "" {
			t.Fatalf("Name = %q after lookup failure, want empty string", got)
		}
	})
}

func TestNoRootCheckReturnsRootIdentityErrors(t *testing.T) {
	tests := []struct {
		name string
		user *osuser.User
		want string
	}{
		{name: "UID", user: &osuser.User{Uid: "0", Gid: "1000"}, want: "UID 0"},
		{name: "GID", user: &osuser.User{Uid: "1000", Gid: "0"}, want: "GID 0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setCurrentUserLookup(t, func() (*osuser.User, error) { return test.user, nil })
			if err := NoRootCheck(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NoRootCheck error = %v, want %q", err, test.want)
			}
		})
	}
}

func setCurrentUserLookup(t *testing.T, lookup func() (*osuser.User, error)) {
	t.Helper()
	original := current
	current = lookup
	t.Cleanup(func() { current = original })
}
