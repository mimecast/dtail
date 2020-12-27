package permissions

/*
#include "permission_linux.h"
#cgo LDFLAGS: -L. -lacl
*/
import "C"

import (
	"errors"
	"unsafe"
)

// ToRead checks whether user has Linux file system permissions to read a given file.
func ToRead(user, filePath string) (bool, error) {
	cUser := C.CString(user)
	cFilePath := C.CString(filePath)

	defer C.free(unsafe.Pointer(cUser))
	defer C.free(unsafe.Pointer(cFilePath))

	cOk, err := C.permission_to_read(cUser, cFilePath)
	if cOk == 1 {
		return true, nil
	}

	if err != nil {
		// err contains errno message
		return false, err
	}

	return false, errors.New("User without permission to read file")
}
