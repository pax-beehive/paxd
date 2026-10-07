package paxlinstall

import "errors"

func lockExecutable(string) (func(), error) {
	return nil, errors.New("remote paxl upgrade is not supported on Windows")
}
