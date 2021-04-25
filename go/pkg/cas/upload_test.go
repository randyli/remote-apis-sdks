package cas

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"google.golang.org/grpc"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/golang/protobuf/proto"
	"github.com/google/go-cmp/cmp"
)

func TestFS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	tmpDir := t.TempDir()
	putFile(t, filepath.Join(tmpDir, "root", "a"), "a")
	aItem := uploadItemFromBlob(filepath.Join(tmpDir, "root", "a"), []byte("a"))

	putFile(t, filepath.Join(tmpDir, "root", "b"), "b")
	bItem := uploadItemFromBlob(filepath.Join(tmpDir, "root", "b"), []byte("b"))

	putFile(t, filepath.Join(tmpDir, "root", "subdir", "c"), "c")
	cItem := uploadItemFromBlob(filepath.Join(tmpDir, "root", "subdir", "c"), []byte("c"))

	subdirItem := uploadItemFromDirMsg(filepath.Join(tmpDir, "root", "subdir"), &repb.Directory{
		Files: []*repb.FileNode{{
			Name:   "c",
			Digest: cItem.Digest,
		}},
	})
	rootItem := uploadItemFromDirMsg(filepath.Join(tmpDir, "root"), &repb.Directory{
		Files: []*repb.FileNode{
			{Name: "a", Digest: aItem.Digest},
			{Name: "b", Digest: bItem.Digest},
		},
		Directories: []*repb.DirectoryNode{
			{Name: "subdir", Digest: subdirItem.Digest},
		},
	})

	putFile(t, filepath.Join(tmpDir, "medium-dir", "medium"), "medium")
	mediumItem := uploadItemFromBlob(filepath.Join(tmpDir, "medium-dir", "medium"), []byte("medium"))
	mediumDirItem := uploadItemFromDirMsg(filepath.Join(tmpDir, "medium-dir"), &repb.Directory{
		Files: []*repb.FileNode{{
			Name:   "medium",
			Digest: mediumItem.Digest,
		}},
	})

	tests := []struct {
		desc                string
		inputs              []*UploadInput
		wantScheduledChecks []*uploadItem
	}{
		{
			desc:                "root",
			inputs:              []*UploadInput{{Path: filepath.Join(tmpDir, "root")}},
			wantScheduledChecks: []*uploadItem{rootItem, aItem, bItem, subdirItem, cItem},
		},
		{
			desc:                "blob",
			inputs:              []*UploadInput{{Content: []byte("foo")}},
			wantScheduledChecks: []*uploadItem{uploadItemFromBlob("digest 2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae/3", []byte("foo"))},
		},
		{
			desc:                "medium",
			inputs:              []*UploadInput{{Path: filepath.Join(tmpDir, "medium-dir")}},
			wantScheduledChecks: []*uploadItem{mediumDirItem, mediumItem},
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			var mu sync.Mutex
			var scheduledCheckCalls []*uploadItem

			client := &Client{
				ClientConfig: DefaultClientConfig(),
				testScheduleCheck: func(ctx context.Context, item *uploadItem) error {
					mu.Lock()
					defer mu.Unlock()
					scheduledCheckCalls = append(scheduledCheckCalls, item)
					return nil
				},
			}
			client.SmallFileThreshold = 5
			client.LargeFileThreshold = 10

			if _, err := client.Upload(ctx, inputChanFrom(tc.inputs...)); err != nil {
				t.Fatalf("failed to upload: %s", err)
			}

			sort.Slice(scheduledCheckCalls, func(i, j int) bool {
				return scheduledCheckCalls[i].Title < scheduledCheckCalls[j].Title
			})
			if diff := cmp.Diff(tc.wantScheduledChecks, scheduledCheckCalls, cmp.Comparer(compareUploadItems)); diff != "" {
				t.Errorf("unexpected scheduled checks (-want +got):\n%s", diff)
			}
		})
	}
}

func TestChecks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var mu sync.Mutex
	var gotDigestChecks []*repb.Digest
	var gotRequestSizes []int
	var gotScheduleUploadCalls []*uploadItem
	cas := &fakeCAS{
		findMissingBlobs: func(ctx context.Context, in *repb.FindMissingBlobsRequest, opts ...grpc.CallOption) (*repb.FindMissingBlobsResponse, error) {
			mu.Lock()
			defer mu.Unlock()
			gotDigestChecks = append(gotDigestChecks, in.BlobDigests...)
			gotRequestSizes = append(gotRequestSizes, len(in.BlobDigests))
			return &repb.FindMissingBlobsResponse{MissingBlobDigests: in.BlobDigests[:1]}, nil
		},
	}
	client := &Client{
		InstanceName: "projects/p/instances/i",
		ClientConfig: DefaultClientConfig(),
		cas:          cas,
		testScheduleUpload: func(ctx context.Context, item *uploadItem) error {
			mu.Lock()
			defer mu.Unlock()
			gotScheduleUploadCalls = append(gotScheduleUploadCalls, item)
			return nil
		},
	}
	client.FindMissingBlobsBatchSize = 2

	inputC := inputChanFrom(
		&UploadInput{Content: []byte("a")},
		&UploadInput{Content: []byte("b")},
		&UploadInput{Content: []byte("c")},
		&UploadInput{Content: []byte("d")},
	)
	if _, err := client.Upload(ctx, inputC); err != nil {
		t.Fatalf("failed to upload: %s", err)
	}

	wantDigestChecks := []*repb.Digest{
		{Hash: "18ac3e7343f016890c510e93f935261169d9e3f565436429830faf0934f4f8e4", SizeBytes: 1},
		{Hash: "2e7d2c03a9507ae265ecf5b5356885a53393a2029d241394997265a1a25aefc6", SizeBytes: 1},
		{Hash: "3e23e8160039594a33894f6564e1b1348bbd7a0088d42c4acb73eeaed59c009d", SizeBytes: 1},
		{Hash: "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb", SizeBytes: 1},
	}
	sort.Slice(gotDigestChecks, func(i, j int) bool {
		return gotDigestChecks[i].Hash < gotDigestChecks[j].Hash
	})
	if diff := cmp.Diff(wantDigestChecks, gotDigestChecks, cmp.Comparer(proto.Equal)); diff != "" {
		t.Error(diff)
	}
	if diff := cmp.Diff([]int{2, 2}, gotRequestSizes); diff != "" {
		t.Error(diff)
	}

	wantDigestUploads := []string{
		"2e7d2c03a9507ae265ecf5b5356885a53393a2029d241394997265a1a25aefc6", // c
		"ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb", // a
	}
	gotDigestUploads := make([]string, len(gotScheduleUploadCalls))
	for i, req := range gotScheduleUploadCalls {
		gotDigestUploads[i] = req.Digest.Hash
	}
	sort.Strings(gotDigestUploads)
	if diff := cmp.Diff(wantDigestUploads, gotDigestUploads); diff != "" {
		t.Error(diff)
	}
}

func compareUploadItems(x, y *uploadItem) bool {
	return x.Title == y.Title &&
		proto.Equal(x.Digest, y.Digest) &&
		((x.Open == nil && y.Open == nil) || cmp.Equal(mustReadAll(x), mustReadAll(y)))
}

func mustReadAll(item *uploadItem) []byte {
	r, err := item.Open()
	if err != nil {
		panic(err)
	}
	data, err := ioutil.ReadAll(r)
	if err != nil {
		panic(err)
	}
	return data
}

func inputChanFrom(inputs ...*UploadInput) chan *UploadInput {
	inputC := make(chan *UploadInput, len(inputs))
	for _, in := range inputs {
		inputC <- in
	}
	close(inputC)
	return inputC
}

type fakeCAS struct {
	repb.ContentAddressableStorageClient
	findMissingBlobs func(ctx context.Context, in *repb.FindMissingBlobsRequest, opts ...grpc.CallOption) (*repb.FindMissingBlobsResponse, error)
}

func (c *fakeCAS) FindMissingBlobs(ctx context.Context, in *repb.FindMissingBlobsRequest, opts ...grpc.CallOption) (*repb.FindMissingBlobsResponse, error) {
	return c.findMissingBlobs(ctx, in, opts...)
}

func putFile(t *testing.T, path, contents string) {
	if err := os.MkdirAll(filepath.Dir(path), 0777); err != nil {
		t.Fatal(err)
	}
	if err := ioutil.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}