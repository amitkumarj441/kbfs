// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/keybase/client/go/libkb"
	keybase1 "github.com/keybase/client/go/protocol"
	"golang.org/x/net/context"
)

func makeTestKBPKIClient(t *testing.T) (
	client *KBPKIClient, currentUID keybase1.UID, users []LocalUser) {
	currentUID = keybase1.MakeTestUID(1)
	names := []libkb.NormalizedUsername{"test_name1", "test_name2"}
	users = MakeLocalUsers(names)
	codec := NewCodecMsgpack()
	daemon := NewKeybaseDaemonMemory(currentUID, users, codec)
	config := &ConfigLocal{codec: codec, service: daemon}
	setTestLogger(config, t)
	return NewKBPKIClient(config), currentUID, users
}

func makeTestKBPKIClientWithRevokedKey(t *testing.T, revokeTime time.Time) (
	client *KBPKIClient, currentUID keybase1.UID, users []LocalUser) {
	currentUID = keybase1.MakeTestUID(1)
	names := []libkb.NormalizedUsername{"test_name1", "test_name2"}
	users = MakeLocalUsers(names)
	// Give each user a revoked key
	for i, user := range users {
		index := 99
		keySalt := keySaltForUserDevice(user.Name, index)
		newVerifyingKey := MakeLocalUserVerifyingKeyOrBust(keySalt)
		user.RevokedVerifyingKeys = map[VerifyingKey]keybase1.KeybaseTime{
			newVerifyingKey: {Unix: keybase1.ToTime(revokeTime), Chain: 100},
		}
		users[i] = user
	}
	codec := NewCodecMsgpack()
	daemon := NewKeybaseDaemonMemory(currentUID, users, codec)
	config := &ConfigLocal{codec: codec, service: daemon}
	setTestLogger(config, t)
	return NewKBPKIClient(config), currentUID, users
}

func TestKBPKIClientIdentify(t *testing.T) {
	c, _, _ := makeTestKBPKIClient(t)

	u, err := c.Identify(context.Background(), "test_name1", "")
	if err != nil {
		t.Fatal(err)
	}
	if u.UID == keybase1.UID("") {
		t.Fatal("empty user")
	}
}

func TestKBPKIClientGetNormalizedUsername(t *testing.T) {
	c, _, _ := makeTestKBPKIClient(t)

	name, err := c.GetNormalizedUsername(context.Background(), keybase1.MakeTestUID(1))
	if err != nil {
		t.Fatal(err)
	}
	if name == libkb.NormalizedUsername("") {
		t.Fatal("empty user")
	}
}

func TestKBPKIClientHasVerifyingKey(t *testing.T) {
	c, _, localUsers := makeTestKBPKIClient(t)

	err := c.HasVerifyingKey(context.Background(), keybase1.MakeTestUID(1),
		localUsers[0].VerifyingKeys[0], time.Now())
	if err != nil {
		t.Error(err)
	}

	err = c.HasVerifyingKey(context.Background(), keybase1.MakeTestUID(1),
		VerifyingKey{}, time.Now())
	if err == nil {
		t.Error("HasVerifyingKey unexpectedly succeeded")
	}
}

func TestKBPKIClientHasRevokedVerifyingKey(t *testing.T) {
	revokeTime := time.Now()
	c, _, localUsers := makeTestKBPKIClientWithRevokedKey(t, revokeTime)

	var revokedKey VerifyingKey
	for k := range localUsers[0].RevokedVerifyingKeys {
		revokedKey = k
		break
	}

	// Something verified before the key was revoked
	err := c.HasVerifyingKey(context.Background(), keybase1.MakeTestUID(1),
		revokedKey, revokeTime.Add(-10*time.Second))
	if err != nil {
		t.Error(err)
	}

	// Something verified after the key was revoked
	err = c.HasVerifyingKey(context.Background(), keybase1.MakeTestUID(1),
		revokedKey, revokeTime.Add(10*time.Second))
	if err == nil {
		t.Error("HasVerifyingKey unexpectedly succeeded")
	}
}

// Test that KBPKI forces a cache flush one time if it can't find a
// given verifying key.
func TestKBPKIClientHasVerifyingKeyStaleCache(t *testing.T) {
	ctr := NewSafeTestReporter(t)
	mockCtrl := gomock.NewController(ctr)
	config := NewConfigMock(mockCtrl, ctr)
	c := NewKBPKIClient(config)
	config.SetKBPKI(c)
	defer func() {
		config.ctr.CheckForFailures()
		mockCtrl.Finish()
	}()

	u := keybase1.MakeTestUID(1)
	key1 := MakeLocalUserVerifyingKeyOrBust("u_1")
	key2 := MakeLocalUserVerifyingKeyOrBust("u_2")
	info1 := UserInfo{
		VerifyingKeys: []VerifyingKey{key1},
	}
	config.mockKbs.EXPECT().LoadUserPlusKeys(gomock.Any(), u).
		Return(info1, nil)

	config.mockKbs.EXPECT().FlushUserFromLocalCache(gomock.Any(), u)
	info2 := UserInfo{
		VerifyingKeys: []VerifyingKey{key1, key2},
	}
	config.mockKbs.EXPECT().LoadUserPlusKeys(gomock.Any(), u).
		Return(info2, nil)

	err := c.HasVerifyingKey(context.Background(), u, key2, time.Now())
	if err != nil {
		t.Error(err)
	}
}

func TestKBPKIClientGetCryptPublicKeys(t *testing.T) {
	c, _, localUsers := makeTestKBPKIClient(t)

	cryptPublicKeys, err := c.GetCryptPublicKeys(context.Background(),
		keybase1.MakeTestUID(1))
	if err != nil {
		t.Fatal(err)
	}

	if len(cryptPublicKeys) != 1 {
		t.Fatalf("Expected 1 crypt public key, got %d", len(cryptPublicKeys))
	}

	kid := cryptPublicKeys[0].kid
	expectedKID := localUsers[0].CryptPublicKeys[0].kid
	if kid != expectedKID {
		t.Errorf("Expected %s, got %s", expectedKID, kid)
	}
}

func TestKBPKIClientGetCurrentCryptPublicKey(t *testing.T) {
	c, _, localUsers := makeTestKBPKIClient(t)

	currPublicKey, err := c.GetCurrentCryptPublicKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	kid := currPublicKey.kid
	expectedKID := localUsers[0].GetCurrentCryptPublicKey().kid
	if kid != expectedKID {
		t.Errorf("Expected %s, got %s", expectedKID, kid)
	}
}

func makeTestKBPKIClientWithUnverifiedKey(t *testing.T) (
	client *KBPKIClient, currentUID keybase1.UID, users []LocalUser) {
	currentUID = keybase1.MakeTestUID(1)
	names := []libkb.NormalizedUsername{"test_name1", "test_name2"}
	users = MakeLocalUsers(names)
	// Give each user an unverified key
	for i, user := range users {
		index := 99
		keySalt := keySaltForUserDevice(user.Name, index)
		newVerifyingKey := MakeLocalUserVerifyingKeyOrBust(keySalt)
		key := keybase1.PublicKey{KID: newVerifyingKey.kid}
		user.UnverifiedKeys = []keybase1.PublicKey{key}
		users[i] = user
	}
	codec := NewCodecMsgpack()
	daemon := NewKeybaseDaemonMemory(currentUID, users, codec)
	config := &ConfigLocal{codec: codec, service: daemon}
	setTestLogger(config, t)
	return NewKBPKIClient(config), currentUID, users
}

func TestKBPKIClientHasUnverifiedVerifyingKey(t *testing.T) {
	c, _, localUsers := makeTestKBPKIClientWithUnverifiedKey(t)

	var unverifiedKey VerifyingKey
	for _, k := range localUsers[0].UnverifiedKeys {
		unverifiedKey.kid = k.KID
		break
	}

	err := c.HasVerifyingKey(context.Background(), keybase1.MakeTestUID(1),
		unverifiedKey, time.Time{})
	if err == nil {
		t.Error("HasVerifyingKey unexpectedly succeeded")
	}

	err = c.HasUnverifiedVerifyingKey(context.Background(), keybase1.MakeTestUID(1),
		unverifiedKey)
	if err != nil {
		t.Fatal(err)
	}
}
