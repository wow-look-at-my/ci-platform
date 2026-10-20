package chaos

import "time"

// epoch is a fixed base.
var epoch = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

func timeAt(seconds int) time.Time { return epoch.Add(time.Duration(seconds) * time.Second) }

func ptr(t time.Time) *time.Time { return &t }
