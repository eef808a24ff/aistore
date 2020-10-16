// Package downloader implements functionality to download resources into AIS cluster from external source.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package downloader

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	jsoniter "github.com/json-iterator/go"
)

var errInvalidTarget = errors.New("invalid target")

// buildDlObjs returns list of objects that must be downloaded by target.
func buildDlObjs(t cluster.Target, bck *cluster.Bck, objects cmn.SimpleKVs) ([]dlObj, error) {
	var (
		smap = t.Sowner().Get()
		sid  = t.Snode().ID()
	)

	objs := make([]dlObj, 0, len(objects))
	for name, link := range objects {
		obj, err := makeDlObj(smap, sid, bck, name, link)
		if err != nil {
			if err == errInvalidTarget {
				continue
			}
			return nil, err
		}
		objs = append(objs, obj)
	}
	return objs, nil
}

func makeDlObj(smap *cluster.Smap, sid string, bck *cluster.Bck, objName, link string) (dlObj, error) {
	objName, err := normalizeObjName(objName)
	if err != nil {
		return dlObj{}, err
	}

	si, err := cluster.HrwTarget(bck.MakeUname(objName), smap)
	if err != nil {
		return dlObj{}, err
	}
	if si.ID() != sid {
		return dlObj{}, errInvalidTarget
	}

	return dlObj{
		objName: objName,
		// Make sure that link contains protocol (absence of protocol can result in errors).
		link:      cmn.PrependProtocol(link),
		fromCloud: link == "",
	}, nil
}

// Removes everything that goes after '?', eg. "?query=key..." so it will not
// be part of final object name.
func normalizeObjName(objName string) (string, error) {
	u, err := url.Parse(objName)
	if err != nil {
		return "", nil
	}

	if u.Path == "" {
		return objName, nil
	}

	return url.PathUnescape(u.Path)
}

func ParseStartDownloadRequest(ctx context.Context, t cluster.Target, bck *cluster.Bck, id string, dlb DlBody, dlXact *Downloader) (DlJob, error) {
	switch dlb.Type {
	case DlTypeCloud:
		dp := &DlCloudBody{}
		err := jsoniter.Unmarshal(dlb.RawMessage, dp)
		if err != nil {
			return nil, err
		}
		if err := dp.Validate(); err != nil {
			return nil, err
		}
		return newCloudBucketDlJob(ctx, t, id, bck, dp, dlXact)

	case DlTypeMulti:
		dp := &DlMultiBody{}
		err := jsoniter.Unmarshal(dlb.RawMessage, dp)
		if err != nil {
			return nil, err
		}
		if err := dp.Validate(); err != nil {
			return nil, err
		}
		return newMultiDlJob(t, id, bck, dp, dlXact)

	case DlTypeRange:
		dp := &DlRangeBody{}
		err := jsoniter.Unmarshal(dlb.RawMessage, dp)
		if err != nil {
			return nil, err
		}
		if err := dp.Validate(); err != nil {
			return nil, err
		}
		return newRangeDlJob(t, id, bck, dp, dlXact)

	case DlTypeSingle:
		dp := &DlSingleBody{}
		err := jsoniter.Unmarshal(dlb.RawMessage, dp)
		if err != nil {
			return nil, err
		}
		if err := dp.Validate(); err != nil {
			return nil, err
		}
		return newSingleDlJob(t, id, bck, dp, dlXact)

	default:
		return nil, errors.New("input does not match any of the supported formats (single, range, multi, cloud)")
	}
}

//
// Checksum and version validation helpers
//

const (
	// https://cloud.google.com/storage/docs/xml-api/reference-headers
	gsCksumHeader   = "x-goog-hash"
	gsVersionHeader = "x-goog-generation"

	// https://docs.aws.amazon.com/AmazonS3/latest/dev/UsingMetadata.html
	// https://docs.aws.amazon.com/AmazonS3/latest/API/RESTCommonResponseHeaders.html
	s3CksumHeader   = "ETag"
	s3VersionHeader = "x-amz-version-id"

	// https://docs.microsoft.com/en-us/rest/api/storageservices/get-blob-properties#response-headers
	azCksumHeader   = "Content-MD5"
	azVersionHeader = "ETag"
)

type (
	remoteObjInfo struct {
		size int64
		md   cmn.SimpleKVs
	}
)

// Get file info if link points to GCP, S3 or Azure.
func roiFromLink(link string, resp *http.Response) (roi remoteObjInfo) {
	u, err := url.Parse(link)
	cmn.AssertNoErr(err)

	if cmn.IsGoogleStorageURL(u) || cmn.IsGoogleAPIURL(u) {
		h := cmn.CloudHelpers.Google
		roi.md = make(cmn.SimpleKVs, 3)
		roi.md[cluster.SourceObjMD] = cluster.SourceGoogleObjMD
		if v, ok := h.EncodeVersion(resp.Header.Get(gsVersionHeader)); ok {
			roi.md[cluster.VersionObjMD] = v
		}
		if hdr := resp.Header[http.CanonicalHeaderKey(gsCksumHeader)]; len(hdr) > 0 {
			for cksumType, cksumValue := range parseGoogleCksumHeader(hdr) {
				switch cksumType {
				case cmn.ChecksumMD5:
					roi.md[cluster.MD5ObjMD] = cksumValue
				case cmn.ChecksumCRC32C:
					roi.md[cluster.CRC32CObjMD] = cksumValue
				default:
					glog.Errorf("unimplemented cksum type for custom metadata: %s", cksumType)
				}
			}
		}
	} else if cmn.IsS3URL(link) {
		h := cmn.CloudHelpers.Amazon
		roi.md = make(cmn.SimpleKVs, 3)
		roi.md[cluster.SourceObjMD] = cluster.SourceAmazonObjMD
		if v, ok := h.EncodeVersion(resp.Header.Get(s3VersionHeader)); ok {
			roi.md[cluster.VersionObjMD] = v
		}
		if v, ok := h.EncodeCksum(resp.Header.Get(s3CksumHeader)); ok {
			roi.md[cluster.MD5ObjMD] = v
		}
	} else if cmn.IsAzureURL(u) {
		h := cmn.CloudHelpers.Azure
		roi.md = make(cmn.SimpleKVs, 1)
		roi.md[cluster.SourceObjMD] = cluster.SourceAzureObjMD
		if v, ok := h.EncodeVersion(resp.Header.Get(azVersionHeader)); ok {
			roi.md[cluster.VersionObjMD] = v
		}
		if v, ok := h.EncodeCksum(resp.Header.Get(azCksumHeader)); ok {
			roi.md[cluster.MD5ObjMD] = v
		}
	} else {
		roi.md = make(cmn.SimpleKVs, 1)
		roi.md[cluster.SourceObjMD] = cluster.SourceWebObjMD
	}
	roi.size = resp.ContentLength
	return
}

func parseGoogleCksumHeader(hdr []string) cmn.SimpleKVs {
	var (
		h      = cmn.CloudHelpers.Google
		cksums = make(cmn.SimpleKVs, 2)
	)
	for _, v := range hdr {
		entry := strings.SplitN(v, "=", 2)
		cmn.Assert(len(entry) == 2)
		cmn.AssertNoErr(cmn.ValidateCksumType(entry[0]))
		if v, ok := h.EncodeCksum(entry[1]); ok {
			cksums[entry[0]] = v
		}
	}
	return cksums
}

func headLink(link string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(context.Background(), headReqTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, link, nil)
	if err != nil {
		return nil, err
	}
	resp, err := clientForURL(link).Do(req)
	if err != nil {
		return nil, err
	}
	cmn.Close(resp.Body)
	return resp, nil
}

func roiFromObjMeta(objMeta cmn.SimpleKVs) (roi remoteObjInfo) {
	roi.md = make(cmn.SimpleKVs, 2)
	switch objMeta[cmn.HeaderCloudProvider] {
	case cmn.ProviderGoogle:
		roi.md[cluster.SourceObjMD] = cluster.SourceGoogleObjMD
	case cmn.ProviderAmazon:
		roi.md[cluster.SourceObjMD] = cluster.SourceAmazonObjMD
	case cmn.ProviderAzure:
		roi.md[cluster.SourceObjMD] = cluster.SourceAzureObjMD
	default:
		return
	}
	if v, ok := objMeta[cmn.HeaderObjVersion]; ok {
		roi.md[cluster.VersionObjMD] = v
	}
	if v, ok := objMeta[cluster.MD5ObjMD]; ok {
		roi.md[cluster.MD5ObjMD] = v
	}
	if v, ok := objMeta[cluster.CRC32CObjMD]; ok {
		roi.md[cluster.CRC32CObjMD] = v
	}
	if v := objMeta[cmn.HeaderObjSize]; v != "" {
		roi.size, _ = strconv.ParseInt(v, 10, 64)
	}
	return
}

func compareObjects(src *cluster.LOM, dst *DstElement) (equal bool, err error) {
	var roi remoteObjInfo
	if dst.Link != "" {
		resp, err := headLink(dst.Link)
		if err != nil {
			return false, err
		}
		roi = roiFromLink(dst.Link, resp)
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), headReqTimeout)
		defer cancel()
		// This should succeed since we check if the bucket exists beforehand.
		objMeta, err, _ := src.T.Cloud(src.Bck()).HeadObj(ctx, src)
		if err != nil {
			return false, err
		}
		roi = roiFromObjMeta(objMeta)
	}

	if roi.size != 0 && roi.size != src.Size() {
		return false, nil
	}

	_, localMDPresent := src.GetCustomMD(cluster.SourceObjMD)
	remoteSource := roi.md[cluster.SourceObjMD]
	if !localMDPresent {
		// Source is present only on the remote object. But if it's the cloud
		// object it will have version set to cloud version. Therefore, we can
		// try to compare it.
		switch remoteSource {
		case cluster.SourceGoogleObjMD, cluster.SourceAmazonObjMD, cluster.SourceAzureObjMD:
			if src.Version() == roi.md[cluster.VersionObjMD] {
				return true, nil
			}
		case cluster.SourceWebObjMD:
			// In case it is web we just assume the objects are equal since the
			// name and size matches.
			return true, nil
		}
		return false, nil
	}
	for k, v := range roi.md {
		if objValue, ok := src.GetCustomMD(k); !ok {
			// Just skip check if `lom` doesn't have some metadata.
			continue
		} else if v != objValue {
			// Metadata does not match - objects are different.
			return false, nil
		}
	}
	// Cannot prove that the objects are different so assume they are equal.
	return true, nil
}
