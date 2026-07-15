package store

import (
	"encoding/binary"
	"math"
	"math/bits"

	_ "github.com/asg017/sqlite-vec-go-bindings/ncruces"
	"github.com/ncruces/go-sqlite3"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// The sqlite-vec WASM build uses atomic instructions, which wazero only
// accepts with the threads feature enabled — ncruces' default runtime
// config does not enable it, so we supply our own.
func init() {
	cfg := wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesThreads)
	if bits.UintSize < 64 {
		cfg = cfg.WithMemoryLimitPages(512)
	} else {
		cfg = cfg.WithMemoryLimitPages(4096)
	}
	sqlite3.RuntimeConfig = cfg
}

func serializeF32(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}
