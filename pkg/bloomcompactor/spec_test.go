package bloomcompactor

import (
	"bytes"
	"context"
	"testing"

	"github.com/go-kit/log"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/require"

	v1 "github.com/grafana/loki/pkg/storage/bloom/v1"
	"github.com/grafana/loki/pkg/storage/stores/shipper/bloomshipper"
)

func blocksFromSchema(t *testing.T, n int, options v1.BlockOptions) (res []*v1.Block, data []v1.SeriesWithBloom) {
	return blocksFromSchemaWithRange(t, n, options, 0, 0xffff)
}

// splits 100 series across `n` non-overlapping blocks.
// uses options to build blocks with.
func blocksFromSchemaWithRange(t *testing.T, n int, options v1.BlockOptions, fromFP, throughFp model.Fingerprint) (res []*v1.Block, data []v1.SeriesWithBloom) {
	if 100%n != 0 {
		panic("100 series must be evenly divisible by n")
	}

	numSeries := 100
	numKeysPerSeries := 10000
	data, _ = v1.MkBasicSeriesWithBlooms(numSeries, numKeysPerSeries, fromFP, throughFp, 0, 10000)

	seriesPerBlock := numSeries / n

	for i := 0; i < n; i++ {
		// references for linking in memory reader+writer
		indexBuf := bytes.NewBuffer(nil)
		bloomsBuf := bytes.NewBuffer(nil)
		writer := v1.NewMemoryBlockWriter(indexBuf, bloomsBuf)
		reader := v1.NewByteReader(indexBuf, bloomsBuf)

		builder, err := v1.NewBlockBuilder(
			options,
			writer,
		)
		require.Nil(t, err)

		itr := v1.NewSliceIter[v1.SeriesWithBloom](data[i*seriesPerBlock : (i+1)*seriesPerBlock])
		_, err = builder.BuildFrom(itr)
		require.Nil(t, err)

		res = append(res, v1.NewBlock(reader))
	}

	return res, data
}

// doesn't actually load any chunks
type dummyChunkLoader struct{}

func (dummyChunkLoader) Load(_ context.Context, _ string, series *v1.Series) (*ChunkItersByFingerprint, error) {
	return &ChunkItersByFingerprint{
		fp:  series.Fingerprint,
		itr: v1.NewEmptyIter[v1.ChunkRefWithIter](),
	}, nil
}

func dummyBloomGen(opts v1.BlockOptions, store v1.Iterator[*v1.Series], blocks []*v1.Block) *SimpleBloomGenerator {
	bqs := make([]*bloomshipper.CloseableBlockQuerier, 0, len(blocks))
	for _, b := range blocks {
		bqs = append(bqs, &bloomshipper.CloseableBlockQuerier{
			BlockQuerier: v1.NewBlockQuerier(b),
		})
	}

	return NewSimpleBloomGenerator(
		"fake",
		opts,
		store,
		dummyChunkLoader{},
		bqs,
		func() (v1.BlockWriter, v1.BlockReader) {
			indexBuf := bytes.NewBuffer(nil)
			bloomsBuf := bytes.NewBuffer(nil)
			return v1.NewMemoryBlockWriter(indexBuf, bloomsBuf), v1.NewByteReader(indexBuf, bloomsBuf)
		},
		NewMetrics(nil, v1.NewMetrics(nil)),
		log.NewNopLogger(),
	)
}

func TestSimpleBloomGenerator(t *testing.T) {
	const maxBlockSize = 100 << 20 // 100MB
	for _, tc := range []struct {
		desc                                   string
		fromSchema, toSchema                   v1.BlockOptions
		sourceBlocks, numSkipped, outputBlocks int
	}{
		{
			desc:         "SkipsIncompatibleSchemas",
			fromSchema:   v1.NewBlockOptions(3, 0, maxBlockSize),
			toSchema:     v1.NewBlockOptions(4, 0, maxBlockSize),
			sourceBlocks: 2,
			numSkipped:   2,
			outputBlocks: 1,
		},
		{
			desc:         "CombinesBlocks",
			fromSchema:   v1.NewBlockOptions(4, 0, maxBlockSize),
			toSchema:     v1.NewBlockOptions(4, 0, maxBlockSize),
			sourceBlocks: 2,
			numSkipped:   0,
			outputBlocks: 1,
		},
		{
			desc:         "MaxBlockSize",
			fromSchema:   v1.NewBlockOptions(4, 0, maxBlockSize),
			toSchema:     v1.NewBlockOptions(4, 0, 1<<10), // 1KB
			sourceBlocks: 2,
			numSkipped:   0,
			outputBlocks: 3,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			sourceBlocks, data := blocksFromSchema(t, tc.sourceBlocks, tc.fromSchema)
			storeItr := v1.NewMapIter[v1.SeriesWithBloom, *v1.Series](
				v1.NewSliceIter[v1.SeriesWithBloom](data),
				func(swb v1.SeriesWithBloom) *v1.Series {
					return swb.Series
				},
			)

			gen := dummyBloomGen(tc.toSchema, storeItr, sourceBlocks)
			skipped, results, err := gen.Generate(context.Background())
			require.Nil(t, err)
			require.Equal(t, tc.numSkipped, len(skipped))

			var outputBlocks []*v1.Block
			for results.Next() {
				outputBlocks = append(outputBlocks, results.At())
			}
			require.Equal(t, tc.outputBlocks, len(outputBlocks))

			// Check all the input series are present in the output blocks.
			expectedRefs := v1.PointerSlice(data)
			outputRefs := make([]*v1.SeriesWithBloom, 0, len(data))
			for _, block := range outputBlocks {
				bq := block.Querier()
				for bq.Next() {
					outputRefs = append(outputRefs, bq.At())
				}
			}
			require.Equal(t, len(expectedRefs), len(outputRefs))
			for i := range expectedRefs {
				require.Equal(t, expectedRefs[i].Series, outputRefs[i].Series)
			}
		})
	}
}
