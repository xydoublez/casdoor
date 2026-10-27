// Copyright 2021 The Casdoor Authors. All Rights Reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !skipCi

package object

import (
	"fmt"
	"os"
	"testing"
)

func TestGetUsers(t *testing.T) {
	InitConfig()
	syncers, _ := GetSyncers("admin")
	if len(syncers) == 0 {
		t.Skip("no syncer owned by \"admin\" is configured, skipping")
	}
	syncer := syncers[0]
	syncer.initAdapter()
	users, _ := syncer.getOriginalUsers()
	for _, user := range users {
		fmt.Printf("%v\n", user)
	}
}

func TestSyncUsers(t *testing.T) {
	// RunSyncUsersJob() ends with an infinite sleep by design (it is a blocking
	// job entrypoint), so it can never finish under `go test`. Opt in explicitly
	// to exercise it against a database that actually has syncers:
	//   CASDOOR_TEST_SYNC_USERS=1 go test ./object/ -run TestSyncUsers -timeout 0
	if os.Getenv("CASDOOR_TEST_SYNC_USERS") == "" {
		t.Skip("RunSyncUsersJob() blocks forever by design, set CASDOOR_TEST_SYNC_USERS=1 to run it manually")
	}

	InitConfig()

	RunSyncUsersJob()
}
