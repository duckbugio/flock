package schedule_test

// Embed the IANA timezone database in the test binary so the per-chat timezone
// tests resolve names like Europe/Moscow via time.LoadLocation regardless of
// whether the host (or CI container) ships OS zoneinfo. Production binaries embed
// it via their own blank import in package main.
import _ "time/tzdata"
