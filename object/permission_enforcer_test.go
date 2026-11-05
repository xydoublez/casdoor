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
	"reflect"
	"testing"
)

func TestGetPoliciesIncludesGroups(t *testing.T) {
	permission := &Permission{
		Owner:     "org",
		Name:      "perm",
		Users:     []string{"org/alice"},
		Groups:    []string{"org/dev"},
		Roles:     []string{"org/admin"},
		Resources: []string{"data1"},
		Actions:   []string{"read"},
		Effect:    "Allow",
	}

	got := getPolicies(permission)
	want := [][]string{
		{"org/alice", "data1", "read", "allow", "", "org/perm"},
		{"group:org/dev", "data1", "read", "allow", "", "org/perm"},
		{"org/admin", "data1", "read", "allow", "", "org/perm"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("getPolicies() = %#v, want %#v", got, want)
	}
}

func TestGetPoliciesIncludesGroupsWithDomains(t *testing.T) {
	permission := &Permission{
		Owner:     "org",
		Name:      "perm",
		Groups:    []string{"org/dev"},
		Domains:   []string{"domain1"},
		Resources: []string{"data1"},
		Actions:   []string{"read"},
		Effect:    "Deny",
	}

	got := getPolicies(permission)
	want := [][]string{
		{"group:org/dev", "domain1", "data1", "read", "deny", "org/perm"},
	}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("getPolicies() = %#v, want %#v", got, want)
	}
}

// TestDeletePermissionWithPartialObjectRemovesPolicies — /api/delete-permission forwards whatever
// the client posted, and an API client commonly posts only {owner, name}. The Casbin rules are
// derived from the object, so deleting with a partial one used to leave them behind, where they
// keep matching for the enforcers sharing the model and adapter.
func TestDeletePermissionWithPartialObjectRemovesPolicies(t *testing.T) {
	owner := "perm-del-" + randomSuffix(t)
	setupGroupAuthTest(t, owner)

	permission := makeAllowPermission(owner, "perm-1", []string{owner + "/dev"})
	addPermission(t, permission)

	id := permission.GetId()
	if count := countPermissionPolicies(t, id); count == 0 {
		t.Fatalf("AddPermission(%s) wrote no Casbin policy", id)
	}

	affected, err := DeletePermission(&Permission{Owner: owner, Name: permission.Name})
	mustErr(t, err)
	if !affected {
		t.Fatalf("DeletePermission(%s) not affected", id)
	}

	if count := countPermissionPolicies(t, id); count != 0 {
		t.Fatalf("%d Casbin policies of %s survived the deletion", count, id)
	}
}

func countPermissionPolicies(t *testing.T, permissionId string) int64 {
	t.Helper()

	count, err := ormer.Engine.Table("permission_rule").Where("v5 = ?", permissionId).Count()
	mustErr(t, err)
	return count
}
