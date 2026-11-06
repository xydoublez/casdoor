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

package controllers

import (
	"fmt"
	"testing"
	"time"

	"github.com/casdoor/casdoor/idp"
	"github.com/casdoor/casdoor/object"
)

// Regression for "DingTalk QR login says the account does not exist after linking":
// a shared application belongs to "built-in", while its users live in the
// organizations that have it as default application. The provider (OAuth/SAML)
// login used to search only the application's own organization, so a user linked
// to a third-party account in their own organization was never found and got
// "The account for provider: ... does not exist and is not allowed to sign up",
// while password login (GetUserByFieldsForSharedApp) worked fine.
func TestSharedAppProviderLoginFindsLinkedUser(t *testing.T) {
	env := setupSharedAppLoginTest(t)

	provider := &object.Provider{Owner: "admin", Name: "dingtalk", Type: "DingTalk", Category: "OAuth"}

	// the binding column lives on the user row in the user's own organization
	user, err := getUserByProvider(env.sharedApp, provider, &idp.UserInfo{Id: env.dingTalkId})
	mustNoErr(t, err)
	if user == nil {
		t.Fatalf("shared application %s must resolve the user linked to DingTalk id %s in organization %s", env.sharedApp.Name, env.dingTalkId, env.orgName)
	}
	if user.GetId() != env.orgName+"/"+env.userName {
		t.Fatalf("resolved %s, want %s/%s", user.GetId(), env.orgName, env.userName)
	}

	// a non-shared application keeps its single-organization scope
	nonShared := &object.Application{Owner: "admin", Name: "app-" + env.orgName, Organization: "built-in"}
	user, err = getUserByProvider(nonShared, provider, &idp.UserInfo{Id: env.dingTalkId})
	mustNoErr(t, err)
	if user != nil {
		t.Fatalf("non-shared application must not resolve users outside its own organization, got %s", user.GetId())
	}

	// an organization that does not default to the shared application stays out of scope
	user, err = getUserByProvider(env.sharedApp, provider, &idp.UserInfo{Id: env.otherDingTalkId})
	mustNoErr(t, err)
	if user != nil {
		t.Fatalf("shared application must not resolve users of organizations that do not default to it, got %s", user.GetId())
	}
}

func TestSharedAppBindingRuleFindsUser(t *testing.T) {
	env := setupSharedAppLoginTest(t)

	providerItem := &object.ProviderItem{Name: "dingtalk"}
	userInfo := &idp.UserInfo{Email: env.email, EmailVerified: true}

	user, err := getExistUserByBindingRule(providerItem, env.sharedApp, userInfo)
	mustNoErr(t, err)
	if user == nil {
		t.Fatal("binding rule must resolve the user by verified email across the shared application's organizations")
	}
	if user.GetId() != env.orgName+"/"+env.userName {
		t.Fatalf("resolved %s, want %s/%s", user.GetId(), env.orgName, env.userName)
	}
}

type sharedAppLoginEnv struct {
	orgName         string
	userName        string
	email           string
	dingTalkId      string
	otherDingTalkId string
	sharedApp       *object.Application
}

func setupSharedAppLoginTest(t *testing.T) *sharedAppLoginEnv {
	t.Helper()
	object.InitConfig()
	object.InitUserManager()

	suffix := nextSharedAppSuffix()
	sharedAppName := "sa-app-" + suffix
	env := &sharedAppLoginEnv{
		orgName:         "sa-org-" + suffix,
		userName:        "sa-user-" + suffix,
		email:           "sa-user-" + suffix + "@example.com",
		dingTalkId:      "dt-open-" + suffix,
		otherDingTalkId: "dt-other-" + suffix,
		sharedApp:       &object.Application{Owner: "admin", Name: sharedAppName, Organization: "built-in", IsShared: true},
	}
	otherOrg := "sa-other-" + suffix

	addOrg := func(name string, defaultApplication string) {
		t.Helper()
		added, err := object.AddOrganization(&object.Organization{
			Owner:              "admin",
			Name:               name,
			CreatedTime:        "2026-01-01 00:00:00",
			PasswordType:       "bcrypt",
			DefaultApplication: defaultApplication,
		})
		mustNoErr(t, err)
		if !added {
			t.Fatalf("AddOrganization(%s) not affected", name)
		}
		// AddUser refuses organizations without any application
		_, err = object.AddApplication(&object.Application{Organization: name, Name: "app-" + name}, "en")
		mustNoErr(t, err)
	}
	addOrg(env.orgName, sharedAppName)
	addOrg(otherOrg, "")

	addUser := func(owner, name, id, dingTalk, email string) *object.User {
		t.Helper()
		user := &object.User{
			Owner:         owner,
			Name:          name,
			Id:            id,
			Type:          "normal-user",
			CreatedTime:   "2026-01-01 00:00:00",
			Password:      "dummy",
			DingTalk:      dingTalk,
			Email:         email,
			EmailVerified: email != "",
		}
		added, err := object.AddUser(user, "en")
		mustNoErr(t, err)
		if !added {
			t.Fatalf("AddUser(%s/%s) not affected", owner, name)
		}
		return user
	}
	user := addUser(env.orgName, env.userName, "u-"+suffix, env.dingTalkId, env.email)
	otherUser := addUser(otherOrg, "sa-other-user-"+suffix, "u2-"+suffix, env.otherDingTalkId, "")

	t.Cleanup(func() {
		_, _ = object.DeleteUser(user)
		_, _ = object.DeleteUser(otherUser)
		_, _ = object.DeleteApplication(&object.Application{Owner: "admin", Name: "app-" + env.orgName})
		_, _ = object.DeleteApplication(&object.Application{Owner: "admin", Name: "app-" + otherOrg})
		_, _ = object.DeleteApplication(&object.Application{Owner: "admin", Name: sharedAppName})
		_, _ = object.DeleteOrganization(&object.Organization{Owner: "admin", Name: env.orgName})
		_, _ = object.DeleteOrganization(&object.Organization{Owner: "admin", Name: otherOrg})
	})

	return env
}

var sharedAppTestSeq int

// nextSharedAppSuffix keeps the throwaway rows of consecutive runs apart.
func nextSharedAppSuffix() string {
	sharedAppTestSeq++
	return fmt.Sprintf("%d-%d", time.Now().UnixMilli(), sharedAppTestSeq)
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
