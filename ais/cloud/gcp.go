// +build gcp

// Package cloud contains implementation of various cloud providers.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package cloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"

	"cloud.google.com/go/storage"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	jsoniter "github.com/json-iterator/go"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	htransport "google.golang.org/api/transport/http"
)

const (
	gcpChecksumType = "x-goog-meta-ais-cksum-type"
	gcpChecksumVal  = "x-goog-meta-ais-cksum-val"

	projectIDField  = "project_id"
	projectIDEnvVar = "GOOGLE_CLOUD_PROJECT"
	credPathEnvVar  = "GOOGLE_APPLICATION_CREDENTIALS"
)

type (
	gcpProvider struct {
		t         cluster.Target
		projectID string
	}
)

var _ cluster.CloudProvider = &gcpProvider{}

func readCredFile() (projectID string) {
	credFile, err := os.Open(os.Getenv(credPathEnvVar))
	if err != nil {
		return
	}
	b, err := ioutil.ReadAll(credFile)
	credFile.Close()
	if err != nil {
		return
	}
	projectID, _ = jsoniter.Get(b, projectIDField).GetInterface().(string)
	return
}

func NewGCP(t cluster.Target) (cluster.CloudProvider, error) {
	var (
		projectID     string
		credProjectID = readCredFile()
		envProjectID  = os.Getenv(projectIDEnvVar)
	)
	if credProjectID != "" && envProjectID != "" && credProjectID != envProjectID {
		return nil, fmt.Errorf(
			"both %q and %q env vars are non-empty (and %s is not equal) cannot decide which to use",
			projectIDEnvVar, credPathEnvVar, projectIDField,
		)
	} else if credProjectID != "" {
		projectID = credProjectID
		glog.Infof("[cloud_gcp] %s: %q (using %q env variable)", projectIDField, projectID, credPathEnvVar)
	} else if envProjectID != "" {
		projectID = envProjectID
		glog.Infof("[cloud_gcp] %s: %q (using %q env variable)", projectIDField, projectID, projectIDEnvVar)
	} else {
		glog.Warningf("[cloud_gcp] unable to determine %q (%q and %q env vars are empty) - using unauthenticated client", projectIDField, projectIDEnvVar, credPathEnvVar)
	}
	return &gcpProvider{t: t, projectID: projectID}, nil
}

func (gcpp *gcpProvider) createClient(ctx context.Context) (*storage.Client, context.Context, error) {
	opts := []option.ClientOption{option.WithScopes(storage.ScopeFullControl)}
	if gcpp.projectID == "" {
		opts = append(opts, option.WithoutAuthentication())
	}

	// Create a custom HTTP client
	transport, err := htransport.NewTransport(ctx, cmn.NewTransport(cmn.TransportArgs{}), opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create http client transport, err: %v", err)
	}
	opts = append(opts, option.WithHTTPClient(&http.Client{Transport: transport}))

	client, err := storage.NewClient(ctx, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create client, err: %v", err)
	}
	return client, ctx, nil
}

func (gcpp *gcpProvider) gcpErrorToAISError(gcpError error, bck *cmn.Bck) (error, int) {
	if gcpError == storage.ErrBucketNotExist {
		return cmn.NewErrorRemoteBucketDoesNotExist(*bck, gcpp.t.Snode().Name()), http.StatusNotFound
	}
	status := http.StatusBadRequest
	if apiErr, ok := gcpError.(*googleapi.Error); ok {
		status = apiErr.Code
	} else if gcpError == storage.ErrObjectNotExist {
		status = http.StatusNotFound
	}
	return gcpError, status
}

func (gcpp *gcpProvider) handleObjectError(ctx context.Context, gcpClient *storage.Client, objErr error, bck *cmn.Bck) (error, int) {
	if objErr != storage.ErrObjectNotExist {
		return objErr, http.StatusBadRequest
	}

	// Object does not exist, but in GCP it doesn't mean that the bucket existed.
	// Check if the buckets exists.
	if _, err := gcpClient.Bucket(bck.Name).Attrs(ctx); err != nil {
		return gcpp.gcpErrorToAISError(err, bck)
	}
	return cmn.NewNotFoundError(objErr.Error()), http.StatusNotFound
}

func (gcpp *gcpProvider) Provider() string { return cmn.ProviderGoogle }

// https://cloud.google.com/storage/docs/json_api/v1/objects/list#parameters
func (gcpp *gcpProvider) MaxPageSize() uint { return 1000 }

//////////////////
// LIST OBJECTS //
//////////////////

func (gcpp *gcpProvider) ListObjects(ctx context.Context, bck *cluster.Bck, msg *cmn.SelectMsg) (bckList *cmn.BucketList, err error, errCode int) {
	gcpClient, gctx, err := gcpp.createClient(ctx)
	if err != nil {
		return
	}
	msg.PageSize = calcPageSize(msg.PageSize, gcpp.MaxPageSize())
	var (
		query    *storage.Query
		h        = cmn.CloudHelpers.Google
		cloudBck = bck.RemoteBck()
	)
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("list_objects %s", cloudBck.Name)
	}

	if msg.Prefix != "" {
		query = &storage.Query{Prefix: msg.Prefix}
	}

	var (
		it    = gcpClient.Bucket(cloudBck.Name).Objects(gctx, query)
		pager = iterator.NewPager(it, int(msg.PageSize), msg.ContinuationToken)
		objs  = make([]*storage.ObjectAttrs, 0, msg.PageSize)
	)
	nextPageToken, err := pager.NextPage(&objs)
	if err != nil {
		err, errCode = gcpp.gcpErrorToAISError(err, cloudBck)
		return
	}

	bckList = &cmn.BucketList{Entries: make([]*cmn.BucketEntry, 0, len(objs))}
	bckList.ContinuationToken = nextPageToken
	for _, attrs := range objs {
		entry := &cmn.BucketEntry{}
		entry.Name = attrs.Name
		if msg.WantProp(cmn.GetPropsSize) {
			entry.Size = attrs.Size
		}
		if msg.WantProp(cmn.GetPropsChecksum) {
			if v, ok := h.EncodeCksum(attrs.MD5); ok {
				entry.Checksum = v
			}
		}
		if msg.WantProp(cmn.GetPropsVersion) {
			if v, ok := h.EncodeVersion(attrs.Generation); ok {
				entry.Version = v
			}
		}
		bckList.Entries = append(bckList.Entries, entry)
	}

	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("[list_bucket] count %d", len(bckList.Entries))
	}

	return
}

func (gcpp *gcpProvider) HeadBucket(ctx context.Context, bck *cluster.Bck) (bckProps cmn.SimpleKVs, err error, errCode int) {
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("head_bucket %s", bck.Name)
	}

	gcpClient, gctx, err := gcpp.createClient(ctx)
	if err != nil {
		return
	}
	cloudBck := bck.RemoteBck()
	_, err = gcpClient.Bucket(cloudBck.Name).Attrs(gctx)
	if err != nil {
		err, errCode = gcpp.gcpErrorToAISError(err, cloudBck)
		return
	}
	bckProps = make(cmn.SimpleKVs)
	bckProps[cmn.HeaderCloudProvider] = cmn.ProviderGoogle
	// GCP always generates a versionid for an object even if versioning is disabled.
	// So, return that we can detect versionid change on getobj etc
	bckProps[cmn.HeaderBucketVerEnabled] = "true"
	return
}

//////////////////
// BUCKET NAMES //
//////////////////

func (gcpp *gcpProvider) ListBuckets(ctx context.Context, _ cmn.QueryBcks) (buckets cmn.BucketNames, err error, errCode int) {
	if gcpp.projectID == "" {
		// NOTE: Passing empty `projectID` to `Buckets` method results in
		//  enigmatic error: "googleapi: Error 400: Invalid argument".
		return nil, errors.New("listing buckets with the unauthenticated client is not possible"), http.StatusBadRequest
	}
	gcpClient, gctx, err := gcpp.createClient(ctx)
	if err != nil {
		return
	}
	buckets = make(cmn.BucketNames, 0, 16)
	it := gcpClient.Buckets(gctx, gcpp.projectID)
	for {
		var battrs *storage.BucketAttrs

		battrs, err = it.Next()
		if err == iterator.Done {
			err = nil
			break
		}
		if err != nil {
			err, errCode = gcpp.gcpErrorToAISError(err, &cmn.Bck{Provider: cmn.ProviderGoogle})
			return
		}
		buckets = append(buckets, cmn.Bck{
			Name:     battrs.Name,
			Provider: cmn.ProviderGoogle,
		})
		if glog.FastV(4, glog.SmoduleAIS) {
			glog.Infof("[bucket_names] %s: created %v, versioning %t", battrs.Name, battrs.Created, battrs.VersioningEnabled)
		}
	}
	return
}

/////////////////
// HEAD OBJECT //
/////////////////

func (gcpp *gcpProvider) HeadObj(ctx context.Context, lom *cluster.LOM) (objMeta cmn.SimpleKVs, err error, errCode int) {
	gcpClient, gctx, err := gcpp.createClient(ctx)
	if err != nil {
		return
	}
	var (
		h        = cmn.CloudHelpers.Google
		cloudBck = lom.Bck().RemoteBck()
	)
	attrs, err := gcpClient.Bucket(cloudBck.Name).Object(lom.ObjName).Attrs(gctx)
	if err != nil {
		err, errCode = gcpp.handleObjectError(gctx, gcpClient, err, cloudBck)
		return
	}
	objMeta = make(cmn.SimpleKVs)
	objMeta[cmn.HeaderCloudProvider] = cmn.ProviderGoogle
	objMeta[cmn.HeaderObjSize] = strconv.FormatInt(attrs.Size, 10)
	if v, ok := h.EncodeVersion(attrs.Generation); ok {
		objMeta[cmn.HeaderObjVersion] = v
	}
	if v, ok := h.EncodeCksum(attrs.MD5); ok {
		objMeta[cluster.MD5ObjMD] = v
	}
	if v, ok := h.EncodeCksum(attrs.CRC32C); ok {
		objMeta[cluster.CRC32CObjMD] = v
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("[head_object] %s/%s", cloudBck, lom.ObjName)
	}
	return
}

////////////////
// GET OBJECT //
////////////////

func (gcpp *gcpProvider) GetObjReader(ctx context.Context, lom *cluster.LOM) (reader io.ReadCloser,
	expectedCksm *cmn.Cksum, err error, errCode int) {
	gcpClient, gctx, err := gcpp.createClient(ctx)
	if err != nil {
		err, errCode = gcpp.gcpErrorToAISError(err, &lom.Bck().Bck)
		return nil, nil, err, errCode
	}
	var (
		h        = cmn.CloudHelpers.Google
		cloudBck = lom.Bck().RemoteBck()
		o        = gcpClient.Bucket(cloudBck.Name).Object(lom.ObjName)
	)
	attrs, err := o.Attrs(gctx)
	if err != nil {
		err, errCode = gcpp.gcpErrorToAISError(err, cloudBck)
		return nil, nil, err, errCode
	}

	cksum := cmn.NewCksum(attrs.Metadata[gcpChecksumType], attrs.Metadata[gcpChecksumVal])
	rc, err := o.NewReader(gctx)
	if err != nil {
		return nil, nil, err, 0
	}

	customMD := cmn.SimpleKVs{
		cluster.SourceObjMD: cluster.SourceGoogleObjMD,
	}

	if v, ok := h.EncodeVersion(attrs.Generation); ok {
		lom.SetVersion(v)
		customMD[cluster.VersionObjMD] = v
	}
	if v, ok := h.EncodeCksum(attrs.MD5); ok {
		expectedCksm = cmn.NewCksum(cmn.ChecksumMD5, v)
		customMD[cluster.MD5ObjMD] = v
	}
	if v, ok := h.EncodeCksum(attrs.CRC32C); ok {
		customMD[cluster.CRC32CObjMD] = v
	}

	lom.SetCksum(cksum)
	lom.SetCustomMD(customMD)
	setSize(ctx, rc.Attrs.Size)
	reader = wrapReader(ctx, rc)
	return
}

func (gcpp *gcpProvider) GetObj(ctx context.Context, workFQN string, lom *cluster.LOM) (err error, errCode int) {
	reader, cksumToCheck, err, errCode := gcpp.GetObjReader(ctx, lom)
	if err != nil {
		return err, errCode
	}
	params := cluster.PutObjectParams{
		Reader:       reader,
		WorkFQN:      workFQN,
		RecvType:     cluster.ColdGet,
		Cksum:        cksumToCheck,
		WithFinalize: false,
	}
	err = gcpp.t.PutObject(lom, params)
	if err != nil {
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("[get_object] %s", lom)
	}
	return
}

////////////////
// PUT OBJECT //
////////////////

func (gcpp *gcpProvider) PutObj(ctx context.Context, r io.Reader, lom *cluster.LOM) (version string, err error, errCode int) {
	gcpClient, gctx, err := gcpp.createClient(ctx)
	if err != nil {
		return
	}

	var (
		h        = cmn.CloudHelpers.Google
		cloudBck = lom.Bck().RemoteBck()
		md       = make(cmn.SimpleKVs, 2)
		gcpObj   = gcpClient.Bucket(cloudBck.Name).Object(lom.ObjName)
		wc       = gcpObj.NewWriter(gctx)
	)

	md[gcpChecksumType], md[gcpChecksumVal] = lom.Cksum().Get()

	wc.Metadata = md
	buf, slab := gcpp.t.MMSA().Alloc()
	written, err := io.CopyBuffer(wc, r, buf)
	slab.Free(buf)
	if err != nil {
		return
	}
	if err = wc.Close(); err != nil {
		err, errCode = gcpp.gcpErrorToAISError(err, cloudBck)
		return
	}
	attr, err := gcpObj.Attrs(gctx)
	if err != nil {
		err, errCode = gcpp.handleObjectError(gctx, gcpClient, err, cloudBck)
		return
	}
	if v, ok := h.EncodeVersion(attr.Generation); ok {
		version = v
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("[put_object] %s, size %d, version %s", lom, written, version)
	}
	return
}

///////////////////
// DELETE OBJECT //
///////////////////

func (gcpp *gcpProvider) DeleteObj(ctx context.Context, lom *cluster.LOM) (err error, errCode int) {
	gcpClient, gctx, err := gcpp.createClient(ctx)
	if err != nil {
		return
	}
	var (
		cloudBck = lom.Bck().RemoteBck()
		o        = gcpClient.Bucket(cloudBck.Name).Object(lom.ObjName)
	)

	if err = o.Delete(gctx); err != nil {
		err, errCode = gcpp.handleObjectError(gctx, gcpClient, err, cloudBck)
		return
	}
	if glog.FastV(4, glog.SmoduleAIS) {
		glog.Infof("[delete_object] %s", lom)
	}
	return
}
