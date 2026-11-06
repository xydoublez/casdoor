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
	"testing"

	"github.com/casdoor/casdoor/idp"
	"github.com/casdoor/casdoor/object"
)

// Regression for "DingTalk QR login says the account does not exist although the
// user is bound": the DingTalk syncer links synced users with the corporate
// userid (object/syncer_dingtalk.go writes it into the binding column), while the
// QR-code OAuth flow identifies the same person by OpenId. Sign-in has to accept
// both ids, otherwise every synced user fails with
// "The account for provider: DingTalk ... does not exist and is not allowed to
// sign up as new account".
func TestDingTalkSyncerBindingColumnLogin(t *testing.T) {
	env := setupDingTalkBindingTest(t)

	provider := &object.Provider{Owner: "admin", Name: "dingtalk", Type: "DingTalk", Category: "OAuth"}

	// the OAuth flow vouches for the OpenId, the binding column holds the synced userid
	user, err := getUserByProvider(env.application, provider, &idp.UserInfo{
		Id:         env.openId,
		CorpUserId: env.corpUserId,
		Username:   "union-" + env.suffix,
	})
	mustNoErr(t, err)
	if user == nil {
		t.Fatalf("sign-in must resolve the user whose binding column holds the synced userid %s", env.corpUserId)
	}
	if user.GetId() != env.orgName+"/"+env.userName {
		t.Fatalf("resolved %s, want %s/%s", user.GetId(), env.orgName, env.userName)
	}

	// a UI link writes the OpenId into the column: the primary candidate still wins
	linked, err := getUserByProvider(env.application, provider, &idp.UserInfo{Id: env.linkedOpenId})
	mustNoErr(t, err)
	if linked == nil || linked.GetId() != env.orgName+"/"+env.linkedUserName {
		t.Fatalf("sign-in must resolve the user linked by OpenId, got %v", linked)
	}

	// an unknown provider account stays unresolved
	unknown, err := getUserByProvider(env.application, provider, &idp.UserInfo{Id: "open-nobody"})
	mustNoErr(t, err)
	if unknown != nil {
		t.Fatalf("unknown provider account resolved to %s", unknown.GetId())
	}
}

type dingTalkBindingEnv struct {
	suffix         string
	orgName        string
	userName       string
	linkedUserName string
	openId         string
	corpUserId     string
	linkedOpenId   string
	application    *object.Application
}

func setupDingTalkBindingTest(t *testing.T) *dingTalkBindingEnv {
	t.Helper()
	object.InitConfig()
	object.InitUserManager()

	suffix := nextSharedAppSuffix()
	env := &dingTalkBindingEnv{
		suffix:         suffix,
		orgName:        "dt-org-" + suffix,
		userName:       "dt-synced-" + suffix,
		linkedUserName: "dt-linked-" + suffix,
		openId:         "open-" + suffix,
		corpUserId:     "corp-" + suffix,
		linkedOpenId:   "open-linked-" + suffix,
		application:    &object.Application{Owner: "admin", Name: "dt-app-" + suffix, Organization: "dt-org-" + suffix},
	}

	added, err := object.AddOrganization(&object.Organization{
		Owner:        "admin",
		Name:         env.orgName,
		CreatedTime:  "2026-01-01 00:00:00",
		PasswordType: "bcrypt",
	})
	mustNoErr(t, err)
	if !added {
		t.Fatalf("AddOrganization(%s) not affected", env.orgName)
	}
	// AddUser refuses organizations without any application
	_, err = object.AddApplication(&object.Application{Organization: env.orgName, Name: "app-" + env.orgName}, "en")
	mustNoErr(t, err)

	addUser := func(name, dingTalk string) *object.User {
		t.Helper()
		user := &object.User{
			Owner:       env.orgName,
			Name:        name,
			Id:          "id-" + name,
			Type:        "normal-user",
			CreatedTime: "2026-01-01 00:00:00",
			Password:    "dummy",
			DingTalk:    dingTalk,
		}
		ok, err := object.AddUser(user, "en")
		mustNoErr(t, err)
		if !ok {
			t.Fatalf("AddUser(%s/%s) not affected", env.orgName, name)
		}
		return user
	}
	// like object/syncer_dingtalk.go: the syncer links by the corporate userid
	synced := addUser(env.userName, env.corpUserId)
	linked := addUser(env.linkedUserName, env.linkedOpenId)

	t.Cleanup(func() {
		_, _ = object.DeleteUser(synced)
		_, _ = object.DeleteUser(linked)
		_, _ = object.DeleteApplication(&object.Application{Owner: "admin", Name: "app-" + env.orgName})
		_, _ = object.DeleteOrganization(&object.Organization{Owner: "admin", Name: env.orgName})
	})

	return env
}
