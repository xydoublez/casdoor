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
	"fmt"
	"sort"
	"testing"
)

// LDAP batch syncs stamp one created_time on thousands of users, so paginated
// user queries sort a large tie group. GetSessionForUser appends an owner,name
// tiebreaker for exactly that: without a deterministic order, the tied rows may
// line up differently in two queries and a row can fall out of every page.
func TestPaginationUsersTieBreaker(t *testing.T) {
	owner := "tie-" + randomSuffix(t)
	setupTieSortTest(t, owner)

	const (
		total    = 120
		pageSize = 50
	)
	tieTime := "2026-01-01T00:00:00+08:00"

	// inserted in reverse name order on purpose: the physical row order then
	// disagrees with the tiebreaker order, so an unsorted walk exposes a missing
	// tiebreaker instead of passing by coincidence
	users := make([]*User, 0, total)
	for i := total - 1; i >= 0; i-- {
		users = append(users, &User{
			Owner:       owner,
			Name:        fmt.Sprintf("tie-%03d", i),
			DisplayName: fmt.Sprintf("Tie %d", i),
			CreatedTime: tieTime,
		})
	}
	_, err := ormer.Engine.Insert(&users)
	mustErr(t, err)

	// walk every page: no gaps, no duplicates between pages
	seen := map[string]int{}
	var walked []string
	for offset := 0; offset < total; offset += pageSize {
		page, err := GetPaginationUsers(owner, offset, pageSize, "", "", "", "", "")
		mustErr(t, err)
		want := pageSize
		if rest := total - offset; rest < want {
			want = rest
		}
		if len(page) != want {
			t.Fatalf("page at offset %d: got %d users, want %d", offset, len(page), want)
		}
		for _, u := range page {
			seen[u.Name]++
			walked = append(walked, u.Name)
		}
	}
	if len(seen) != total {
		t.Fatalf("pages covered %d distinct users, want %d", len(seen), total)
	}
	for name, n := range seen {
		if n != 1 {
			t.Fatalf("user %s appeared on %d pages", name, n)
		}
	}

	// every created_time is equal, so the tiebreaker (owner, name) must order the walk
	if !sort.StringsAreSorted(walked) {
		t.Fatalf("tied rows are not in tiebreaker order: %v", walked)
	}

	// the same page twice must come back in the same sequence
	first, err := GetPaginationUsers(owner, pageSize, pageSize, "", "", "", "", "")
	mustErr(t, err)
	again, err := GetPaginationUsers(owner, pageSize, pageSize, "", "", "", "", "")
	mustErr(t, err)
	if len(first) != len(again) {
		t.Fatalf("page size drifted: %d then %d", len(first), len(again))
	}
	for i := range first {
		if first[i].Name != again[i].Name {
			t.Fatalf("page at offset %d drifted: %s then %s", pageSize, first[i].Name, again[i].Name)
		}
	}

	// the search path paginates through the same session: matches page without gaps too
	var searched []string
	for offset := 0; offset < total; offset += 60 {
		page, err := GetPaginationUsers(owner, offset, 60, "name", "tie-", "", "", "")
		mustErr(t, err)
		for _, u := range page {
			searched = append(searched, u.Name)
		}
	}
	if len(searched) != total {
		t.Fatalf("search pages covered %d users, want %d", len(searched), total)
	}
}

// setupTieSortTest only needs the database and a throwaway owner: the rows are
// inserted straight into the user table, and cleanup deletes just those.
func setupTieSortTest(t *testing.T, owner string) {
	t.Helper()
	InitConfig()
	t.Cleanup(func() {
		_, _ = ormer.Engine.Where("owner = ?", owner).Delete(&User{})
	})
}
