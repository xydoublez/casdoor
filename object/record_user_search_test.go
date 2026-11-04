// Copyright 2026 The Casdoor Authors. All Rights Reserved.
//
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

package object

import (
	"testing"

	"github.com/casdoor/casdoor/util"
)

// The audit log (record) table has a "user" column, and "user" is a reserved
// word in PostgreSQL: an unquoted `user like ?` compares the session user
// instead of the column, so searching the log page by user name silently
// returned zero rows. GetSession builds raw SQL conditions, so it must quote
// the column name.
func TestRecordSearchByUser(t *testing.T) {
	InitConfig()

	owner := "rec-" + randomSuffix(t)
	record := &Record{
		Owner:        owner,
		Name:         util.GenerateId(),
		CreatedTime:  util.GetCurrentTime(),
		Organization: owner,
		User:         "yaozhen2",
		Method:       "GET",
		Action:       "login",
	}
	_, err := ormer.Engine.Insert(record)
	mustErr(t, err)
	t.Cleanup(func() {
		_, _ = ormer.Engine.Where("owner = ?", owner).Delete(&Record{})
	})

	filter := &Record{Organization: owner}

	count, err := GetRecordCount("user", "yaozhen2", filter)
	mustErr(t, err)
	if count != 1 {
		t.Fatalf("record count by user: got %d, want 1", count)
	}

	records, err := GetPaginationRecords(0, 10, "user", "yaozhen2", "", "", filter)
	mustErr(t, err)
	if len(records) != 1 || records[0].User != "yaozhen2" {
		t.Fatalf("paginated records by user: got %d rows, want exactly the inserted one", len(records))
	}

	count, err = GetRecordCount("user", "nosuchuser", filter)
	mustErr(t, err)
	if count != 0 {
		t.Fatalf("record count for absent user: got %d, want 0", count)
	}
}
