// Copyright 2026 Google LLC
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

package main

import (
	"context"
	"testing"
)

func TestCloudControllerManager(t *testing.T) {
	if cfg.CAFile == "" || cfg.SAFile == "" {
		t.Skip("skipping integration test: -cafile or -service_account not provided")
	}

	ctx := context.Background()
	t.Logf("Starting Cloud Controller Manager presubmit integration test for commit %q", cfg.CommitHash)

	env := setupTestEnvironment(ctx, t)

	t.Run("NodeController", func(t *testing.T) {
		runNodeControllerTests(ctx, t, env)
	})

	t.Run("LoadBalancerController", func(t *testing.T) {
		runLoadBalancerTests(ctx, t, env)
	})
}
