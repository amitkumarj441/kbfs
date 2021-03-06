// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/keybase/client/go/externals"
	"github.com/keybase/client/go/libkb"
	"github.com/keybase/client/go/logger"
	keybase1 "github.com/keybase/client/go/protocol"
	"github.com/keybase/kbfs/env"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
)

const (
	// EnvTestMDServerAddr is the environment variable name for an
	// mdserver address.
	EnvTestMDServerAddr = "KEYBASE_TEST_MDSERVER_ADDR"
	// EnvTestBServerAddr is the environment variable name for a block
	// server address.
	EnvTestBServerAddr = "KEYBASE_TEST_BSERVER_ADDR"
	// TempdirServerAddr is the special value of the
	// EnvTest{B,MD}ServerAddr environment value to signify that
	// an on-disk implementation of the {b,md}server should be
	// used with a temporary directory.
	TempdirServerAddr = "tempdir"
)

// RandomBlockID returns a randomly-generated BlockID for testing.
func RandomBlockID() BlockID {
	var dh RawDefaultHash
	err := cryptoRandRead(dh[:])
	if err != nil {
		panic(err)
	}
	h, err := HashFromRaw(DefaultHashType, dh[:])
	if err != nil {
		panic(err)
	}
	return BlockID{h}
}

func fakeMdID(b byte) MdID {
	dh := RawDefaultHash{b}
	h, err := HashFromRaw(DefaultHashType, dh[:])
	if err != nil {
		panic(err)
	}
	return MdID{h}
}

func testLoggerMaker(t logger.TestLogBackend) func(m string) logger.Logger {
	return func(m string) logger.Logger {
		return logger.NewTestLogger(t)
	}
}

func setTestLogger(config Config, t logger.TestLogBackend) {
	config.SetLoggerMaker(testLoggerMaker(t))
}

// MakeTestConfigOrBust creates and returns a config suitable for
// unit-testing with the given list of users.
func MakeTestConfigOrBust(t logger.TestLogBackend,
	users ...libkb.NormalizedUsername) *ConfigLocal {
	config := NewConfigLocal()
	setTestLogger(config, t)

	kbfsOps := NewKBFSOpsStandard(config)
	config.SetKBFSOps(kbfsOps)
	config.SetNotifier(kbfsOps)

	config.SetBlockSplitter(&BlockSplitterSimple{64 * 1024, 8 * 1024})
	config.SetKeyManager(NewKeyManagerStandard(config))
	config.SetMDOps(NewMDOpsStandard(config))

	localUsers := MakeLocalUsers(users)
	loggedInUser := localUsers[0]

	daemon := NewKeybaseDaemonMemory(loggedInUser.UID, localUsers,
		config.Codec())
	config.SetKeybaseService(daemon)

	kbpki := NewKBPKIClient(config)
	config.SetKBPKI(kbpki)

	signingKey := MakeLocalUserSigningKeyOrBust(loggedInUser.Name)
	cryptPrivateKey := MakeLocalUserCryptPrivateKeyOrBust(loggedInUser.Name)
	crypto := NewCryptoLocal(config, signingKey, cryptPrivateKey)
	config.SetCrypto(crypto)

	// see if a local remote server is specified
	bserverAddr := os.Getenv(EnvTestBServerAddr)
	var blockServer BlockServer
	switch {
	case bserverAddr == TempdirServerAddr:
		var err error
		blockServer, err = NewBlockServerTempDir(config)
		if err != nil {
			t.Fatal(err)
		}

	case len(bserverAddr) != 0:
		blockServer = NewBlockServerRemote(config, bserverAddr, env.NewContext())

	default:
		blockServer = NewBlockServerMemory(config)
	}
	config.SetBlockServer(blockServer)

	// see if a local remote server is specified
	mdServerAddr := os.Getenv(EnvTestMDServerAddr)
	var mdServer MDServer
	var keyServer KeyServer
	switch {
	case mdServerAddr == TempdirServerAddr:
		var err error
		mdServer, err = NewMDServerTempDir(config)
		if err != nil {
			t.Fatal(err)
		}
		keyServer, err = NewKeyServerTempDir(config)
		if err != nil {
			t.Fatal(err)
		}

	case len(mdServerAddr) != 0:
		var err error
		// start/restart local in-memory DynamoDB
		runner, err := NewTestDynamoDBRunner()
		if err != nil {
			t.Fatal(err)
		}
		runner.Run(t)

		// initialize libkb -- this probably isn't the best place to do this
		// but it seems as though the MDServer rpc client is the first thing to
		// use things from it which require initialization.
		libkb.G.Init()
		libkb.G.ConfigureLogging()

		// connect to server
		mdServer = NewMDServerRemote(config, mdServerAddr, env.NewContext())
		// for now the MD server acts as the key server in production
		keyServer = mdServer.(*MDServerRemote)

	default:
		var err error
		// create in-memory server shim
		mdServer, err = NewMDServerMemory(config)
		if err != nil {
			t.Fatal(err)
		}
		// shim for the key server too
		keyServer, err = NewKeyServerMemory(config)
		if err != nil {
			t.Fatal(err)
		}
	}
	config.SetMDServer(mdServer)
	config.SetKeyServer(keyServer)

	// turn off background flushing by default during tests
	config.noBGFlush = true

	// no auto reclamation
	config.qrPeriod = 0 * time.Second

	configs := []Config{config}
	config.allKnownConfigsForTesting = &configs

	return config
}

// ConfigAsUser clones a test configuration, setting another user as
// the logged in user
func ConfigAsUser(config *ConfigLocal, loggedInUser libkb.NormalizedUsername) *ConfigLocal {
	c := NewConfigLocal()
	c.SetLoggerMaker(config.loggerFn)

	kbfsOps := NewKBFSOpsStandard(c)
	c.SetKBFSOps(kbfsOps)
	c.SetNotifier(kbfsOps)

	c.SetBlockSplitter(config.BlockSplitter())
	c.SetKeyManager(NewKeyManagerStandard(c))
	c.SetMDOps(NewMDOpsStandard(c))
	c.SetClock(config.Clock())

	daemon := config.KeybaseService().(*KeybaseDaemonLocal)
	loggedInUID, ok := daemon.asserts[string(loggedInUser)]
	if !ok {
		panic("bad test: unknown user: " + loggedInUser)
	}

	var localUsers []LocalUser
	for _, u := range daemon.localUsers {
		localUsers = append(localUsers, u)
	}
	newDaemon := NewKeybaseDaemonMemory(loggedInUID, localUsers, c.Codec())
	c.SetKeybaseService(newDaemon)
	c.SetKBPKI(NewKBPKIClient(c))

	signingKey := MakeLocalUserSigningKeyOrBust(loggedInUser)
	cryptPrivateKey := MakeLocalUserCryptPrivateKeyOrBust(loggedInUser)
	crypto := NewCryptoLocal(config, signingKey, cryptPrivateKey)
	c.SetCrypto(crypto)
	c.noBGFlush = config.noBGFlush

	if s, ok := config.BlockServer().(*BlockServerRemote); ok {
		blockServer := NewBlockServerRemote(c, s.RemoteAddress(), env.NewContext())
		c.SetBlockServer(blockServer)
	} else {
		c.SetBlockServer(config.BlockServer())
	}

	var mdServer MDServer
	var keyServer KeyServer
	if s, ok := config.MDServer().(*MDServerRemote); ok {
		// connect to server
		mdServer = NewMDServerRemote(c, s.RemoteAddress(), env.NewContext())
		// for now the MD server also acts as the key server.
		keyServer = mdServer.(*MDServerRemote)
	} else {
		// copy the existing mdServer but update the config
		// this way the current device KID is paired with
		// the proper user yet the DB state is all shared.
		mdServerToCopy := config.MDServer().(mdServerLocal)
		mdServer = mdServerToCopy.copy(c)

		// use the same db but swap configs
		keyServerToCopy := config.KeyServer().(*KeyServerLocal)
		keyServer = keyServerToCopy.copy(c)
	}
	c.SetMDServer(mdServer)
	c.SetKeyServer(keyServer)

	// Keep track of all the other configs in a shared slice.
	c.allKnownConfigsForTesting = config.allKnownConfigsForTesting
	*c.allKnownConfigsForTesting = append(*c.allKnownConfigsForTesting, c)

	return c
}

// FakeTlfID creates a fake public or private TLF ID from the given
// byte.
func FakeTlfID(b byte, public bool) TlfID {
	bytes := [TlfIDByteLen]byte{b}
	if public {
		bytes[TlfIDByteLen-1] = PubTlfIDSuffix
	} else {
		bytes[TlfIDByteLen-1] = TlfIDSuffix
	}
	return TlfID{bytes}
}

func fakeTlfIDByte(id TlfID) byte {
	return id.id[0]
}

// FakeBranchID creates a fake branch ID from the given
// byte.
func FakeBranchID(b byte) BranchID {
	bytes := [BranchIDByteLen]byte{b}
	return BranchID{bytes}
}

// NewEmptyTLFWriterKeyBundle creates a new empty TLFWriterKeyBundle
func NewEmptyTLFWriterKeyBundle() TLFWriterKeyBundle {
	return TLFWriterKeyBundle{
		WKeys: make(UserDeviceKeyInfoMap, 0),
	}
}

// NewEmptyTLFReaderKeyBundle creates a new empty TLFReaderKeyBundle
func NewEmptyTLFReaderKeyBundle() TLFReaderKeyBundle {
	return TLFReaderKeyBundle{
		RKeys: make(UserDeviceKeyInfoMap, 0),
	}
}

// AddNewKeysOrBust adds new keys to root metadata and blows up on error.
func AddNewKeysOrBust(t logger.TestLogBackend, rmd *RootMetadata, wkb TLFWriterKeyBundle, rkb TLFReaderKeyBundle) {
	if err := rmd.AddNewKeys(wkb, rkb); err != nil {
		t.Fatal(err)
	}
}

func keySaltForUserDevice(name libkb.NormalizedUsername,
	index int) libkb.NormalizedUsername {
	if index > 0 {
		// We can't include the device index when it's 0, because we
		// have to match what's done in MakeLocalUsers.
		return libkb.NormalizedUsername(string(name) + " " + string(index))
	}
	return name
}

func makeFakeKeys(name libkb.NormalizedUsername, index int) (
	CryptPublicKey, VerifyingKey) {
	keySalt := keySaltForUserDevice(name, index)
	newCryptPublicKey := MakeLocalUserCryptPublicKeyOrBust(keySalt)
	newVerifyingKey := MakeLocalUserVerifyingKeyOrBust(keySalt)
	return newCryptPublicKey, newVerifyingKey
}

// AddDeviceForLocalUserOrBust creates a new device for a user and
// returns the index for that device.
func AddDeviceForLocalUserOrBust(t logger.TestLogBackend, config Config,
	uid keybase1.UID) int {
	kbd, ok := config.KeybaseService().(*KeybaseDaemonLocal)
	if !ok {
		t.Fatal("Bad keybase daemon")
	}

	index, err := kbd.addDeviceForTesting(uid, makeFakeKeys)
	if err != nil {
		t.Fatal(err.Error())
	}
	return index
}

// RevokeDeviceForLocalUserOrBust revokes a device for a user in the
// given index.
func RevokeDeviceForLocalUserOrBust(t logger.TestLogBackend, config Config,
	uid keybase1.UID, index int) {
	kbd, ok := config.KeybaseService().(*KeybaseDaemonLocal)
	if !ok {
		t.Fatal("Bad keybase daemon")
	}

	if err := kbd.revokeDeviceForTesting(
		config.Clock(), uid, index); err != nil {
		t.Fatal(err.Error())
	}
}

// SwitchDeviceForLocalUserOrBust switches the current user's current device
func SwitchDeviceForLocalUserOrBust(t logger.TestLogBackend, config Config, index int) {
	name, uid, err := config.KBPKI().GetCurrentUserInfo(context.Background())
	if err != nil {
		t.Fatalf("Couldn't get UID: %v", err)
	}

	kbd, ok := config.KeybaseService().(*KeybaseDaemonLocal)
	if !ok {
		t.Fatal("Bad keybase daemon")
	}

	if err := kbd.switchDeviceForTesting(uid, index); err != nil {
		t.Fatal(err.Error())
	}

	if _, ok := config.Crypto().(CryptoLocal); !ok {
		t.Fatal("Bad crypto")
	}

	keySalt := keySaltForUserDevice(name, index)
	signingKey := MakeLocalUserSigningKeyOrBust(keySalt)
	cryptPrivateKey := MakeLocalUserCryptPrivateKeyOrBust(keySalt)
	config.SetCrypto(NewCryptoLocal(config, signingKey, cryptPrivateKey))
}

// AddNewAssertionForTest makes newAssertion, which should be a single
// assertion that doesn't already resolve to anything, resolve to the
// same UID as oldAssertion, which should be an arbitrary assertion
// that does already resolve to something.  It only applies to the
// given config.
func AddNewAssertionForTest(
	config Config, oldAssertion, newAssertion string) error {
	kbd, ok := config.KeybaseService().(*KeybaseDaemonLocal)
	if !ok {
		return errors.New("Bad keybase daemon")
	}

	uid, err := kbd.addNewAssertionForTest(oldAssertion, newAssertion)
	if err != nil {
		return err
	}

	// Let the mdserver know about the name change
	md, ok := config.MDServer().(mdServerLocal)
	if !ok {
		return errors.New("Bad md server")
	}
	// If this function is called multiple times for different
	// configs, it may end up invoking the following call more than
	// once on the shared md databases.  That's ok though, it's an
	// idempotent call.
	newSocialAssertion, ok := externals.NormalizeSocialAssertion(newAssertion)
	if !ok {
		return fmt.Errorf("%s couldn't be parsed as a social assertion", newAssertion)
	}
	if err := md.addNewAssertionForTest(uid, newSocialAssertion); err != nil {
		return fmt.Errorf("Couldn't update md server: %v", err)
	}
	return nil
}

// AddNewAssertionForTestOrBust is like AddNewAssertionForTest, but
// dies if there's an error.
func AddNewAssertionForTestOrBust(t logger.TestLogBackend, config Config,
	oldAssertion, newAssertion string) {
	err := AddNewAssertionForTest(config, oldAssertion, newAssertion)
	if err != nil {
		t.Fatal(err)
	}
}

func testRPCWithCanceledContext(t logger.TestLogBackend,
	serverConn net.Conn, fn func(context.Context) error) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Wait for RPC in fn to make progress.
		n, err := serverConn.Read([]byte{1})
		assert.Equal(t, n, 1)
		assert.NoError(t, err)
		cancel()
	}()

	err := fn(ctx)
	if err != context.Canceled {
		t.Fatalf("Function did not return a canceled error: %v", err)
	}
}

// DisableUpdatesForTesting stops the given folder from acting on new
// updates.  Send a struct{}{} down the returned channel to restart
// notifications
func DisableUpdatesForTesting(config Config, folderBranch FolderBranch) (
	chan<- struct{}, error) {
	kbfsOps, ok := config.KBFSOps().(*KBFSOpsStandard)
	if !ok {
		return nil, errors.New("Unexpected KBFSOps type")
	}

	ops := kbfsOps.getOpsNoAdd(folderBranch)
	c := make(chan struct{})
	ops.updatePauseChan <- c
	return c, nil
}

// DisableCRForTesting stops conflict resolution for the given folder.
// RestartCRForTesting should be called to restart it.
func DisableCRForTesting(config Config, folderBranch FolderBranch) error {
	kbfsOps, ok := config.KBFSOps().(*KBFSOpsStandard)
	if !ok {
		return errors.New("Unexpected KBFSOps type")
	}

	ops := kbfsOps.getOpsNoAdd(folderBranch)
	ops.cr.Pause()
	return nil
}

// RestartCRForTesting re-enables conflict resolution for
// the given folder.
func RestartCRForTesting(baseCtx context.Context, config Config,
	folderBranch FolderBranch) error {
	kbfsOps, ok := config.KBFSOps().(*KBFSOpsStandard)
	if !ok {
		return errors.New("Unexpected KBFSOps type")
	}

	ops := kbfsOps.getOpsNoAdd(folderBranch)
	ops.cr.Restart(baseCtx)

	// Start a resolution for anything we've missed.
	lState := makeFBOLockState()
	if !ops.isMasterBranch(lState) {
		ops.cr.Resolve(ops.getCurrMDRevision(lState),
			MetadataRevisionUninitialized)
	}
	return nil
}

// ForceQuotaReclamationForTesting kicks off quota reclamation under
// the given config, for the given folder-branch.
func ForceQuotaReclamationForTesting(config Config,
	folderBranch FolderBranch) error {
	kbfsOps, ok := config.KBFSOps().(*KBFSOpsStandard)
	if !ok {
		return errors.New("Unexpected KBFSOps type")
	}

	ops := kbfsOps.getOpsNoAdd(folderBranch)
	ops.fbm.forceQuotaReclamation()
	return nil
}

// TestClock returns a set time as the current time.
type TestClock struct {
	l sync.Mutex
	t time.Time
}

func newTestClockNow() *TestClock {
	return &TestClock{t: time.Now()}
}

func newTestClockAndTimeNow() (*TestClock, time.Time) {
	t0 := time.Now()
	return &TestClock{t: t0}, t0
}

// Now implements the Clock interface for TestClock.
func (tc *TestClock) Now() time.Time {
	tc.l.Lock()
	defer tc.l.Unlock()
	return tc.t
}

// Set sets the test clock time.
func (tc *TestClock) Set(t time.Time) {
	tc.l.Lock()
	defer tc.l.Unlock()
	tc.t = t
}

// Add adds to the test clock time.
func (tc *TestClock) Add(d time.Duration) {
	tc.l.Lock()
	defer tc.l.Unlock()
	tc.t = tc.t.Add(d)
}

// CheckConfigAndShutdown shuts down the given config, but fails the
// test if there's an error.
func CheckConfigAndShutdown(t logger.TestLogBackend, config Config) {
	if err := config.Shutdown(); err != nil {
		t.Errorf(err.Error())
	}
}

// GetRootNodeForTest gets the root node for the given TLF name, which
// must be canonical, creating it if necessary.
func GetRootNodeForTest(config Config, name string, public bool) (Node, error) {
	ctx := context.Background()
	h, err := ParseTlfHandle(ctx, config.KBPKI(), name, public)
	if err != nil {
		return nil, err
	}

	n, _, err := config.KBFSOps().GetOrCreateRootNode(ctx, h, MasterBranch)
	if err != nil {
		return nil, err
	}

	return n, nil
}

// GetRootNodeOrBust gets the root node for the given TLF name, which
// must be canonical, creating it if necessary, and failing if there's
// an error.
func GetRootNodeOrBust(
	t logger.TestLogBackend, config Config, name string, public bool) Node {
	n, err := GetRootNodeForTest(config, name, public)
	if err != nil {
		t.Fatalf("Couldn't get root node for %s (public=%t): %v",
			name, public, err)
	}
	return n
}
