// Copyright 2026 DataRobot, Inc. and its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package drapi

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"time"
)

// transportError is a failure before a response was received: DNS, connection reset, TLS.
// These are always worth retrying because no server state changed.
type transportError struct {
	Method string
	Path   string
	Err    error
}

func (e *transportError) Error() string {
	return fmt.Sprintf("%s %s: %v", e.Method, e.Path, e.Err)
}

func (e *transportError) Unwrap() error {
	return e.Err
}

// isRetryable reports whether err is worth another attempt.
//
// Every request dr-sync makes is safe to replay: reads are idempotent, a stage upload
// deduplicates a repeated name, and `fromStage` is atomic so a rejected commit changed
// nothing. That is why uploads are in scope here and not excluded as unsafe POSTs.
func isRetryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var transportErr *transportError
	if errors.As(err, &transportErr) {
		return true
	}

	switch StatusOf(err) {
	case http.StatusTooManyRequests,
		http.StatusRequestEntityTooLarge, // the API uses 413 for transient overload too
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

// retryAfterOf reads the Retry-After header, honouring both the delay-seconds and HTTP-date
// forms. It returns 0 when the header is absent or unparseable.
func retryAfterOf(resp *http.Response) time.Duration {
	value := resp.Header.Get("Retry-After")
	if value == "" {
		return 0
	}

	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}

	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}

	return 0
}

const (
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 30 * time.Second
)

// waitBeforeRetry sleeps for the server's Retry-After when it gave one, otherwise for an
// exponential backoff with full jitter. Jitter matters because dr-sync fires several
// uploads concurrently and un-jittered backoff would resynchronize them into a thundering
// herd against a rate limit.
func waitBeforeRetry(ctx context.Context, attempt int, retryAfter time.Duration) error {
	delay := retryAfter

	if delay <= 0 {
		delay = backoff(attempt)
	}

	if delay > retryMaxDelay {
		delay = retryMaxDelay
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// backoff returns a full-jitter exponential delay for the given zero-based attempt.
func backoff(attempt int) time.Duration {
	ceiling := retryBaseDelay << min(attempt, 6)
	if ceiling > retryMaxDelay {
		ceiling = retryMaxDelay
	}

	jittered, err := rand.Int(rand.Reader, big.NewInt(int64(ceiling)))
	if err != nil {
		return ceiling
	}

	// Keep a floor so a near-zero draw does not turn the backoff into a busy loop.
	return retryBaseDelay/2 + time.Duration(jittered.Int64())
}
