// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"errors"
	"io/ioutil"
	"os"
	"testing"

	"golang.org/x/net/context"

	"github.com/keybase/client/go/logger"
	keybase1 "github.com/keybase/client/go/protocol"
	"github.com/stretchr/testify/require"
)

type singleEncryptionKeyGetter struct {
	k TLFCryptKey
}

func (g singleEncryptionKeyGetter) GetTLFCryptKeyForEncryption(
	ctx context.Context, kmd KeyMetadata) (TLFCryptKey, error) {
	return g.k, nil
}

func getTlfJournalLength(t *testing.T, j *mdJournal) int {
	len, err := j.length()
	require.NoError(t, err)
	return int(len)
}

func setupMDJournalTest(t *testing.T) (
	codec Codec, crypto CryptoCommon,
	uid keybase1.UID, id TlfID, h BareTlfHandle,
	signer cryptoSigner, verifyingKey VerifyingKey,
	ekg singleEncryptionKeyGetter, bsplit BlockSplitter,
	tempdir string, j *mdJournal) {
	codec = NewCodecMsgpack()
	crypto = MakeCryptoCommon(codec)

	uid = keybase1.MakeTestUID(1)
	id = FakeTlfID(1, false)
	h, err := MakeBareTlfHandle([]keybase1.UID{uid}, nil, nil, nil, nil)
	require.NoError(t, err)

	signingKey := MakeFakeSigningKeyOrBust("fake seed")
	signer = cryptoSignerLocal{signingKey}
	verifyingKey = signingKey.GetVerifyingKey()
	ekg = singleEncryptionKeyGetter{MakeTLFCryptKey([32]byte{0x1})}

	// Do this last so we don't have to worry about cleaning up
	// the tempdir if anything else errors.
	tempdir, err = ioutil.TempDir(os.TempDir(), "mdserver_tlf_journal")
	require.NoError(t, err)

	log := logger.NewTestLogger(t)
	j, err = makeMDJournal(codec, crypto, tempdir, log)
	require.NoError(t, err)

	bsplit = &BlockSplitterSimple{64 * 1024, 8 * 1024}

	return codec, crypto, uid, id, h, signer, verifyingKey, ekg,
		bsplit, tempdir, j
}

func teardownMDJournalTest(t *testing.T, tempdir string) {
	err := os.RemoveAll(tempdir)
	require.NoError(t, err)
}

func makeMDForTest(t *testing.T, id TlfID, h BareTlfHandle,
	revision MetadataRevision, uid keybase1.UID,
	prevRoot MdID) *RootMetadata {
	md := NewRootMetadata()
	err := md.Update(id, h)
	require.NoError(t, err)
	md.SetRevision(revision)
	md.FakeInitialRekey(h)
	md.SetPrevRoot(prevRoot)
	return md
}

func TestMDJournalBasic(t *testing.T) {
	codec, crypto, uid, id, h, signer, verifyingKey, ekg, bsplit, tempdir, j :=
		setupMDJournalTest(t)
	defer teardownMDJournalTest(t, tempdir)

	// Should start off as empty.

	head, err := j.getHead(uid)
	require.NoError(t, err)
	require.Equal(t, ImmutableBareRootMetadata{}, head)
	require.Equal(t, 0, getTlfJournalLength(t, j))

	// Push some new metadata blocks.

	ctx := context.Background()

	firstRevision := MetadataRevision(10)
	firstPrevRoot := fakeMdID(1)
	mdCount := 10

	prevRoot := firstPrevRoot
	for i := 0; i < mdCount; i++ {
		revision := firstRevision + MetadataRevision(i)
		md := makeMDForTest(t, id, h, revision, uid, prevRoot)
		mdID, err := j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
		require.NoError(t, err)
		prevRoot = mdID
	}

	require.Equal(t, mdCount, getTlfJournalLength(t, j))

	// Should now be non-empty.

	ibrmds, err := j.getRange(
		uid, 1, firstRevision+MetadataRevision(2*mdCount))
	require.NoError(t, err)
	require.Equal(t, mdCount, len(ibrmds))

	require.Equal(t, firstRevision, ibrmds[0].RevisionNumber())
	require.Equal(t, firstPrevRoot, ibrmds[0].GetPrevRoot())
	err = ibrmds[0].IsValidAndSigned(codec, crypto)
	require.NoError(t, err)
	err = ibrmds[0].IsLastModifiedBy(uid, verifyingKey)
	require.NoError(t, err)

	for i := 1; i < len(ibrmds); i++ {
		err := ibrmds[i].IsValidAndSigned(codec, crypto)
		require.NoError(t, err)
		err = ibrmds[i].IsLastModifiedBy(uid, verifyingKey)
		require.NoError(t, err)
		err = ibrmds[i-1].CheckValidSuccessor(
			ibrmds[i-1].mdID, ibrmds[i].BareRootMetadata)
		require.NoError(t, err)
	}

	head, err = j.getHead(uid)
	require.NoError(t, err)
	require.Equal(t, ibrmds[len(ibrmds)-1], head)
}

func TestMDJournalReplaceHead(t *testing.T) {
	_, _, uid, id, h, signer, verifyingKey, ekg, bsplit, tempdir, j :=
		setupMDJournalTest(t)
	defer teardownMDJournalTest(t, tempdir)

	// Push some new metadata blocks.

	ctx := context.Background()

	firstRevision := MetadataRevision(10)
	firstPrevRoot := fakeMdID(1)
	mdCount := 3

	prevRoot := firstPrevRoot
	for i := 0; i < mdCount; i++ {
		revision := firstRevision + MetadataRevision(i)
		md := makeMDForTest(t, id, h, revision, uid, prevRoot)
		mdID, err := j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
		md.SetDiskUsage(500)
		require.NoError(t, err)
		prevRoot = mdID
	}

	// Should just replace the head.

	revision := firstRevision + MetadataRevision(mdCount) - 1
	md := makeMDForTest(t, id, h, revision, uid, prevRoot)
	md.SetDiskUsage(501)
	_, err := j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
	require.NoError(t, err)

	head, err := j.getHead(uid)
	require.NoError(t, err)
	require.Equal(t, md.Revision(), head.RevisionNumber())
	require.Equal(t, md.DiskUsage(), head.DiskUsage())
}

func TestMDJournalBranchConversion(t *testing.T) {
	codec, crypto, uid, id, h, signer, verifyingKey, ekg, bsplit, tempdir, j :=
		setupMDJournalTest(t)
	defer teardownMDJournalTest(t, tempdir)

	ctx := context.Background()

	firstRevision := MetadataRevision(10)
	firstPrevRoot := fakeMdID(1)
	mdCount := 10

	prevRoot := firstPrevRoot
	for i := 0; i < mdCount; i++ {
		revision := firstRevision + MetadataRevision(i)
		md := makeMDForTest(t, id, h, revision, uid, prevRoot)
		mdID, err := j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
		require.NoError(t, err)
		prevRoot = mdID
	}

	err := j.convertToBranch(ctx, signer, uid, verifyingKey)
	require.NoError(t, err)

	ibrmds, err := j.getRange(
		uid, 1, firstRevision+MetadataRevision(2*mdCount))
	require.NoError(t, err)
	require.Equal(t, mdCount, len(ibrmds))

	require.Equal(t, firstRevision, ibrmds[0].RevisionNumber())
	require.Equal(t, firstPrevRoot, ibrmds[0].GetPrevRoot())
	require.Equal(t, Unmerged, ibrmds[0].MergedStatus())
	err = ibrmds[0].IsValidAndSigned(codec, crypto)
	require.NoError(t, err)
	err = ibrmds[0].IsLastModifiedBy(uid, verifyingKey)
	require.NoError(t, err)

	bid := ibrmds[0].BID()
	require.NotEqual(t, NullBranchID, bid)

	for i := 1; i < len(ibrmds); i++ {
		require.Equal(t, Unmerged, ibrmds[i].MergedStatus())
		require.Equal(t, bid, ibrmds[i].BID())
		err := ibrmds[i].IsValidAndSigned(codec, crypto)
		require.NoError(t, err)
		err = ibrmds[i].IsLastModifiedBy(uid, verifyingKey)
		require.NoError(t, err)
		err = ibrmds[i-1].CheckValidSuccessor(
			ibrmds[i-1].mdID, ibrmds[i].BareRootMetadata)
		require.NoError(t, err)
	}

	require.Equal(t, 10, getTlfJournalLength(t, j))

	head, err := j.getHead(uid)
	require.NoError(t, err)
	require.Equal(t, ibrmds[len(ibrmds)-1], head)
}

type limitedCryptoSigner struct {
	cryptoSigner
	remaining int
}

func (s *limitedCryptoSigner) Sign(ctx context.Context, msg []byte) (
	SignatureInfo, error) {
	if s.remaining <= 0 {
		return SignatureInfo{}, errors.New("No more Sign calls left")
	}
	s.remaining--
	return s.cryptoSigner.Sign(ctx, msg)
}

func TestMDJournalBranchConversionAtomic(t *testing.T) {
	codec, crypto, uid, id, h, signer, verifyingKey, ekg, bsplit, tempdir, j :=
		setupMDJournalTest(t)
	defer teardownMDJournalTest(t, tempdir)

	ctx := context.Background()

	firstRevision := MetadataRevision(10)
	firstPrevRoot := fakeMdID(1)
	mdCount := 10

	prevRoot := firstPrevRoot
	for i := 0; i < mdCount; i++ {
		revision := firstRevision + MetadataRevision(i)
		md := makeMDForTest(t, id, h, revision, uid, prevRoot)
		mdID, err := j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
		require.NoError(t, err)
		prevRoot = mdID
	}

	limitedSigner := limitedCryptoSigner{signer, 5}

	err := j.convertToBranch(ctx, &limitedSigner, uid, verifyingKey)
	require.NotNil(t, err)

	// All entries should remain unchanged, since the conversion
	// encountered an error.

	ibrmds, err := j.getRange(
		uid, 1, firstRevision+MetadataRevision(2*mdCount))
	require.NoError(t, err)
	require.Equal(t, mdCount, len(ibrmds))

	require.Equal(t, firstRevision, ibrmds[0].RevisionNumber())
	require.Equal(t, firstPrevRoot, ibrmds[0].GetPrevRoot())
	require.Equal(t, Merged, ibrmds[0].MergedStatus())
	err = ibrmds[0].IsValidAndSigned(codec, crypto)
	require.NoError(t, err)
	err = ibrmds[0].IsLastModifiedBy(uid, verifyingKey)
	require.NoError(t, err)

	for i := 1; i < len(ibrmds); i++ {
		require.Equal(t, Merged, ibrmds[i].MergedStatus())
		require.Equal(t, NullBranchID, ibrmds[i].BID())
		err := ibrmds[i].IsValidAndSigned(codec, crypto)
		require.NoError(t, err)
		err = ibrmds[i].IsLastModifiedBy(uid, verifyingKey)
		require.NoError(t, err)
		err = ibrmds[i-1].CheckValidSuccessor(
			ibrmds[i-1].mdID, ibrmds[i].BareRootMetadata)
		require.NoError(t, err)
	}

	require.Equal(t, 10, getTlfJournalLength(t, j))

	head, err := j.getHead(uid)
	require.NoError(t, err)
	require.Equal(t, ibrmds[len(ibrmds)-1], head)
}

type shimMDServer struct {
	MDServer
	rmdses       []*RootMetadataSigned
	nextGetRange []*RootMetadataSigned
	nextErr      error
}

func (s *shimMDServer) GetRange(
	ctx context.Context, id TlfID, bid BranchID, mStatus MergeStatus,
	start, stop MetadataRevision) ([]*RootMetadataSigned, error) {
	rmdses := s.nextGetRange
	s.nextGetRange = nil
	return rmdses, nil
}

func (s *shimMDServer) Put(
	ctx context.Context, rmds *RootMetadataSigned) error {
	if s.nextErr != nil {
		err := s.nextErr
		s.nextErr = nil
		return err
	}
	s.rmdses = append(s.rmdses, rmds)

	// Pretend all cancels happen after the actual put.
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	return nil
}

func TestMDJournalFlushBasic(t *testing.T) {
	codec, crypto, uid, id, h, signer, verifyingKey, ekg, bsplit, tempdir, j :=
		setupMDJournalTest(t)
	defer teardownMDJournalTest(t, tempdir)

	ctx := context.Background()

	firstRevision := MetadataRevision(10)
	firstPrevRoot := fakeMdID(1)
	mdCount := 10

	prevRoot := firstPrevRoot
	for i := 0; i < mdCount; i++ {
		revision := firstRevision + MetadataRevision(i)
		md := makeMDForTest(t, id, h, revision, uid, prevRoot)
		mdID, err := j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
		require.NoError(t, err)
		prevRoot = mdID
	}

	// Flush all entries.
	var mdserver shimMDServer
	for i := 0; i < mdCount; i++ {
		flushed, err := j.flushOne(
			ctx, signer, uid, verifyingKey, &mdserver)
		require.NoError(t, err)
		require.True(t, flushed)
	}
	flushed, err := j.flushOne(ctx, signer, uid, verifyingKey, &mdserver)
	require.NoError(t, err)
	require.False(t, flushed)
	require.Equal(t, 0, getTlfJournalLength(t, j))

	rmdses := mdserver.rmdses
	require.Equal(t, mdCount, len(rmdses))

	// Check RMDSes on the server.

	require.Equal(t, firstRevision, rmdses[0].MD.RevisionNumber())
	require.Equal(t, firstPrevRoot, rmdses[0].MD.GetPrevRoot())
	err = rmdses[0].IsValidAndSigned(codec, crypto)
	require.NoError(t, err)
	err = rmdses[0].IsLastModifiedBy(uid, verifyingKey)
	require.NoError(t, err)

	for i := 1; i < len(rmdses); i++ {
		err := rmdses[i].IsValidAndSigned(codec, crypto)
		require.NoError(t, err)
		err = rmdses[i].IsLastModifiedBy(uid, verifyingKey)
		require.NoError(t, err)
		prevID, err := crypto.MakeMdID(rmdses[i-1].MD)
		require.NoError(t, err)
		err = rmdses[i-1].MD.CheckValidSuccessor(prevID, rmdses[i].MD)
		require.NoError(t, err)
	}
}

func TestMDJournalFlushConflict(t *testing.T) {
	codec, crypto, uid, id, h, signer, verifyingKey, ekg, bsplit, tempdir, j :=
		setupMDJournalTest(t)
	defer teardownMDJournalTest(t, tempdir)

	ctx := context.Background()

	firstRevision := MetadataRevision(10)
	firstPrevRoot := fakeMdID(1)
	mdCount := 10

	prevRoot := firstPrevRoot
	for i := 0; i < mdCount/2; i++ {
		revision := firstRevision + MetadataRevision(i)
		md := makeMDForTest(t, id, h, revision, uid, prevRoot)
		mdID, err := j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
		require.NoError(t, err)
		prevRoot = mdID
	}

	var mdserver shimMDServer
	mdserver.nextErr = MDServerErrorConflictRevision{}

	// Simulate a flush with a conflict error halfway through.
	{
		flushed, err := j.flushOne(
			ctx, signer, uid, verifyingKey, &mdserver)
		require.NoError(t, err)
		require.True(t, flushed)

		revision := firstRevision + MetadataRevision(mdCount/2)
		md := makeMDForTest(t, id, h, revision, uid, prevRoot)
		_, err = j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
		require.IsType(t, MDJournalConflictError{}, err)

		md.SetUnmerged()
		mdID, err := j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
		require.NoError(t, err)
		prevRoot = mdID
	}

	for i := mdCount/2 + 1; i < mdCount; i++ {
		revision := firstRevision + MetadataRevision(i)
		md := makeMDForTest(t, id, h, revision, uid, prevRoot)
		md.SetUnmerged()
		mdID, err := j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
		require.NoError(t, err)
		prevRoot = mdID
	}

	// Flush remaining entries.
	for i := 0; i < mdCount-1; i++ {
		flushed, err := j.flushOne(
			ctx, signer, uid, verifyingKey, &mdserver)
		require.NoError(t, err)
		require.True(t, flushed)
	}
	flushed, err := j.flushOne(ctx, signer, uid, verifyingKey, &mdserver)
	require.NoError(t, err)
	require.False(t, flushed)
	require.Equal(t, 0, getTlfJournalLength(t, j))

	rmdses := mdserver.rmdses
	require.Equal(t, mdCount, len(rmdses))

	// Check RMDSes on the server.

	require.Equal(t, firstRevision, rmdses[0].MD.RevisionNumber())
	require.Equal(t, firstPrevRoot, rmdses[0].MD.GetPrevRoot())
	require.Equal(t, Unmerged, rmdses[0].MD.MergedStatus())
	err = rmdses[0].IsValidAndSigned(codec, crypto)
	require.NoError(t, err)
	err = rmdses[0].IsLastModifiedBy(uid, verifyingKey)
	require.NoError(t, err)

	bid := rmdses[0].MD.BID()
	require.NotEqual(t, NullBranchID, bid)

	for i := 1; i < len(rmdses); i++ {
		require.Equal(t, Unmerged, rmdses[i].MD.MergedStatus())
		require.Equal(t, bid, rmdses[i].MD.BID())
		err := rmdses[i].IsValidAndSigned(codec, crypto)
		require.NoError(t, err)
		err = rmdses[i].IsLastModifiedBy(uid, verifyingKey)
		require.NoError(t, err)
		prevID, err := crypto.MakeMdID(rmdses[i-1].MD)
		require.NoError(t, err)
		err = rmdses[i-1].MD.CheckValidSuccessor(prevID, rmdses[i].MD)
		require.NoError(t, err)
	}
}

// TestMDJournalPreservesBranchID tests that the branch ID is
// preserved even if the journal is fully drained. This is a
// regression test for KBFS-1344.
func TestMDJournalPreservesBranchID(t *testing.T) {
	codec, crypto, uid, id, h, signer, verifyingKey, ekg, bsplit, tempdir, j :=
		setupMDJournalTest(t)
	defer teardownMDJournalTest(t, tempdir)

	ctx := context.Background()

	firstRevision := MetadataRevision(10)
	firstPrevRoot := fakeMdID(1)
	mdCount := 10

	prevRoot := firstPrevRoot
	for i := 0; i < mdCount-1; i++ {
		revision := firstRevision + MetadataRevision(i)
		md := makeMDForTest(t, id, h, revision, uid, prevRoot)
		mdID, err := j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
		require.NoError(t, err)
		prevRoot = mdID
	}

	var mdserver shimMDServer
	mdserver.nextErr = MDServerErrorConflictRevision{}

	// Flush all entries, with the first one encountering a
	// conflict error.
	for i := 0; i < mdCount-1; i++ {
		flushed, err := j.flushOne(
			ctx, signer, uid, verifyingKey, &mdserver)
		require.NoError(t, err)
		require.True(t, flushed)
	}

	flushed, err := j.flushOne(ctx, signer, uid, verifyingKey, &mdserver)
	require.NoError(t, err)
	require.False(t, flushed)
	require.Equal(t, 0, getTlfJournalLength(t, j))

	// Put last revision and flush it.
	{
		revision := firstRevision + MetadataRevision(mdCount-1)
		md := makeMDForTest(t, id, h, revision, uid, prevRoot)
		mdID, err := j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
		require.IsType(t, MDJournalConflictError{}, err)

		md.SetUnmerged()
		mdID, err = j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
		require.NoError(t, err)
		prevRoot = mdID

		flushed, err := j.flushOne(
			ctx, signer, uid, verifyingKey, &mdserver)
		require.NoError(t, err)
		require.True(t, flushed)

		flushed, err = j.flushOne(
			ctx, signer, uid, verifyingKey, &mdserver)
		require.NoError(t, err)
		require.False(t, flushed)
		require.Equal(t, 0, getTlfJournalLength(t, j))
	}

	rmdses := mdserver.rmdses
	require.Equal(t, mdCount, len(rmdses))

	// Check RMDSes on the server. In particular, the BranchID of
	// the last put MD should match the rest.

	require.Equal(t, firstRevision, rmdses[0].MD.RevisionNumber())
	require.Equal(t, firstPrevRoot, rmdses[0].MD.GetPrevRoot())
	require.Equal(t, Unmerged, rmdses[0].MD.MergedStatus())
	err = rmdses[0].IsValidAndSigned(codec, crypto)
	require.NoError(t, err)
	err = rmdses[0].IsLastModifiedBy(uid, verifyingKey)
	require.NoError(t, err)

	bid := rmdses[0].MD.BID()
	require.NotEqual(t, NullBranchID, bid)

	for i := 1; i < len(rmdses); i++ {
		require.Equal(t, Unmerged, rmdses[i].MD.MergedStatus())
		require.Equal(t, bid, rmdses[i].MD.BID())
		err := rmdses[i].IsValidAndSigned(codec, crypto)
		require.NoError(t, err)
		err = rmdses[i].IsLastModifiedBy(uid, verifyingKey)
		require.NoError(t, err)
		prevID, err := crypto.MakeMdID(rmdses[i-1].MD)
		require.NoError(t, err)
		err = rmdses[i-1].MD.CheckValidSuccessor(prevID, rmdses[i].MD)
		require.NoError(t, err)
	}
}

// TestMDJournalDoubleFlush tests that flushing handles the case where
// the correct MD is already present.
func TestMDJournalDoubleFlush(t *testing.T) {
	_, _, uid, id, h, signer, verifyingKey, ekg, bsplit, tempdir, j :=
		setupMDJournalTest(t)
	defer teardownMDJournalTest(t, tempdir)

	ctx := context.Background()

	prevRoot := fakeMdID(1)
	revision := MetadataRevision(10)
	md := makeMDForTest(t, id, h, revision, uid, prevRoot)
	mdID, err := j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
	require.NoError(t, err)
	prevRoot = mdID

	var mdserver shimMDServer

	// Simulate a flush that is cancelled but succeeds anyway.
	ctx2, cancel := context.WithCancel(ctx)
	cancel()
	flushed, err := j.flushOne(ctx2, signer, uid, verifyingKey, &mdserver)
	require.Equal(t, ctx2.Err(), err)
	require.False(t, flushed)
	require.Equal(t, 1, len(mdserver.rmdses))

	// Simulate the conflict error and GetRange call resulting
	// from the successful put.
	mdserver.nextErr = MDServerErrorConflictRevision{}
	mdserver.nextGetRange = mdserver.rmdses
	flushed, err = j.flushOne(ctx, signer, uid, verifyingKey, &mdserver)
	require.NoError(t, err)
	require.True(t, flushed)
	require.Equal(t, Merged, mdserver.rmdses[0].MD.MergedStatus())
}

func TestMDJournalClear(t *testing.T) {
	_, _, uid, id, h, signer, verifyingKey, ekg, bsplit, tempdir, j :=
		setupMDJournalTest(t)
	defer teardownMDJournalTest(t, tempdir)

	ctx := context.Background()

	firstRevision := MetadataRevision(10)
	firstPrevRoot := fakeMdID(1)
	mdCount := 10

	prevRoot := firstPrevRoot
	for i := 0; i < mdCount; i++ {
		revision := firstRevision + MetadataRevision(i)
		md := makeMDForTest(t, id, h, revision, uid, prevRoot)
		mdID, err := j.put(ctx, signer, ekg, bsplit, md, uid, verifyingKey)
		require.NoError(t, err)
		prevRoot = mdID
	}

	err := j.convertToBranch(ctx, signer, uid, verifyingKey)
	require.NoError(t, err)
	require.NotEqual(t, NullBranchID, j.branchID)

	bid := j.branchID

	// Clearing the master branch shouldn't work.
	err = j.clear(ctx, uid, NullBranchID)
	require.Error(t, err)

	// Clearing a different branch ID should do nothing.

	err = j.clear(ctx, uid, FakeBranchID(1))
	require.NoError(t, err)
	require.Equal(t, bid, j.branchID)

	head, err := j.getHead(uid)
	require.NoError(t, err)
	require.NotEqual(t, ImmutableBareRootMetadata{}, head)

	// Clearing the correct branch ID should clear the entire
	// journal, and reset the branch ID.

	err = j.clear(ctx, uid, bid)
	require.NoError(t, err)
	require.Equal(t, NullBranchID, j.branchID)

	head, err = j.getHead(uid)
	require.NoError(t, err)
	require.Equal(t, ImmutableBareRootMetadata{}, head)

	// Clearing twice should do nothing.

	err = j.clear(ctx, uid, bid)
	require.NoError(t, err)
	require.Equal(t, NullBranchID, j.branchID)

	head, err = j.getHead(uid)
	require.NoError(t, err)
	require.Equal(t, ImmutableBareRootMetadata{}, head)
}
