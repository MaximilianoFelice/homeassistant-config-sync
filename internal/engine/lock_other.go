//go:build !unix

package engine

import "errors"

func lock(string) (func(), error) {
	return nil, errors.New("the engine lock is only implemented on unix systems")
}
