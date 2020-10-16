// Package api provides RESTful API to AIS object storage
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package api

import (
	"io/ioutil"
	"net/http"
	"net/url"

	"github.com/NVIDIA/aistore/cmn"
)

// s3/<bucket-name>/<object-name>
func GetObjectS3(baseParams BaseParams, bck cmn.Bck, objectName string, options ...GetObjectInput) (n int64, err error) {
	var (
		w   = ioutil.Discard
		q   url.Values
		hdr http.Header
	)
	if len(options) != 0 {
		w, q, hdr = getObjectOptParams(options[0])
	}
	q = cmn.AddBckToQuery(q, bck)
	baseParams.Method = http.MethodGet
	resp, err := doHTTPRequestGetResp(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.JoinWords(cmn.S3, bck.Name, objectName),
		Query:      q,
		Header:     hdr,
	}, w)
	if err != nil {
		return 0, err
	}
	return resp.n, nil
}
