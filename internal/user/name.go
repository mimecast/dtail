package user

import (
	"os/user"
  )


func Name() string {
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

	return user.Username
}

