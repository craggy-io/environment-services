package adb_test

import "time"

func deadline() time.Time   { return time.Now().Add(2 * time.Second) }
func noDeadline() time.Time { return time.Time{} }
