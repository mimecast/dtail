package funcs

import (
	"crypto/md5"
	"encoding/hex"
)

// Md5Sum returns the hex encoded MD5 checksum of a given input string.
func Md5Sum(text string) string {
	hash := md5.Sum([]byte(text))
	return hex.EncodeToString(hash[:])
}
