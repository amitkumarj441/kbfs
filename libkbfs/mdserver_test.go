// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"testing"

	"github.com/keybase/client/go/protocol"
	"github.com/stretchr/testify/require"

	"golang.org/x/net/context"
)

func makeRMDSForTest(t *testing.T, id TlfID, h BareTlfHandle,
	revision MetadataRevision, uid keybase1.UID,
	prevRoot MdID) *RootMetadataSigned {
	rmds, err := NewRootMetadataSignedForTest(id, h)
	require.NoError(t, err)
	rmds.MD.SetSerializedPrivateMetadata([]byte{0x1})
	rmds.MD.SetRevision(revision)
	rmds.MD.SetLastModifyingWriter(uid)
	rmds.MD.SetLastModifyingUser(uid)
	rmds.MD.FakeInitialRekey(h)
	rmds.MD.SetPrevRoot(prevRoot)
	return rmds
}

func signRMDSForTest(t *testing.T, codec Codec, signer cryptoSigner,
	rmds *RootMetadataSigned) {
	ctx := context.Background()

	// Encode and sign writer metadata.
	buf, err := rmds.MD.GetSerializedWriterMetadata(codec)
	require.NoError(t, err)

	sigInfo, err := signer.Sign(ctx, buf)
	require.NoError(t, err)
	rmds.MD.SetWriterMetadataSigInfo(sigInfo)

	// Encode and sign root metadata.
	buf, err = codec.Encode(rmds.MD)
	require.NoError(t, err)

	sigInfo, err = signer.Sign(ctx, buf)
	require.NoError(t, err)
	rmds.SigInfo = sigInfo
}

// This should pass for both local and remote servers.
func TestMDServerBasics(t *testing.T) {
	// setup
	config := MakeTestConfigOrBust(t, "test_user")
	defer config.Shutdown()
	mdServer := config.MDServer()
	ctx := context.Background()

	_, uid, err := config.KBPKI().GetCurrentUserInfo(ctx)
	require.NoError(t, err)

	// (1) get metadata -- allocates an ID
	h, err := MakeBareTlfHandle([]keybase1.UID{uid}, nil, nil, nil, nil)
	require.NoError(t, err)

	id, rmds, err := mdServer.GetForHandle(ctx, h, Merged)
	require.NoError(t, err)
	require.Nil(t, rmds)

	// (2) push some new metadata blocks
	prevRoot := MdID{}
	middleRoot := MdID{}
	for i := MetadataRevision(1); i <= 10; i++ {
		rmds := makeRMDSForTest(t, id, h, i, uid, prevRoot)
		signRMDSForTest(t, config.Codec(), config.Crypto(), rmds)
		err = mdServer.Put(ctx, rmds)
		require.NoError(t, err)
		prevRoot, err = config.Crypto().MakeMdID(rmds.MD)
		require.NoError(t, err)
		if i == 5 {
			middleRoot = prevRoot
		}
	}

	// (3) trigger a conflict
	rmds = makeRMDSForTest(t, id, h, 10, uid, prevRoot)
	signRMDSForTest(t, config.Codec(), config.Crypto(), rmds)
	err = mdServer.Put(ctx, rmds)
	require.IsType(t, MDServerErrorConflictRevision{}, err)

	// (4) push some new unmerged metadata blocks linking to the
	//     middle merged block.
	prevRoot = middleRoot
	bid, err := config.Crypto().MakeRandomBranchID()
	require.NoError(t, err)
	for i := MetadataRevision(6); i < 41; i++ {
		rmds := makeRMDSForTest(t, id, h, i, uid, prevRoot)
		rmds.MD.SetUnmerged()
		rmds.MD.SetBranchID(bid)
		signRMDSForTest(t, config.Codec(), config.Crypto(), rmds)
		err = mdServer.Put(ctx, rmds)
		require.NoError(t, err)
		prevRoot, err = config.Crypto().MakeMdID(rmds.MD)
		require.NoError(t, err)
	}

	// (5) check for proper unmerged head
	head, err := mdServer.GetForTLF(ctx, id, bid, Unmerged)
	require.NoError(t, err)
	require.NotNil(t, head)
	require.Equal(t, MetadataRevision(40), head.MD.RevisionNumber())

	// (6a) try to get unmerged range
	rmdses, err := mdServer.GetRange(ctx, id, bid, Unmerged, 1, 100)
	require.NoError(t, err)
	require.Equal(t, 35, len(rmdses))
	for i := MetadataRevision(6); i < 41; i++ {
		require.Equal(t, i, rmdses[i-6].MD.RevisionNumber())
	}

	// (6b) try to get unmerged range subset.
	rmdses, err = mdServer.GetRange(ctx, id, bid, Unmerged, 7, 14)
	require.NoError(t, err)
	require.Equal(t, 8, len(rmdses))
	for i := MetadataRevision(7); i <= 14; i++ {
		require.Equal(t, i, rmdses[i-7].MD.RevisionNumber())
	}

	// (7) prune unmerged
	err = mdServer.PruneBranch(ctx, id, bid)
	require.NoError(t, err)

	// (8) verify head is pruned
	head, err = mdServer.GetForTLF(ctx, id, NullBranchID, Unmerged)
	require.NoError(t, err)
	require.Nil(t, head)

	// (9) verify revision history is pruned
	rmdses, err = mdServer.GetRange(ctx, id, NullBranchID, Unmerged, 1, 100)
	require.NoError(t, err)
	require.Equal(t, 0, len(rmdses))

	// (10) check for proper merged head
	head, err = mdServer.GetForTLF(ctx, id, NullBranchID, Merged)
	require.NoError(t, err)
	require.NotNil(t, head)
	require.Equal(t, MetadataRevision(10), head.MD.RevisionNumber())

	// (11) try to get merged range
	rmdses, err = mdServer.GetRange(ctx, id, NullBranchID, Merged, 1, 100)
	require.NoError(t, err)
	require.Equal(t, 10, len(rmdses))
	for i := MetadataRevision(1); i <= 10; i++ {
		require.Equal(t, i, rmdses[i-1].MD.RevisionNumber())
	}
}

// This should pass for both local and remote servers. Make sure that
// registering multiple TLFs for updates works. This is a regression
// test for https://keybase.atlassian.net/browse/KBFS-467 .
func TestMDServerRegisterForUpdate(t *testing.T) {
	// setup
	config := MakeTestConfigOrBust(t, "test_user")
	defer config.Shutdown()
	mdServer := config.MDServer()
	ctx := context.Background()

	_, uid, err := config.KBPKI().GetCurrentUserInfo(ctx)
	require.NoError(t, err)

	// Create first TLF.
	h1, err := MakeBareTlfHandle([]keybase1.UID{uid}, nil, nil, nil, nil)
	require.NoError(t, err)

	id1, _, err := mdServer.GetForHandle(ctx, h1, Merged)
	require.NoError(t, err)

	// Create second TLF, which should end up being different from
	// the first one.
	h2, err := MakeBareTlfHandle([]keybase1.UID{uid}, []keybase1.UID{keybase1.PUBLIC_UID}, nil, nil, nil)
	require.NoError(t, err)

	id2, _, err := mdServer.GetForHandle(ctx, h2, Merged)
	require.NoError(t, err)
	require.NotEqual(t, id1, id2)

	_, err = mdServer.RegisterForUpdate(ctx, id1, MetadataRevisionInitial)
	require.NoError(t, err)

	_, err = mdServer.RegisterForUpdate(ctx, id2, MetadataRevisionInitial)
	require.NoError(t, err)
}
