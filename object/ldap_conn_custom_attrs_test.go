// Copyright 2025 The Casdoor Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !skipCi

package object

import (
	"sort"
	"testing"
)

// Note: LdapUser.Attributes is keyed by the *user property name* (the mapped
// value of Ldap.CustomAttributes), not by the LDAP attribute name. E.g. a
// CustomAttributes config of {"department": "affiliation"} produces
// Attributes = {"affiliation": "信息安全合规部"}.

func hasColumn(columns []string, column string) bool {
	for _, c := range columns {
		if c == column {
			return true
		}
	}
	return false
}

func TestApplyLdapCustomAttributesADFields(t *testing.T) {
	// Produced by CustomAttributes {"department": "affiliation", "title": "title"}
	attributes := map[string]string{
		"affiliation": "信息安全合规部",
		"title":       "安全工程师",
	}

	user := &User{}
	columns := applyLdapCustomAttributes(user, attributes)

	if user.Affiliation != "信息安全合规部" {
		t.Fatalf("expected affiliation to be 信息安全合规部, got %q", user.Affiliation)
	}
	if user.Title != "安全工程师" {
		t.Fatalf("expected title to be 安全工程师, got %q", user.Title)
	}
	if !hasColumn(columns, "affiliation") || !hasColumn(columns, "title") {
		t.Fatalf("expected columns [affiliation, title], got %v", columns)
	}
}

func TestApplyLdapCustomAttributesUnchangedReturnsNoColumns(t *testing.T) {
	attributes := map[string]string{
		"affiliation": "信息安全合规部",
		"title":       "安全工程师",
	}

	user := &User{Affiliation: "信息安全合规部", Title: "安全工程师"}
	columns := applyLdapCustomAttributes(user, attributes)

	if len(columns) != 0 {
		t.Fatalf("expected no changed columns, got %v", columns)
	}
}

func TestApplyLdapCustomAttributesAllSupportedFields(t *testing.T) {
	attributes := map[string]string{
		"affiliation": "dep",
		"title":       "job",
		"tag":         "tag",
		"bio":         "bio",
		"location":    "loc",
		"region":      "region",
		"homepage":    "home",
	}

	user := &User{}
	applyLdapCustomAttributes(user, attributes)

	if user.Affiliation != "dep" || user.Title != "job" || user.Tag != "tag" ||
		user.Bio != "bio" || user.Location != "loc" || user.Region != "region" ||
		user.Homepage != "home" {
		t.Fatalf("unexpected user: affiliation=%q title=%q tag=%q bio=%q location=%q region=%q homepage=%q",
			user.Affiliation, user.Title, user.Tag, user.Bio, user.Location, user.Region, user.Homepage)
	}
}

func TestApplyLdapCustomAttributesCaseInsensitiveAndEmpty(t *testing.T) {
	attributes := map[string]string{
		"Affiliation": "信息安全合规部",
		"TITLE":       "安全工程师",
		"tag":         "", // empty value must be skipped
	}

	user := &User{Title: "安全工程师"}
	columns := applyLdapCustomAttributes(user, attributes)

	if user.Affiliation != "信息安全合规部" {
		t.Fatalf("expected affiliation to be set (case-insensitive key), got %q", user.Affiliation)
	}
	if user.Title != "安全工程师" {
		t.Fatalf("expected title unchanged, got %q", user.Title)
	}
	if user.Tag != "" {
		t.Fatalf("expected tag skipped on empty value, got %q", user.Tag)
	}
	if len(columns) != 1 || columns[0] != "affiliation" {
		t.Fatalf("expected only [affiliation] changed, got %v", columns)
	}
}

func TestApplyLdapCustomAttributesUnknownAndNil(t *testing.T) {
	if columns := applyLdapCustomAttributes(&User{}, nil); columns != nil {
		t.Fatalf("expected nil columns for nil attributes, got %v", columns)
	}

	// Unknown property names are ignored (still kept in Properties elsewhere)
	user := &User{Affiliation: "original"}
	columns := applyLdapCustomAttributes(user, map[string]string{"unknownProp": "x"})

	if user.Affiliation != "original" {
		t.Fatalf("expected affiliation untouched, got %q", user.Affiliation)
	}
	if len(columns) != 0 {
		t.Fatalf("expected no changed columns, got %v", columns)
	}
}

func TestApplyLdapCustomAttributesOverridesBuiltins(t *testing.T) {
	// Custom mapping has the highest priority: it is applied after the
	// built-in o/company/title mapping in SyncLdapUsers.
	user := &User{Affiliation: "dc-derived.example.com", Title: "built-in title"}
	columns := applyLdapCustomAttributes(user, map[string]string{"affiliation": "研发部"})

	if user.Affiliation != "研发部" {
		t.Fatalf("expected custom mapping to override, got %q", user.Affiliation)
	}
	if user.Title != "built-in title" {
		t.Fatalf("expected unrelated fields untouched, got %q", user.Title)
	}
	if !hasColumn(columns, "affiliation") {
		t.Fatalf("expected [affiliation] in changed columns, got %v", columns)
	}
}

func TestApplyLdapCustomAttributesOnlyChangedColumns(t *testing.T) {
	user := &User{Affiliation: "dep", Title: "job", Tag: "old"}
	columns := applyLdapCustomAttributes(user, map[string]string{
		"affiliation": "dep",  // unchanged -> no column
		"title":       "new-job",
		"tag":         "new-tag",
	})

	sort.Strings(columns)
	expected := []string{"tag", "title"}
	if len(columns) != len(expected) {
		t.Fatalf("expected %v, got %v", expected, columns)
	}
	for i := range expected {
		if columns[i] != expected[i] {
			t.Fatalf("expected %v, got %v", expected, columns)
		}
	}
	if user.Affiliation != "dep" || user.Title != "new-job" || user.Tag != "new-tag" {
		t.Fatalf("unexpected user: %+v", user)
	}
}
