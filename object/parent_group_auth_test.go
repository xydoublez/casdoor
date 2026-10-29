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
	"time"
)

// TestParentGroupAuthWithOUHierarchy — baseline: ParentId OU hierarchy still works.
func TestParentGroupAuthWithOUHierarchy(t *testing.T) {
	owner := "pg-ou-" + randomSuffix(t)
	setupGroupAuthTest(t, owner)

	parentGroup := &Group{Owner: owner, Name: "parent", IsTopGroup: true, Type: "test"}
	addGroup(t, parentGroup)
	childGroup := &Group{Owner: owner, Name: "child", ParentId: "parent", Type: "test"}
	addGroup(t, childGroup)

	user := addTestUser(t, owner, "child-user", []string{owner + "/child"})

	perm := makeAllowPermission(owner, "perm-parent", []string{owner + "/parent"})
	addPermission(t, perm)
	defer cleanUpPermission(t, perm)

	checkParentGroupPermission(t, owner, perm, user)
}

// TestNestedGroupAuth — group member nesting (ExtraParentIds).
//
//	outer (top-level, contains inner via AD member attribute)
//	inner (stores ExtraParentIds=[outer])
//	inner-child (ParentId=inner)
//
// User in inner-child must be covered by a permission bound to outer.
func TestNestedGroupAuth(t *testing.T) {
	owner := "pg-nest-" + randomSuffix(t)
	setupGroupAuthTest(t, owner)

	addGroup(t, &Group{Owner: owner, Name: "outer", IsTopGroup: true, Type: "test"})

	// inner has its own OU position (top-level in the OU tree), but is ALSO
	// contained in outer via LDAP member nesting — stored in ExtraParentIds.
	addGroup(t, &Group{
		Owner:          owner,
		Name:           "inner",
		ParentId:       owner,
		IsTopGroup:     true,
		Type:           "test",
		ExtraParentIds: []string{"outer"},
	})

	addGroup(t, &Group{Owner: owner, Name: "inner-child", ParentId: "inner", Type: "test"})

	user := addTestUser(t, owner, "inner-child-user", []string{owner + "/inner-child"})

	perm := makeAllowPermission(owner, "perm-outer", []string{owner + "/outer"})
	addPermission(t, perm)
	defer cleanUpPermission(t, perm)

	checkParentGroupPermission(t, owner, perm, user)

	resolver := newPermissionGroupResolver()
	desc, err := resolver.getGroupIdsWithDescendants(owner, "outer")
	mustErr(t, err)
	assertContains(t, desc, owner+"/inner", "outer descendants must include inner via ExtraParentIds")
	assertContains(t, desc, owner+"/inner-child", "outer descendants must include inner-child")

	ancestors, err := GetGroupIdsWithAncestors(owner, []string{owner + "/inner-child"})
	mustErr(t, err)
	assertContains(t, ancestors, owner+"/outer", "inner-child ancestors must include outer via ExtraParentIds")
}

// TestMixedHierarchy — both OU ParentId and ExtraParentIds contribute in a chain.
//
//	grandparent (top)
//	  -> parent  (ParentId=grandparent)
//	  -> uncle   (top-level in OU tree, ExtraParentIds=[grandparent])
//	parent -> child (ParentId=parent)
//	uncle  -> niece (ParentId=uncle)
//
// A permission on grandparent must cover both child and niece.
func TestMixedHierarchy(t *testing.T) {
	owner := "pg-mix-" + randomSuffix(t)
	setupGroupAuthTest(t, owner)

	addGroup(t, &Group{Owner: owner, Name: "gp", IsTopGroup: true, Type: "test"})
	addGroup(t, &Group{Owner: owner, Name: "parent", ParentId: "gp", Type: "test"})
	addGroup(t, &Group{
		Owner:          owner,
		Name:           "uncle",
		ParentId:       owner,
		IsTopGroup:     true,
		Type:           "test",
		ExtraParentIds: []string{"gp"},
	})
	addGroup(t, &Group{Owner: owner, Name: "child", ParentId: "parent", Type: "test"})
	addGroup(t, &Group{Owner: owner, Name: "niece", ParentId: "uncle", Type: "test"})

	childUser := addTestUser(t, owner, "child-user", []string{owner + "/child"})
	nieceUser := addTestUser(t, owner, "niece-user", []string{owner + "/niece"})

	perm := makeAllowPermission(owner, "perm-gp", []string{owner + "/gp"})
	addPermission(t, perm)
	defer cleanUpPermission(t, perm)

	checkParentGroupPermission(t, owner, perm, childUser)
	checkParentGroupPermission(t, owner, perm, nieceUser)

	resolver := newPermissionGroupResolver()
	desc, err := resolver.getGroupIdsWithDescendants(owner, "gp")
	mustErr(t, err)
	sort.Strings(desc)
	t.Logf("gp descendants: %v", desc)
	assertContains(t, desc, owner+"/parent", "gp descendants include parent (OU)")
	assertContains(t, desc, owner+"/child", "gp descendants include child (OU chain)")
	assertContains(t, desc, owner+"/uncle", "gp descendants include uncle (ExtraParentIds)")
	assertContains(t, desc, owner+"/niece", "gp descendants include niece (ExtraParentIds chain)")
}

// ----- helpers -----

var suffixSeq int

// randomSuffix must stay unique across runs, not just within one: the suite
// runs repeatedly against a live database, and a repeated suffix would
// collide with rows left behind by a previous run.
func randomSuffix(t *testing.T) string {
	suffixSeq++
	return fmt.Sprintf("%d-%d", time.Now().UnixMilli(), suffixSeq)
}

func setupGroupAuthTest(t *testing.T, owner string) {
	t.Helper()
	InitConfig()
	if userEnforcer == nil {
		InitUserManager()
	}

	// Ensure the organization exists (DB may have been reset, AddGroup/User need it).
	if existing, _ := getOrganization("admin", owner); existing == nil {
		added, err := AddOrganization(&Organization{
			Owner:        "admin",
			Name:         owner,
			CreatedTime:  "2026-01-01 00:00:00",
			PasswordType: "bcrypt",
		})
		mustErr(t, err)
		if !added {
			t.Fatalf("AddOrganization(%s) not affected", owner)
		}
		org, err := getOrganization("admin", owner)
		mustErr(t, err)
		mustErr(t, ensureOrganizationApplication(org, ""))
	}

	// Everything a test creates lives under this throwaway organization —
	// delete it wholesale so the suite can run repeatedly against one database.
	t.Cleanup(func() {
		_, _ = ormer.Engine.Exec("DELETE FROM permission_rule WHERE v5 LIKE ?", owner+"/%")
		_, _ = ormer.Engine.Exec("DELETE FROM casbin_user_rule WHERE v0 LIKE ?", owner+"/%")
		_, _ = ormer.Engine.Where("owner = ?", owner).Delete(&Permission{})
		_, _ = ormer.Engine.Where("owner = ?", owner).Delete(&Role{})
		_, _ = ormer.Engine.Where("owner = ?", owner).Delete(&Group{})
		_, _ = ormer.Engine.Where("owner = ?", owner).Delete(&User{})
		_, _ = ormer.Engine.Where("owner = ? AND organization = ?", "admin", owner).Delete(&Application{})
		_, _ = ormer.Engine.Where("owner = ? AND name = ?", "admin", owner).Delete(&Organization{})
	})
}

func addGroup(t *testing.T, g *Group) {
	t.Helper()
	if existing, _ := getGroup(g.Owner, g.Name); existing != nil {
		return
	}
	affected, err := AddGroup(g)
	mustErr(t, err)
	if !affected {
		t.Fatalf("AddGroup(%s/%s) not affected", g.Owner, g.Name)
	}
}

func addPermission(t *testing.T, p *Permission) {
	t.Helper()
	affected, err := AddPermission(p)
	mustErr(t, err)
	if !affected {
		t.Fatalf("AddPermission(%s/%s) not affected", p.Owner, p.Name)
	}
}

func cleanUpPermission(t *testing.T, p *Permission) {
	t.Helper()
	_, _ = DeletePermission(p)
}

func addTestUser(t *testing.T, owner, name string, groups []string) *User {
	t.Helper()
	user := &User{
		Owner:       owner,
		Name:        name,
		Type:        "normal-user",
		Groups:      groups,
		CreatedTime: "2026-01-01 00:00:00",
		Password:    "dummy",
	}
	affected, err := AddUser(user, "")
	mustErr(t, err)
	if !affected {
		t.Fatalf("AddUser(%s/%s) not affected", owner, name)
	}

	// Ensure userEnforcer has the g rows for the memberships we just declared.
	_, err = userEnforcer.UpdateGroupsForUser(user.GetId(), groups)
	mustErr(t, err)

	return user
}

func makeAllowPermission(owner, name string, groups []string) *Permission {
	return &Permission{
		Owner:     owner,
		Name:      name,
		Groups:    groups,
		Resources: []string{"res"},
		Actions:   []string{"act"},
		Effect:    "Allow",
		IsEnabled: true,
		State:     "Approved",
	}
}

func checkParentGroupPermission(t *testing.T, owner string, perm *Permission, user *User) {
	t.Helper()

	// Debug: what groups does the cache see?
	ancestors, err := GetGroupIdsWithAncestors(user.Owner, user.Groups)
	mustErr(t, err)
	t.Logf("user %s groups=%v ancestors=%v perm.Groups=%v", user.GetId(), user.Groups, ancestors, perm.Groups)

	// 1. isGroupHit — membership lookup via ancestor expansion.
	if !perm.isGroupHit(user.GetId()) {
		t.Fatalf("isGroupHit = false for %s via parent-group permission %s", user.GetId(), perm.GetId())
	}

	// 2. Enforce — runtime grouping policy + descendant expansion.
	allowed, err := Enforce(perm, []interface{}{user.GetId(), "res", "act"})
	mustErr(t, err)
	if !allowed {
		t.Fatalf("Enforce = false for %s (want true)", user.GetId())
	}

	// 3. CheckApiPermission — same ancestor expansion via isGroupHit.
	apiPerm := Permission{
		Owner:        owner,
		Name:         perm.Name + "-api",
		Groups:       perm.Groups,
		Resources:    []string{"myapi"},
		Actions:      []string{"GET"},
		Effect:       "Allow",
		IsEnabled:    true,
		State:        "Approved",
		ResourceType: "API",
	}
	addPermission(t, &apiPerm)
	defer cleanUpPermission(t, &apiPerm)

	ok, err := CheckApiPermission(user.GetId(), owner, "/api/myapi", "GET")
	mustErr(t, err)
	if !ok {
		t.Fatalf("CheckApiPermission = false for %s via parent-group API permission", user.GetId())
	}

	// 4. getPermissionsAndRolesByUser — permission discovery via ancestor expansion.
	perms, _, err := getPermissionsAndRolesByUser(user.GetId())
	mustErr(t, err)
	found := false
	for _, p := range perms {
		if p.GetId() == perm.GetId() {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("getPermissionsAndRolesByUser does not surface parent-group permission for %s", user.GetId())
	}
}

func mustErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func assertContains(t *testing.T, slice []string, needle, msg string) {
	t.Helper()
	for _, s := range slice {
		if s == needle {
			return
		}
	}
	t.Fatalf("%s — %v not found in %v", msg, needle, slice)
}
