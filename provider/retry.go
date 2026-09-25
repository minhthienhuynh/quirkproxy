package provider

import (
	"context"
	"time"
)

// Retry runs fn up to attempts times with exponential backoff starting at
// base. It stops early when ctx is done and returns the last error.
func Retry(ctx context.Context, attempts int, base time.Duration, fn func(context.Context) error) error {
	if attempts <= 0 {
		return nil
	}
	var err error
	wait := base
	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err = fn(ctx); err == nil {
			return nil
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			wait *= 2
		}
	}
	return err
}
