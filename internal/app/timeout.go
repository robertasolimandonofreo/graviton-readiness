package app

import (
	"fmt"
	"time"
)

func parseTimeout(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid --timeout %q (use a duration such as 20s)", s)
	}
	return d, nil
}
