// Copyright 2025 The Casdoor Authors. All Rights Reserved.
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
)

func TestConvertToTreeDataWithLdapSyncedRootGroups(t *testing.T) {
	owner := "zymsunsoft"

	// Simulate groups synced from LDAP: top-level groups have ParentId=""
	// and IsTopGroup=true, while manually created top-level groups have
	// ParentId equal to the owner.
	groups := []*Group{
		{Owner: owner, Name: "产品研发中心", DisplayName: "产品研发中心", ParentId: "", IsTopGroup: true, Type: "ldap-synced"},
		{Owner: owner, Name: "临床住院部", DisplayName: "临床住院部", ParentId: "产品研发中心", IsTopGroup: false, Type: "ldap-synced"},
		{Owner: owner, Name: "manual_root", DisplayName: "手动根组", ParentId: owner, IsTopGroup: true, Type: "Virtual"},
		{Owner: owner, Name: "manual_child", DisplayName: "手动子组", ParentId: "manual_root", IsTopGroup: false, Type: "Virtual"},
	}

	tree := ConvertToTreeData(groups, owner)

	if len(tree) != 2 {
		t.Fatalf("expected 2 root nodes, got %d", len(tree))
	}

	rootKeys := map[string]bool{}
	for _, node := range tree {
		rootKeys[node.Key] = true
	}
	if !rootKeys["产品研发中心"] {
		t.Fatalf("expected 产品研发中心 to be a root node, got %v", rootKeys)
	}
	if !rootKeys["manual_root"] {
		t.Fatalf("expected manual_root to be a root node, got %v", rootKeys)
	}

	// Verify the LDAP hierarchy is preserved under 产品研发中心
	var rdNode *Group
	for _, node := range tree {
		if node.Key == "产品研发中心" {
			rdNode = node
			break
		}
	}
	if rdNode == nil || len(rdNode.Children) != 1 || rdNode.Children[0].Key != "临床住院部" {
		t.Fatalf("expected 临床住院部 as child of 产品研发中心, got %+v", rdNode)
	}

	// Verify the manual hierarchy is preserved
	var manualRoot *Group
	for _, node := range tree {
		if node.Key == "manual_root" {
			manualRoot = node
			break
		}
	}
	if manualRoot == nil || len(manualRoot.Children) != 1 || manualRoot.Children[0].Key != "manual_child" {
		t.Fatalf("expected manual_child as child of manual_root, got %+v", manualRoot)
	}
}

func TestConvertToTreeDataEmpty(t *testing.T) {
	tree := ConvertToTreeData([]*Group{}, "zymsunsoft")
	if len(tree) != 0 {
		t.Fatalf("expected empty tree, got %d nodes", len(tree))
	}
}

func TestConvertToTreeDataSelfParentGroup(t *testing.T) {
	owner := "zymsunsoft"

	// Some LDAP entries are self-referencing (e.g. cn=X inside ou=X).
	// They must be treated as top-level root nodes and must not recurse into themselves.
	groups := []*Group{
		{Owner: owner, Name: "self-ref", DisplayName: "自引用组", ParentId: "self-ref", IsTopGroup: true, Type: "ldap-synced"},
		{Owner: owner, Name: "child", DisplayName: "子组", ParentId: "self-ref", IsTopGroup: false, Type: "ldap-synced"},
	}

	tree := ConvertToTreeData(groups, owner)
	if len(tree) != 1 || tree[0].Key != "self-ref" {
		t.Fatalf("expected self-ref as single root node, got %+v", tree)
	}
	if len(tree[0].Children) != 1 || tree[0].Children[0].Key != "child" {
		t.Fatalf("expected child as child of self-ref, got %+v", tree[0].Children)
	}
}
