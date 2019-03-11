package recipes

import (
	"time"

	"github.com/NVIDIA/aistore/bench/soaktest/soakprim"
)

func recBasicPut(rctx *soakprim.RecipeContext) {
	//most basic recipe that puts some stuff in buckets,
	// the 'hello world' of recipes

	conds := &soakprim.PreConds{
		ExpBuckets: []string{},
	}

	rctx.Pre(conds)
	rctx.MakeBucket("b1")
	rctx.MakeBucket("b2")
	rctx.Post(nil)

	conds.ExpBuckets = []string{"b2", "b1"}
	rctx.Pre(conds)
	rctx.Put("b1", time.Second*8, 30)
	rctx.Put("b2", time.Second*10, 20)
	rctx.Post(nil)

	conds.ExpBuckets = []string{"b2", "b1"}
	rctx.Pre(conds)
	rctx.Destroy("b2")
	rctx.Get("b1", time.Second*10, true, 4, 0)
	rctx.Post(nil)

	conds.ExpBuckets = []string{"b1"}
	rctx.Pre(conds)
	rctx.Destroy("b1")
	conds.ExpBuckets = []string{}
	rctx.Post(nil)
}