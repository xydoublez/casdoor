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
	"strings"
	"testing"
)

// Real AD-synced hierarchy in the dev database (org "test"):
//
//	AI中台部 (parent, has direct members e.g. chenyipeng)
//	└── AI医疗专家组 (child, members e.g. liuyangyang, shiyu2)
//
// A permission bound only to the parent group must also authorize
// the users of the child group.
const (
	parentGroupAuthTestOwner      = "test"
	parentGroupAuthTestParent     = "AI中台部"
	parentGroupAuthTestChild      = "AI医疗专家组"
	parentGroupAuthTestUser       = "test/liuyangyang" // only in child group
	parentGroupAuthTestDirectUser = "test/chenyipeng"  // directly in parent group
)

func parentGroupAuthTestInit(t *testing.T) {
	t.Helper()

	InitConfig()
	if userEnforcer == nil {
		InitUserManager()
	}
}

func TestGetGroupIdsWithDescendantsRealData(t *testing.T) {
	parentGroupAuthTestInit(t)

	resolver := newPermissionGroupResolver()
	groupIds, err := resolver.getGroupIdsWithDescendants(parentGroupAuthTestOwner, parentGroupAuthTestParent)
	if err != nil {
		t.Fatalf("getGroupIdsWithDescendants() error: %v", err)
	}

	t.Logf("descendants of %s: %v", parentGroupAuthTestParent, groupIds)

	found := false
	for _, groupId := range groupIds {
		if groupId == parentGroupAuthTestOwner+"/"+parentGroupAuthTestChild {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("descendants of %s do not contain child group %s", parentGroupAuthTestParent, parentGroupAuthTestChild)
	}
}

func TestGetGroupIdsWithAncestorsRealData(t *testing.T) {
	parentGroupAuthTestInit(t)

	groupIds, err := GetGroupIdsWithAncestors(parentGroupAuthTestOwner, []string{parentGroupAuthTestOwner + "/" + parentGroupAuthTestChild})
	if err != nil {
		t.Fatalf("GetGroupIdsWithAncestors() error: %v", err)
	}

	t.Logf("ancestors of %s: %v", parentGroupAuthTestChild, groupIds)

	found := false
	for _, groupId := range groupIds {
		if groupId == parentGroupAuthTestOwner+"/"+parentGroupAuthTestParent {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ancestors of %s do not contain parent group %s", parentGroupAuthTestChild, parentGroupAuthTestParent)
	}
}

func TestIsGroupHitParentGroupRealData(t *testing.T) {
	parentGroupAuthTestInit(t)

	permission := &Permission{
		Owner:  parentGroupAuthTestOwner,
		Name:   "parent-group-auth-test",
		Groups: []string{parentGroupAuthTestOwner + "/" + parentGroupAuthTestParent},
	}

	if !permission.isGroupHit(parentGroupAuthTestUser) {
		t.Fatalf("isGroupHit() = false for %s who belongs to child group %s, want true", parentGroupAuthTestUser, parentGroupAuthTestChild)
	}
}

func TestGetPermissionsAndRolesByUserParentGroupRealData(t *testing.T) {
	parentGroupAuthTestInit(t)

	permission := &Permission{
		Owner:     parentGroupAuthTestOwner,
		Name:      "parent-group-auth-test",
		Groups:    []string{parentGroupAuthTestOwner + "/" + parentGroupAuthTestParent},
		Resources: []string{"test-api"},
		Actions:   []string{"GET"},
		Effect:    "Allow",
		IsEnabled: true,
		State:     "Approved",
	}
	_, err := AddPermission(permission)
	if err != nil {
		t.Fatalf("AddPermission() error: %v", err)
	}
	defer func() {
		_, err := DeletePermission(permission)
		if err != nil {
			t.Logf("cleanup DeletePermission() error: %v", err)
		}
	}()

	permissions, _, err := getPermissionsAndRolesByUser(parentGroupAuthTestUser)
	if err != nil {
		t.Fatalf("getPermissionsAndRolesByUser() error: %v", err)
	}

	found := false
	for _, perm := range permissions {
		if perm.GetId() == permission.GetId() {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("getPermissionsAndRolesByUser() does not return the parent-group permission for %s who belongs to child group %s", parentGroupAuthTestUser, parentGroupAuthTestChild)
	}
	t.Logf("getPermissionsAndRolesByUser() returns the parent-group permission for %s", parentGroupAuthTestUser)
}

func TestEnforceParentGroupRealData(t *testing.T) {
	parentGroupAuthTestInit(t)

	permission := &Permission{
		Owner:     parentGroupAuthTestOwner,
		Name:      "parent-group-auth-test",
		Groups:    []string{parentGroupAuthTestOwner + "/" + parentGroupAuthTestParent},
		Resources: []string{"test-api"},
		Actions:   []string{"GET"},
		Effect:    "Allow",
		IsEnabled: true,
		State:     "Approved",
	}
	_, err := AddPermission(permission)
	if err != nil {
		t.Fatalf("AddPermission() error: %v", err)
	}
	defer func() {
		_, err := DeletePermission(permission)
		if err != nil {
			t.Logf("cleanup DeletePermission() error: %v", err)
		}
	}()

	for _, userId := range []string{parentGroupAuthTestUser, parentGroupAuthTestDirectUser} {
		allowed, err := Enforce(permission, []interface{}{userId, "test-api", "GET"})
		if err != nil {
			t.Fatalf("Enforce() for %s error: %v", userId, err)
		}
		if !allowed {
			t.Fatalf("Enforce() = false for %s (parent group: %s), want true", userId, parentGroupAuthTestParent)
		}
		t.Logf("Enforce() = true for %s", userId)
	}

	denied, err := Enforce(permission, []interface{}{parentGroupAuthTestUser, "other-api", "GET"})
	if err != nil {
		t.Fatalf("Enforce() error: %v", err)
	}
	if denied {
		t.Fatalf("Enforce() = true for an unrelated resource, want false")
	}
}

func TestCheckApiPermissionParentGroupRealData(t *testing.T) {
	parentGroupAuthTestInit(t)

	permission := &Permission{
		Owner:        parentGroupAuthTestOwner,
		Name:         "parent-group-auth-test",
		Groups:       []string{parentGroupAuthTestOwner + "/" + parentGroupAuthTestParent},
		Resources:    []string{"test-api"},
		Actions:      []string{"GET"},
		Effect:       "Allow",
		ResourceType: "API",
		IsEnabled:    true,
		State:        "Approved",
	}
	_, err := AddPermission(permission)
	if err != nil {
		t.Fatalf("AddPermission() error: %v", err)
	}
	defer func() {
		_, err := DeletePermission(permission)
		if err != nil {
			t.Logf("cleanup DeletePermission() error: %v", err)
		}
	}()

	allowed, err := CheckApiPermission(parentGroupAuthTestUser, parentGroupAuthTestOwner, "/api/test-api", "GET")
	if err != nil {
		t.Fatalf("CheckApiPermission() error: %v", err)
	}
	if !allowed {
		t.Fatalf("CheckApiPermission() = false for %s via parent group %s, want true", parentGroupAuthTestUser, parentGroupAuthTestParent)
	}
	t.Logf("CheckApiPermission() = true for %s", parentGroupAuthTestUser)
}

func TestGetRolesByUserParentGroupRoleRealData(t *testing.T) {
	parentGroupAuthTestInit(t)

	role := &Role{
		Owner:  parentGroupAuthTestOwner,
		Name:   "parent-group-auth-test-role",
		Groups: []string{parentGroupAuthTestOwner + "/" + parentGroupAuthTestParent},
	}
	affected, err := AddRole(role)
	if err != nil {
		t.Fatalf("AddRole() error: %v", err)
	}
	if !affected {
		t.Fatalf("AddRole() not affected")
	}
	defer func() {
		_, err := DeleteRole(role)
		if err != nil {
			t.Logf("cleanup DeleteRole() error: %v", err)
		}
	}()

	roles, err := getRolesByUser(parentGroupAuthTestUser)
	if err != nil {
		t.Fatalf("getRolesByUser() error: %v", err)
	}

	found := false
	var roleNames []string
	for _, r := range roles {
		roleNames = append(roleNames, r.GetId())
		if r.GetId() == role.GetId() {
			found = true
		}
	}
	t.Logf("roles of %s: [%s]", parentGroupAuthTestUser, strings.Join(roleNames, ", "))
	if !found {
		t.Fatalf("getRolesByUser() does not contain role bound to parent group %s", parentGroupAuthTestParent)
	}
}
