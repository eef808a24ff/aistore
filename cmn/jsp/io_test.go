// Package jsp (JSON persistence) provides utilities to store and load arbitrary
// JSON-encoded structures with optional checksumming and compression.
/*
 * Copyright (c) 2018-2020, NVIDIA CORPORATION. All rights reserved.
 */
package jsp_test

import (
	"bytes"
	"io/ioutil"
	"math/rand"
	"testing"

	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/cmn/jsp"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/tutils/tassert"
)

type testStruct struct {
	I  int    `json:"a,omitempty"`
	S  string `json:"zero"`
	B  []byte `json:"bytes,omitempty"`
	ST struct {
		I64 int64 `json:"int64"`
	}
}

func (ts *testStruct) equal(other testStruct) bool {
	return ts.I == other.I &&
		ts.S == other.S &&
		string(ts.B) == string(other.B) &&
		ts.ST.I64 == other.ST.I64
}

func makeRandStruct() (ts testStruct) {
	if rand.Intn(2) == 0 {
		ts.I = rand.Int()
	}
	ts.S = cmn.RandString(rand.Intn(100))
	if rand.Intn(2) == 0 {
		ts.B = []byte(cmn.RandString(rand.Intn(200)))
	}
	ts.ST.I64 = rand.Int63()
	return
}

func makeStaticStruct() (ts testStruct) {
	ts.I = rand.Int()
	ts.S = cmn.RandString(100)
	ts.B = []byte(cmn.RandString(200))
	ts.ST.I64 = rand.Int63()
	return
}

func TestDecodeAndEncode(t *testing.T) {
	tests := []struct {
		name string
		v    testStruct
		opts jsp.Options
	}{
		{name: "empty", v: testStruct{}, opts: jsp.Options{}},
		{name: "default", v: makeRandStruct(), opts: jsp.Options{}},
		{name: "compress", v: makeRandStruct(), opts: jsp.Options{Compression: true}},
		{name: "cksum", v: makeRandStruct(), opts: jsp.Options{Checksum: true}},
		{name: "sign", v: makeRandStruct(), opts: jsp.Options{Signature: true}},
		{name: "compress_cksum", v: makeRandStruct(), opts: jsp.Options{Compression: true, Checksum: true}},
		{name: "cksum_sign", v: makeRandStruct(), opts: jsp.Options{Checksum: true, Signature: true}},
		{name: "ccs", v: makeRandStruct(), opts: jsp.CCSign()},
		{
			name: "special_char",
			v:    testStruct{I: 10, S: "abc\ncd", B: []byte{'a', 'b', '\n', 'c', 'd'}},
			opts: jsp.Options{Checksum: true},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var (
				v testStruct
				b = memsys.DefaultPageMM().NewSGL(cmn.MiB)
			)
			defer b.Free()

			err := jsp.Encode(b, test.v, test.opts)
			tassert.CheckFatal(t, err)

			err = jsp.Decode(b, &v, test.opts, "test")
			tassert.CheckFatal(t, err)

			// reflect.DeepEqual may not work here due to using `[]byte` in the struct.
			// `Decode` may generate empty slice from original `nil` slice and while
			// both are kind of the same, DeepEqual says they differ. From output when
			// the test fails:
			//      v(B:[]uint8(nil))   !=   test.v(B:[]uint8{})
			tassert.Fatalf(
				t, v.equal(test.v),
				"structs are not equal, (got: %+v, expected: %+v)", v, test.v,
			)
		})
	}
}

func BenchmarkEncode(b *testing.B) {
	benches := []struct {
		name string
		v    testStruct
		opts jsp.Options
	}{
		{name: "empty", v: testStruct{}, opts: jsp.Options{}},
		{name: "default", v: makeStaticStruct(), opts: jsp.Options{}},
		{name: "sign", v: makeStaticStruct(), opts: jsp.Options{Signature: true}},
		{name: "cksum", v: makeStaticStruct(), opts: jsp.Options{Checksum: true}},
		{name: "compress", v: makeStaticStruct(), opts: jsp.Options{Compression: true}},
		{name: "ccs", v: makeStaticStruct(), opts: jsp.CCSign()},
	}
	for _, bench := range benches {
		b.Run(bench.name, func(b *testing.B) {
			body := memsys.DefaultPageMM().NewSGL(cmn.MiB)
			defer func() {
				b.StopTimer()
				body.Free()
			}()
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				err := jsp.Encode(body, bench.v, bench.opts)
				tassert.CheckFatal(b, err)
				body.Reset()
			}
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	benches := []struct {
		name string
		v    testStruct
		opts jsp.Options
	}{
		{name: "empty", v: testStruct{}, opts: jsp.Options{}},
		{name: "default", v: makeStaticStruct(), opts: jsp.Options{}},
		{name: "sign", v: makeStaticStruct(), opts: jsp.Options{Signature: true}},
		{name: "cksum", v: makeStaticStruct(), opts: jsp.Options{Checksum: true}},
		{name: "compress", v: makeStaticStruct(), opts: jsp.Options{Compression: true}},
		{name: "ccs", v: makeStaticStruct(), opts: jsp.CCSign()},
	}
	for _, bench := range benches {
		b.Run(bench.name, func(b *testing.B) {
			sgl := memsys.DefaultPageMM().NewSGL(cmn.MiB)
			err := jsp.Encode(sgl, bench.v, bench.opts)
			tassert.CheckFatal(b, err)
			network, err := sgl.ReadAll()
			sgl.Free()
			tassert.CheckFatal(b, err)

			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				var (
					v testStruct
					r = ioutil.NopCloser(bytes.NewReader(network))
				)
				err := jsp.Decode(r, &v, bench.opts, "benchmark")
				tassert.CheckFatal(b, err)
			}
		})
	}
}
