// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package colexec

import (
	"context"
	"fmt"
	"testing"
	"unsafe"

	"github.com/cockroachdb/cockroach/pkg/col/coldata"
	"github.com/cockroachdb/cockroach/pkg/col/coldatatestutils"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/colexec/colexectestutils"
	"github.com/cockroachdb/cockroach/pkg/sql/colexecerror"
	"github.com/cockroachdb/cockroach/pkg/sql/colexecop"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/errors"
	"github.com/stretchr/testify/require"
)

func TestColumnarizeMaterialize(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	rng, _ := randutil.NewPseudoRand()
	nCols := 1 + rng.Intn(4)
	var typs []*types.T
	for len(typs) < nCols {
		typs = append(typs, rowenc.RandType(rng))
	}
	nRows := 10000
	rows := rowenc.RandEncDatumRowsOfTypes(rng, nRows, typs)
	input := execinfra.NewRepeatableRowSource(typs, rows)

	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)
	flowCtx := &execinfra.FlowCtx{
		Cfg:     &execinfra.ServerConfig{Settings: st},
		EvalCtx: &evalCtx,
	}
	c, err := NewBufferingColumnarizer(ctx, testAllocator, flowCtx, 0, input)
	if err != nil {
		t.Fatal(err)
	}

	m, err := NewMaterializer(
		flowCtx,
		1, /* processorID */
		c,
		typs,
		nil, /* output */
		nil, /* getStats */
		nil, /* metadataSources */
		nil, /* toClose */
		nil, /* cancelFlow */
	)
	if err != nil {
		t.Fatal(err)
	}
	m.Start(ctx)

	for i := 0; i < nRows; i++ {
		row, meta := m.Next()
		if meta != nil {
			t.Fatalf("unexpected meta %+v", meta)
		}
		if row == nil {
			t.Fatal("unexpected nil row")
		}
		for j := range typs {
			if row[j].Datum.Compare(&evalCtx, rows[i][j].Datum) != 0 {
				t.Fatal("unequal rows", row, rows[i])
			}
		}
	}
	row, meta := m.Next()
	if meta != nil {
		t.Fatalf("unexpected meta %+v", meta)
	}
	if row != nil {
		t.Fatal("unexpected not nil row", row)
	}
}

func BenchmarkMaterializer(b *testing.B) {
	defer log.Scope(b).Close(b)
	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)
	flowCtx := &execinfra.FlowCtx{
		Cfg:     &execinfra.ServerConfig{Settings: st},
		EvalCtx: &evalCtx,
	}

	rng, _ := randutil.NewPseudoRand()
	nBatches := 10
	nRows := nBatches * coldata.BatchSize()
	for _, typ := range []*types.T{types.Int, types.Float, types.Bytes} {
		typs := []*types.T{typ}
		nCols := len(typs)
		for _, hasNulls := range []bool{false, true} {
			for _, useSelectionVector := range []bool{false, true} {
				b.Run(fmt.Sprintf("%s/hasNulls=%t/useSel=%t", typ, hasNulls, useSelectionVector), func(b *testing.B) {
					nullProb := 0.0
					if hasNulls {
						nullProb = nullProbability
					}
					batch := testAllocator.NewMemBatchWithMaxCapacity(typs)
					for _, colVec := range batch.ColVecs() {
						coldatatestutils.RandomVec(coldatatestutils.RandomVecArgs{
							Rand:             rng,
							Vec:              colVec,
							N:                coldata.BatchSize(),
							NullProbability:  nullProb,
							BytesFixedLength: 8,
						})
					}
					batch.SetLength(coldata.BatchSize())
					if useSelectionVector {
						batch.SetSelection(true)
						sel := batch.Selection()
						for i := 0; i < coldata.BatchSize(); i++ {
							sel[i] = i
						}
					}
					input := colexectestutils.NewFiniteBatchSource(testAllocator, batch, typs, nBatches)

					b.SetBytes(int64(nRows * nCols * int(unsafe.Sizeof(int64(0)))))
					for i := 0; i < b.N; i++ {
						m, err := NewMaterializer(
							flowCtx,
							0, /* processorID */
							input,
							typs,
							nil, /* output */
							nil, /* getStats */
							nil, /* metadataSources */
							nil, /* toClose */
							nil, /* cancelFlow */
						)
						if err != nil {
							b.Fatal(err)
						}
						m.Start(ctx)

						foundRows := 0
						for {
							row, meta := m.Next()
							if meta != nil {
								b.Fatalf("unexpected metadata %v", meta)
							}
							if row == nil {
								break
							}
							foundRows++
						}
						if foundRows != nRows {
							b.Fatalf("expected %d rows, found %d", nRows, foundRows)
						}
						input.Reset(nBatches)
					}
				})
			}
		}
	}
}

func TestMaterializerNextErrorAfterConsumerDone(t *testing.T) {
	defer leaktest.AfterTest(t)()

	testError := errors.New("test-induced error")
	metadataSource := &execinfrapb.CallbackMetadataSource{DrainMetaCb: func(_ context.Context) []execinfrapb.ProducerMetadata {
		colexecerror.InternalError(testError)
		// Unreachable
		return nil
	}}
	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)
	flowCtx := &execinfra.FlowCtx{
		EvalCtx: &evalCtx,
	}

	m, err := NewMaterializer(
		flowCtx,
		0, /* processorID */
		&colexecop.CallbackOperator{},
		nil, /* typ */
		nil, /* output */
		nil, /* getStats */
		[]execinfrapb.MetadataSource{metadataSource},
		nil, /* toClose */
		nil, /* cancelFlow */
	)
	require.NoError(t, err)

	m.Start(ctx)
	// Call ConsumerDone.
	m.ConsumerDone()
	// We expect Next to panic since DrainMeta panics are currently not caught by
	// the materializer and it's not clear whether they should be since
	// implementers of DrainMeta do not return errors as panics.
	testutils.IsError(
		colexecerror.CatchVectorizedRuntimeError(func() {
			m.Next()
		}),
		testError.Error(),
	)
}

func BenchmarkColumnarizeMaterialize(b *testing.B) {
	defer log.Scope(b).Close(b)
	types := []*types.T{types.Int, types.Int}
	nRows := 10000
	nCols := 2
	rows := rowenc.MakeIntRows(nRows, nCols)
	input := execinfra.NewRepeatableRowSource(types, rows)

	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := tree.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)
	flowCtx := &execinfra.FlowCtx{
		Cfg:     &execinfra.ServerConfig{Settings: st},
		EvalCtx: &evalCtx,
	}
	c, err := NewBufferingColumnarizer(ctx, testAllocator, flowCtx, 0, input)
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(nRows * nCols * int(unsafe.Sizeof(int64(0)))))
	for i := 0; i < b.N; i++ {
		m, err := NewMaterializer(
			flowCtx,
			1, /* processorID */
			c,
			types,
			nil, /* output */
			nil, /* getStats */
			nil, /* metadataSources */
			nil, /* toClose */
			nil, /* cancelFlow */
		)
		if err != nil {
			b.Fatal(err)
		}
		m.Start(ctx)

		foundRows := 0
		for {
			row, meta := m.Next()
			if meta != nil {
				b.Fatalf("unexpected metadata %v", meta)
			}
			if row == nil {
				break
			}
			foundRows++
		}
		if foundRows != nRows {
			b.Fatalf("expected %d rows, found %d", nRows, foundRows)
		}
		input.Reset()
	}
}
