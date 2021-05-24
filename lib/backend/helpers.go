/*
Copyright 2018 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package backend

import (
	"context"
	"path/filepath"
	"time"

	"github.com/gravitational/trace"
)

const locksPrefix = ".locks"

// AcquireLock grabs a lock that will be released automatically in TTL
func AcquireLock(ctx context.Context, backend Backend, lockName string, ttl time.Duration) (err error) {
	if lockName == "" {
		return trace.BadParameter("missing parameter lock name")
	}
	key := []byte(filepath.Join(locksPrefix, lockName))
	for {
		// Get will clear TTL on a lock
		backend.Get(ctx, key)

		// CreateVal is atomic:
		_, err = backend.Create(ctx, Item{Key: key, Value: []byte{1}, Expires: backend.Clock().Now().UTC().Add(ttl)})
		if err == nil {
			break // success
		}
		if trace.IsAlreadyExists(err) { // locked? wait and repeat:
			backend.Clock().Sleep(250 * time.Millisecond)
			continue
		}
		return trace.ConvertSystemError(err)
	}
	return nil
}

// ReleaseLock forces lock release
func ReleaseLock(ctx context.Context, backend Backend, lockName string) error {
	if lockName == "" {
		return trace.BadParameter("missing parameter lockName")
	}
	key := []byte(filepath.Join(locksPrefix, lockName))
	if err := backend.Delete(ctx, key); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// ResetLockTTL resets the TTL on a given lock.
func ResetLockTTL(ctx context.Context, backend Backend, lockName string) error {
	if lockName == "" {
		return trace.BadParameter("missing parameter lock name")
	}
	key := []byte(filepath.Join(locksPrefix, lockName))
	if _, err := backend.Get(ctx, key); err != nil {
		return trace.Wrap(err)
	}

	return nil
}

// RunWhileLocked allows you to run a function while a lock is held.
func RunWhileLocked(ctx context.Context, backend Backend, lockName string, ttl time.Duration, fn func() error) error {
	if err := AcquireLock(ctx, backend, lockName, ttl); err != nil {
		return trace.Wrap(err)
	}

	fnErr := fn()

	if err := ReleaseLock(ctx, backend, lockName); err != nil {
		return trace.NewAggregate(fnErr, err)
	}

	return fnErr
}
