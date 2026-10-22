package cronjob

import (
	"errors"
)

func lockScheduler(string) (func(), error) {
	return nil, errors.New("cron execution is not supported on Windows yet")
}
