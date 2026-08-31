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

import "testing"

func TestDnToGroupName(t *testing.T) {
	tests := []struct {
		name     string
		owner    string
		dn       string
		expected string
	}{
		{
			name:     "empty DN",
			owner:    "admin",
			dn:       "",
			expected: "",
		},
		{
			name:     "simple CN",
			owner:    "admin",
			dn:       "CN=研发中心,DC=example,DC=com",
			expected: "研发中心",
		},
		{
			name:     "CJK name",
			owner:    "admin",
			dn:       "CN=信息安全合规部,OU=研发中心,DC=example,DC=com",
			expected: "信息安全合规部",
		},
		{
			name:     "slash is replaced",
			owner:    "admin",
			dn:       "CN=group/name,DC=example,DC=com",
			expected: "group_name",
		},
		{
			name:     "lowercase prefix",
			owner:    "admin",
			dn:       "cn=test,dc=example,dc=com",
			expected: "test",
		},
		{
			name:     "OU first RDN",
			owner:    "admin",
			dn:       "OU=研发中心,DC=example,DC=com",
			expected: "研发中心",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dnToGroupName(tt.owner, tt.dn)
			if got != tt.expected {
				t.Fatalf("dnToGroupName(%q, %q) = %q, want %q", tt.owner, tt.dn, got, tt.expected)
			}
		})
	}
}
