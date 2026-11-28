// Package retry is a throwaway file to test posting review comments from next-gen-review.
// This PR will be closed without merging.
package retry

import "time"

// Do calls f until it succeeds, at most n times, and waits d between tries.
func Do(n int, d time.Duration, f func() error) error {
	var err error
	for i := 0; i <= n; i++ {
		err = f()
		if err == nil {
			return nil
		}
		time.Sleep(d)
	}
	return nil
}
