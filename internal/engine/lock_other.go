//go:build !unix

package engine

import "errors"

func lock(string) (func(), error) {
	return nil, errors.New("el lock del motor solo está implementado en sistemas unix")
}
