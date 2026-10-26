package bridge

import "time"

// Fast shortens the runner's timings for tests.
func Fast() {
	scanEvery = 50 * time.Millisecond
	leaseTTL = 300 * time.Millisecond
	resync = 200 * time.Millisecond
	retryFirst = 50 * time.Millisecond
	retryMax = 200 * time.Millisecond
	logoutTimeout = 200 * time.Millisecond
	logoutFor = time.Second
}
