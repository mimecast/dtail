package user

import (
	"os/user"
)

// NoRootCheck verifies that the DTail run user is not with UID or GID 0.
func NoRootCheck() {
	user, err := user.Current()
	if err != nil {
		panic(err)
	}

	if user.Uid == "0" {
		panic("Not allowed to run as UID 0")
	}

	if user.Gid == "0" {
		panic("Not allowed to run as GID 0")
	}
}

// Name of the current run user.
func Name() string {
	user, err := user.Current()
	if err != nil {
		panic(err)
	}

	return user.Username
}
