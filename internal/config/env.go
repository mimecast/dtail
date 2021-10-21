package config

import "os"

func Env(env string) bool {
	return "yes" == os.Getenv(env)
}
