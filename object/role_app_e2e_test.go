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

// TestRoleWithParentGroupGrantsTwoApplications —— 端到端场景：
//
//	组织架构（OU 层级）:  parent (研发中心/父群组)
//	                       └── child  (子群组)
//	用户: alice 只属于 child
//
//	授权配置:
//	  Role  "dev-role"   Groups = [parent]        ← 角色只绑父群组
//	  App1  "app-test-1" / App2 "app-test-2"      ← 两个测试应用
//	  Permission "app-access" ResourceType=Application
//	      Roles = [dev-role], Resources = [app-test-1, app-test-2]
//
//	预期: alice（子群组用户）通过父群组继承 dev-role，两个应用均可登录；
//	      bob（不属于任何群组）不能登录。
func TestRoleWithParentGroupGrantsTwoApplications(t *testing.T) {
	owner := "rg-" + randomSuffix(t)
	// 自动清理由 setupGroupAuthTest 注册的 t.Cleanup 完成（连同组织一起删除），保证测试可重复执行
	setupGroupAuthTest(t, owner)

	// ---- 1. 组织架构：父群组 + 子群组 ----
	addGroup(t, &Group{Owner: owner, Name: "parent", DisplayName: "研发中心", IsTopGroup: true, Type: "test"})
	addGroup(t, &Group{Owner: owner, Name: "child", DisplayName: "后端组", ParentId: "parent", Type: "test"})

	// alice 只直接属于子群组 child
	alice := addTestUser(t, owner, "alice", []string{owner + "/child"})
	// bob 不属于任何群组（对照组）
	bob := addTestUser(t, owner, "bob", nil)

	// ---- 2. 测试角色：只绑定父群组 ----
	devRole := &Role{
		Owner:       owner,
		Name:        "dev-role",
		DisplayName: "研发角色",
		Groups:      []string{owner + "/parent"}, // ← 只配父群组
	}
	affected, err := AddRole(devRole)
	mustErr(t, err)
	if !affected {
		t.Fatal("AddRole not affected")
	}

	// ---- 3. 两个测试应用 ----
	app1 := &Application{Name: "app-test-1", Organization: owner, DisplayName: "测试应用一"}
	app2 := &Application{Name: "app-test-2", Organization: owner, DisplayName: "测试应用二"}
	for _, app := range []*Application{app1, app2} {
		added, err := AddApplication(app, "")
		mustErr(t, err)
		if !added {
			t.Fatalf("AddApplication(%s) not affected", app.Name)
		}
	}
	// 重新查库拿到完整字段（ClientSecret 等）
	app1, err = GetApplication("admin/" + app1.Name)
	mustErr(t, err)
	app2, err = GetApplication("admin/" + app2.Name)
	mustErr(t, err)

	// ---- 4. 授权权限：角色 → 两个应用 ----
	appPerm := &Permission{
		Owner:        owner,
		Name:         "perm-app-access",
		DisplayName:  "应用访问授权",
		ResourceType: "Application",
		Resources:    []string{app1.Name, app2.Name},
		Roles:        []string{devRole.GetId()}, // ← 通过角色授权
		Effect:       "Allow",
		IsEnabled:    true,
		State:        "Approved",
	}
	addPermission(t, appPerm)

	// ---- 5. 验证 1: 角色继承（子群组用户 → 父群组角色）----
	roles, err := getRolesByUser(alice.GetId())
	mustErr(t, err)
	var roleIds []string
	hit := false
	for _, r := range roles {
		roleIds = append(roleIds, r.GetId())
		if r.GetId() == devRole.GetId() {
			hit = true
			if len(r.SourceGroups) == 0 {
				t.Fatalf("role.SourceGroups is empty, want to record inheritance source")
			}
			t.Logf("alice 继承角色 %s，来源群组: %v", r.GetId(), r.SourceGroups)
			foundParent := false
			for _, g := range r.SourceGroups {
				if g == owner+"/parent" {
					foundParent = true
				}
			}
			if !foundParent {
				t.Fatalf("SourceGroups = %v, want to contain parent %s", r.SourceGroups, owner+"/parent")
			}
		}
	}
	t.Logf("alice 的角色: [%s]", strings.Join(roleIds, ", "))
	if !hit {
		t.Fatalf("alice (子群组 child 用户) 未继承绑定父群组 parent 的角色 %s", devRole.GetId())
	}

	// ---- 6. 验证 2: 两个应用都能登录 ----
	for _, app := range []*Application{app1, app2} {
		okAlice, err := CheckLoginPermission(alice.GetId(), app)
		mustErr(t, err)
		t.Logf("CheckLoginPermission(alice, %s) = %v", app.Name, okAlice)
		if !okAlice {
			t.Fatalf("alice 无法登录 %s —— 父群组角色未对子群组用户生效", app.Name)
		}

		// 对照组：bob 没有任何群组/角色
		okBob, err := CheckLoginPermission(bob.GetId(), app)
		mustErr(t, err)
		t.Logf("CheckLoginPermission(bob,   %s) = %v (对照组，应 false)", app.Name, okBob)
		if okBob {
			t.Fatalf("bob 无群组无角色，不应登录 %s", app.Name)
		}
	}

	// ---- 7. 验证 3: 权限发现（token/前端权限列表）----
	perms, _, err := getPermissionsAndRolesByUser(alice.GetId())
	mustErr(t, err)
	found := false
	for _, p := range perms {
		if p.GetId() == appPerm.GetId() {
			found = true
		}
	}
	if !found {
		t.Fatalf("getPermissionsAndRolesByUser 未返回应用授权权限给 alice")
	}
	t.Logf("alice 权限发现包含 %s ✓", appPerm.GetId())

	// ---- 8. 验证 4: 新用户加入子群组后同样生效（模拟 AD 同步新增用户）----
	carol := addTestUser(t, owner, "carol", []string{owner + "/child"})
	okCarol, err := CheckLoginPermission(carol.GetId(), app1)
	mustErr(t, err)
	if !okCarol {
		t.Fatalf("carol (新加入子群组) 无法登录 %s", app1.Name)
	}
	t.Logf("carol (新加入 child) 登录 %s = %v ✓", app1.Name, okCarol)

	t.Logf("全部通过：角色绑父群组 → 子群组用户获得两个应用的访问权限")
}
