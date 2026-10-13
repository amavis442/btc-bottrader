// Package platform bevat technische hulpmiddelen: klok, logging en planning.
package platform

import "time"

// SystemClock is de echte klok (UTC).
type SystemClock struct{}

// Now geeft de huidige tijd in UTC.
func (SystemClock) Now() time.Time { return time.Now().UTC() }
