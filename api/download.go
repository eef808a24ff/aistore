// Package api provides RESTful API to AIS object storage
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package api

import (
	"net/http"
	"sort"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/downloader"
)

func DownloadSingle(baseParams BaseParams, description string, bck cmn.Bck, objName, link string) (string, error) {
	dlBody := downloader.DlSingleBody{
		DlSingleObj: downloader.DlSingleObj{
			ObjName: objName,
			Link:    link,
		},
	}
	dlBody.Bck = bck
	dlBody.Description = description
	return DownloadWithParam(baseParams, downloader.DlTypeSingle, &dlBody)
}

func DownloadRange(baseParams BaseParams, description string, bck cmn.Bck, template string) (string, error) {
	dlBody := downloader.DlRangeBody{
		Template: template,
	}
	dlBody.Bck = bck
	dlBody.Description = description
	return DownloadWithParam(baseParams, downloader.DlTypeRange, dlBody)
}

func DownloadWithParam(baseParams BaseParams, dlt downloader.DlType, body interface{}) (string, error) {
	baseParams.Method = http.MethodPost
	return doDlDownloadRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.JoinWords(cmn.Version, cmn.Download),
		Body:       cmn.MustMarshal(downloader.DlBody{Type: dlt, RawMessage: cmn.MustMarshal(body)}),
	})
}

func DownloadMulti(baseParams BaseParams, description string, bck cmn.Bck, msg interface{}) (string, error) {
	dlBody := downloader.DlMultiBody{}
	dlBody.Bck = bck
	dlBody.Description = description
	dlBody.ObjectsPayload = msg
	return DownloadWithParam(baseParams, downloader.DlTypeMulti, dlBody)
}

func DownloadCloud(baseParams BaseParams, description string, bck cmn.Bck, prefix, suffix string) (string, error) {
	dlBody := downloader.DlCloudBody{
		Prefix: prefix,
		Suffix: suffix,
	}
	dlBody.Bck = bck
	dlBody.Description = description
	return DownloadWithParam(baseParams, downloader.DlTypeCloud, dlBody)
}

func DownloadStatus(baseParams BaseParams, id string) (downloader.DlStatusResp, error) {
	dlBody := downloader.DlAdminBody{
		ID: id,
	}
	baseParams.Method = http.MethodGet
	return doDlStatusRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.JoinWords(cmn.Version, cmn.Download),
		Body:       cmn.MustMarshal(dlBody),
	})
}

func DownloadGetList(baseParams BaseParams, regex string) (dlList downloader.DlJobInfos, err error) {
	dlBody := downloader.DlAdminBody{
		Regex: regex,
	}
	baseParams.Method = http.MethodGet
	err = DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.JoinWords(cmn.Version, cmn.Download),
		Body:       cmn.MustMarshal(dlBody),
	}, &dlList)
	sort.Sort(dlList)
	return dlList, err
}

func DownloadAbort(baseParams BaseParams, id string) error {
	dlBody := downloader.DlAdminBody{
		ID: id,
	}
	baseParams.Method = http.MethodDelete
	return DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.JoinWords(cmn.Version, cmn.Download, cmn.Abort),
		Body:       cmn.MustMarshal(dlBody),
	})
}

func DownloadRemove(baseParams BaseParams, id string) error {
	dlBody := downloader.DlAdminBody{
		ID: id,
	}
	baseParams.Method = http.MethodDelete
	return DoHTTPRequest(ReqParams{
		BaseParams: baseParams,
		Path:       cmn.JoinWords(cmn.Version, cmn.Download, cmn.Remove),
		Body:       cmn.MustMarshal(dlBody),
	})
}

func doDlDownloadRequest(reqParams ReqParams) (string, error) {
	var resp downloader.DlPostResp
	err := DoHTTPRequest(reqParams, &resp)
	return resp.ID, err
}

func doDlStatusRequest(reqParams ReqParams) (resp downloader.DlStatusResp, err error) {
	err = DoHTTPRequest(reqParams, &resp)
	return resp, err
}